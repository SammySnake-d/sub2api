// Package ip 提供客户端 IP 地址提取工具。
//
// 唯一的信任判据是「直连 peer 是否落在 server.trusted_proxies 里」。转发头
// （X-Forwarded-For / X-Real-IP / CF-Connecting-IP / 自定义 CDN 头）一律不再被
// 无条件采信。
//
// 为什么这么写 —— 2026-09-16 生产事故：
// 库里 api_key_acl_trust_forwarded_ip=true（updated_at 17:35:32，是那次整批
// settings 导入带进来的；容器内 /app/data/config.yaml 写的是 false，但 DB 值优先
// 且启动零提示），于是本文件的 legacy 分支接管解析，对*任何*能连上监听口的客户端
// 直接采信它自报的头。生产只读探针（404）实测：
//   - 直连 0.0.0.0:6699（公网裸奔，iptables DOCKER-USER 对 6699 一条规则都没有），
//     带 X-Real-IP: 198.51.100.9 → 日志 client_ip=198.51.100.9（真实 peer 是
//     203.10.99.42）。
//   - 经 Caddy 带 CF-Connecting-IP: 198.51.100.21 → 日志 client_ip=198.51.100.21。
//     Caddy 2.10.2 只重写 X-Forwarded-For，不碰 X-Real-IP / CF-Connecting-IP，而
//     legacy 优先级恰好是 CF-Connecting-IP > X-Real-IP > XFF —— 所以「前面有反代」
//     并不能挡住伪造。
//
// 影响面：限流(internal/middleware/rate_limiter.go:117)、审计与会话绑定
// (internal/server/middleware/session_binding.go)、API Key IP 白名单
// (internal/server/middleware/api_key_auth.go:135) 共用这一套解析，全部可被调用方
// 自报 IP 绕过。
//
// 缺陷本体是「按一个全局布尔决定信不信转发头」：它与 peer 无关，要么信所有人的头，
// 要么谁的头都不信。这里改成按 peer 判定 —— gin 的 trusted_proxies 链（从右往左
// 走 XFF，遇到第一个非可信 IP 就停）是权威；自定义 CDN 头只有在 peer 本身可信时
// 才读。
package ip

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

const forwardedIPSettingsKey = "sub2api.forwarded_ip_settings"

type forwardedIPSettings struct {
	trustForwarded bool
	headers        []string
}

// SetForwardedIPSettings snapshots the forwarded-IP mode and custom header list
// for this request.
func SetForwardedIPSettings(c *gin.Context, enabled bool, headers []string) {
	if c == nil {
		return
	}
	c.Set(forwardedIPSettingsKey, forwardedIPSettings{
		trustForwarded: enabled,
		headers:        append([]string(nil), headers...),
	})
}

func requestForwardedIPSettings(c *gin.Context) (forwardedIPSettings, bool) {
	if c == nil {
		return forwardedIPSettings{}, false
	}
	value, ok := c.Get(forwardedIPSettingsKey)
	if !ok {
		return forwardedIPSettings{}, false
	}
	settings, ok := value.(forwardedIPSettings)
	return settings, ok
}

// trustedProxyMatcher 是 server.trusted_proxies 的编译结果。空实例 = 谁都不信。
type trustedProxyMatcher struct {
	cidrs []*net.IPNet
}

// trustedProxyPolicy 是进程级的可信代理快照，由 config 在加载/校验配置时推入
// （config.Config.ApplyTrustedProxyPolicy）。未推入 = 谁都不信（fail-closed）：
// 没有这一条，任何新挂在 SessionBindingContext 之前的中间件、或忘了走 config 的
// 调用方，都会退回「无条件信头」的老毛病。
var trustedProxyPolicy atomic.Pointer[trustedProxyMatcher]

// SetTrustedProxies 记录本进程的可信代理列表，必须与交给 gin 的那一份同源
// （internal/server/http.go configureTrustedProxies：未显式配置时传 nil，
// SetTrustedProxies 失败时退回 nil）。
//
// 任何一条非法规则都会让整份列表作废（与 gin 一致：gin 的 SetTrustedProxies 返回
// error 后，http.go 会 SetTrustedProxies(nil)），保证这里永远不会比 gin 更宽松。
func SetTrustedProxies(patterns []string) error {
	matcher, err := compileTrustedProxies(patterns)
	if err != nil {
		trustedProxyPolicy.Store(&trustedProxyMatcher{})
		return err
	}
	trustedProxyPolicy.Store(matcher)
	return nil
}

// compileTrustedProxies 复刻 gin v1.9.1 gin.go:390 prepareTrustedCIDRs 的接受面：
// 裸 IP 按 /32（IPv4）或 /128（IPv6）处理，其余按 CIDR 解析。
func compileTrustedProxies(patterns []string) (*trustedProxyMatcher, error) {
	matcher := &trustedProxyMatcher{cidrs: make([]*net.IPNet, 0, len(patterns))}
	for _, pattern := range patterns {
		// 刻意**不** TrimSpace：gin v1.9.1 prepareTrustedCIDRs 也不 trim。
		// 多 trim 一次会让本包比 gin 宽 —— " 10.0.0.0/8" 这种带空白的模式在 gin 侧
		// 解析失败 → configureTrustedProxies 退回 SetTrustedProxies(nil) → gin 谁都不信，
		// 而本包若照单全收就会认为该 peer 可信，两半对「谁是代理」的判定出现裂缝。
		// 本包的不变量是「永远不比 gin 更宽松」，对齐接受面是它的前提。
		trimmed := pattern
		if !strings.Contains(trimmed, "/") {
			parsed := net.ParseIP(trimmed)
			if parsed == nil {
				return nil, fmt.Errorf("invalid trusted proxy %q", pattern)
			}
			if parsed.To4() != nil {
				trimmed += "/32"
			} else {
				trimmed += "/128"
			}
		}
		_, cidr, err := net.ParseCIDR(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: %w", pattern, err)
		}
		matcher.cidrs = append(matcher.cidrs, cidr)
	}
	return matcher, nil
}

// IsTrustedProxyAddr 报告某个地址是否是已配置的可信代理。
func IsTrustedProxyAddr(addr string) bool {
	matcher := trustedProxyPolicy.Load()
	if matcher == nil || len(matcher.cidrs) == 0 {
		return false
	}
	parsed := net.ParseIP(normalizeIP(addr))
	if parsed == nil {
		return false
	}
	for _, cidr := range matcher.cidrs {
		if cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

// PeerIsTrustedProxy 报告本请求的直连 peer 是否可信。注意判据是 peer（RemoteAddr），
// 不是任何请求头 —— 公网直连的包不经过 docker 的 MASQUERADE，源地址保留，永远不会
// 等于反代落点（生产是 172.21.0.1）。
func PeerIsTrustedProxy(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	return IsTrustedProxyAddr(c.RemoteIP())
}

// GetClientIP 解析请求的客户端地址，用于请求元数据与用量/错误日志。
// 现在与 GetSecurityClientIP 同一套解析：以 gin 的可信代理链为权威。
func GetClientIP(c *gin.Context) string {
	return resolveClientIP(c)
}

// resolveClientIP 是本包唯一的解析实现。
//
// 顺序：
//  1. peer 可信 + 本次请求快照开启转发信任 + 配置了自定义 CDN 头 → 读自定义头；
//  2. 否则/或自定义头没给出公网地址 → gin 的可信代理链（peer 不可信时就是 peer 本身）；
//  3. gin 只给出私网地址（典型：反代落点）时，才退到自定义头里的私网候选值。
//
// 第 3 步保留的是老实现里「公网优先于私网」那条经验：反代把自己的网桥地址写进头里
// 是常见配置事故，不能因此把所有用户塌进同一个桶。
func resolveClientIP(c *gin.Context) string {
	if c == nil {
		return ""
	}
	trusted := normalizeIP(c.ClientIP())

	// PeerIsTrustedProxy 这道闸是缺陷本体的收口点，不能省：
	// 老实现把「读不读转发头」挂在 security.trust_forwarded_ip_for_api_key_acl 这个
	// peer 无关的全局布尔上 —— 开了就信所有人的头。2026-09-16 生产上它因迁移导入被
	// 置成 true，于是公网直连 6699 的探针带 X-Real-IP: 198.51.100.9 就让日志写下
	// client_ip=198.51.100.9。把自定义 CDN 头的消费搬到 peer 判定之后，这个开关退化
	// 成「可信代理送来的头要不要读」，任何不可信 peer 的头都不再有出路。
	//
	// 若哪天把这个 if 拿掉：只要运营在 forwarded_client_ip_headers 里填上
	// X-Real-IP / CF-Connecting-IP（生产当时是 []，但它是后台可改的），伪造面立刻整条
	// 回来 —— 且这次连 gin 的可信链都绕过了。
	var customIP, customFallback string
	if PeerIsTrustedProxy(c) {
		if settings, ok := requestForwardedIPSettings(c); ok && settings.trustForwarded &&
			len(settings.headers) > 0 {
			customIP, customFallback = resolveCustomForwardedClientIP(c, settings.headers)
		}
	}
	if customIP != "" {
		return customIP
	}
	if trusted != "" && !isPrivateIP(trusted) {
		return trusted
	}
	if customFallback != "" {
		return customFallback
	}
	return trusted
}

func resolveCustomForwardedClientIP(c *gin.Context, headers []string) (string, string) {
	if c == nil {
		return "", ""
	}
	var fallback string
	for _, header := range headers {
		for _, value := range c.Request.Header.Values(header) {
			for _, candidate := range strings.Split(value, ",") {
				parsed := net.ParseIP(strings.TrimSpace(candidate))
				if parsed == nil {
					continue
				}
				normalized := parsed.String()
				if isPrivateIP(normalized) {
					if fallback == "" {
						fallback = normalized
					}
					continue
				}
				return normalized, fallback
			}
		}
	}
	return "", fallback
}

// GetTrustedClientIP 从 Gin 的可信代理解析链提取客户端 IP。
// 该方法依赖 gin.Engine.SetTrustedProxies 配置，不读任何原始转发头。
func GetTrustedClientIP(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return normalizeIP(c.ClientIP())
}

// GetSecurityClientIP 返回安全敏感路径（API Key IP 白名单、限流、审计、会话绑定）
// 使用的客户端 IP。
//
// 第二个参数是老开关 security.trust_forwarded_ip_for_api_key_acl 的取值，现在不再
// 参与判定：开关是 peer 无关的，正是 2026-09-16 那条「任何客户端都能自报 IP」的
// 缺陷本体。信任只由 peer 决定（见 resolveClientIP）。参数保留是因为调用点
// （api_key_auth.go:135、api_key_auth_google.go:94、rate_limiter.go:117）不在本次
// 改动范围内；清理调用点后应连同参数一起删除。
func GetSecurityClientIP(c *gin.Context, _ bool) string {
	return resolveClientIP(c)
}

// normalizeIP 规范化 IP 地址，去除端口号和空格。
func normalizeIP(ip string) string {
	ip = strings.TrimSpace(ip)
	// 移除端口号（如 "192.168.1.1:8080" -> "192.168.1.1"）
	if host, _, err := net.SplitHostPort(ip); err == nil {
		return host
	}
	return ip
}

// privateNets contains the private/loopback ranges skipped while selecting a
// public address from a legacy X-Forwarded-For chain.
var privateNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"::1/128",
		"fc00::/7",
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("invalid CIDR: " + cidr)
		}
		privateNets = append(privateNets, block)
	}
}

// CompiledIPRules 表示预编译的 IP 匹配规则。
// PatternCount 记录原始规则数量，用于保留“规则存在但全无效”时的行为语义。
type CompiledIPRules struct {
	CIDRs        []*net.IPNet
	IPs          []net.IP
	PatternCount int
}

// CompileIPRules 将 IP/CIDR 字符串规则预编译为可复用结构。
// 非法规则会被忽略，但 PatternCount 会保留原始规则条数。
func CompileIPRules(patterns []string) *CompiledIPRules {
	compiled := &CompiledIPRules{
		CIDRs:        make([]*net.IPNet, 0, len(patterns)),
		IPs:          make([]net.IP, 0, len(patterns)),
		PatternCount: len(patterns),
	}
	for _, pattern := range patterns {
		normalized := strings.TrimSpace(pattern)
		if normalized == "" {
			continue
		}
		if strings.Contains(normalized, "/") {
			_, cidr, err := net.ParseCIDR(normalized)
			if err != nil || cidr == nil {
				continue
			}
			compiled.CIDRs = append(compiled.CIDRs, cidr)
			continue
		}
		parsedIP := net.ParseIP(normalized)
		if parsedIP == nil {
			continue
		}
		compiled.IPs = append(compiled.IPs, parsedIP)
	}
	return compiled
}

func matchesCompiledRules(parsedIP net.IP, rules *CompiledIPRules) bool {
	if parsedIP == nil || rules == nil {
		return false
	}
	for _, cidr := range rules.CIDRs {
		if cidr.Contains(parsedIP) {
			return true
		}
	}
	for _, ruleIP := range rules.IPs {
		if parsedIP.Equal(ruleIP) {
			return true
		}
	}
	return false
}

func isPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, block := range privateNets {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// MatchesPattern 检查 IP 是否匹配指定的模式（支持单个 IP 或 CIDR）。
// pattern 可以是：
// - 单个 IP: "192.168.1.100"
// - CIDR 范围: "192.168.1.0/24"
func MatchesPattern(clientIP, pattern string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}

	// 尝试解析为 CIDR
	if strings.Contains(pattern, "/") {
		_, cidr, err := net.ParseCIDR(pattern)
		if err != nil {
			return false
		}
		return cidr.Contains(ip)
	}

	// 作为单个 IP 处理
	patternIP := net.ParseIP(pattern)
	if patternIP == nil {
		return false
	}
	return ip.Equal(patternIP)
}

// MatchesAnyPattern 检查 IP 是否匹配任意一个模式。
func MatchesAnyPattern(clientIP string, patterns []string) bool {
	for _, pattern := range patterns {
		if MatchesPattern(clientIP, pattern) {
			return true
		}
	}
	return false
}

// CheckIPRestriction 检查 IP 是否被 API Key 的 IP 限制允许。
// 返回值：(是否允许, 拒绝原因)
// 逻辑：
// 1. 先检查黑名单，如果在黑名单中则直接拒绝
// 2. 如果白名单不为空，IP 必须在白名单中
// 3. 如果白名单为空，允许访问（除非被黑名单拒绝）
func CheckIPRestriction(clientIP string, whitelist, blacklist []string) (bool, string) {
	return CheckIPRestrictionWithCompiledRules(
		clientIP,
		CompileIPRules(whitelist),
		CompileIPRules(blacklist),
	)
}

// CheckIPRestrictionWithCompiledRules 使用预编译规则检查 IP 是否允许访问。
func CheckIPRestrictionWithCompiledRules(clientIP string, whitelist, blacklist *CompiledIPRules) (bool, string) {
	// 规范化 IP
	clientIP = normalizeIP(clientIP)
	if clientIP == "" {
		return false, "access denied"
	}
	parsedIP := net.ParseIP(clientIP)
	if parsedIP == nil {
		return false, "access denied"
	}

	// 1. 检查黑名单
	if blacklist != nil && blacklist.PatternCount > 0 && matchesCompiledRules(parsedIP, blacklist) {
		return false, "access denied"
	}

	// 2. 检查白名单（如果设置了白名单，IP 必须在其中）
	if whitelist != nil && whitelist.PatternCount > 0 && !matchesCompiledRules(parsedIP, whitelist) {
		return false, "access denied"
	}

	return true, ""
}

// ValidateIPPattern 验证 IP 或 CIDR 格式是否有效。
func ValidateIPPattern(pattern string) bool {
	if strings.Contains(pattern, "/") {
		_, _, err := net.ParseCIDR(pattern)
		return err == nil
	}
	return net.ParseIP(pattern) != nil
}

// ValidateIPPatterns 验证多个 IP 或 CIDR 格式。
// 返回无效的模式列表。
func ValidateIPPatterns(patterns []string) []string {
	var invalid []string
	for _, p := range patterns {
		if !ValidateIPPattern(p) {
			invalid = append(invalid, p)
		}
	}
	return invalid
}
