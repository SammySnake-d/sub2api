//go:build unit

package ip

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 2026-09-16 生产现场的真实拓扑（只读探针实测，见 ip.go 包注释）：
//   - caddyPeer 是 Caddy 经 docker MASQUERADE 后打到 6699 的源地址，
//     nsenter 采样里 6699 上 100% 的 established socket peer 都是它。
//   - realClient 是探针机的公网出口 IP；公网直连 6699 的包不过 MASQUERADE，
//     源地址保留，所以外网客户端的 peer 永远不会等于 caddyPeer。
//   - forged* 是探针伪造的头值，修复前它们会原样出现在日志的 client_ip 里。
const (
	caddyPeer    = "172.21.0.1"
	realClient   = "203.10.99.42"
	forgedCFIP   = "198.51.100.21"
	forgedRealIP = "198.51.100.22"
	forgedXFF    = "198.51.100.11"
)

var productionTrustedProxies = []string{caddyPeer + "/32"}

// newForwardedIPRouter 让 gin 和本包用同一份可信代理列表，复刻
// internal/server/http.go configureTrustedProxies + config.ApplyTrustedProxyPolicy。
func newForwardedIPRouter(t *testing.T, trustedProxies []string, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	require.NoError(t, SetTrustedProxies(trustedProxies))
	t.Cleanup(func() { require.NoError(t, SetTrustedProxies(nil)) })

	r := gin.New()
	require.NoError(t, r.SetTrustedProxies(trustedProxies))
	r.GET("/t", handler)
	return r
}

// runResolve 同时校验两个出口：日志/元数据用的 GetClientIP 与安全敏感路径用的
// GetSecurityClientIP 必须给出同一个答案，否则「限流按 A 记账、白名单按 B 放行」的
// 分叉会再次出现。
func runResolve(t *testing.T, trustedProxies []string, snapshot func(c *gin.Context), remoteAddr string, headers map[string]string) string {
	t.Helper()
	var securityIP string
	r := newForwardedIPRouter(t, trustedProxies, func(c *gin.Context) {
		if snapshot != nil {
			snapshot(c)
		}
		securityIP = GetSecurityClientIP(c, true)
		require.Equal(t, securityIP, GetClientIP(c), "GetClientIP 与 GetSecurityClientIP 必须同源")
		c.String(200, securityIP)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/t", nil)
	req.RemoteAddr = remoteAddr
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	return w.Body.String()
}

func legacyTrustSnapshot(c *gin.Context) { SetForwardedIPSettings(c, true, nil) }

// 阳性 1：探针 __probe_direct_forged —— 公网直连 6699 自报 IP。
// 修复前：X-Real-IP / CF-Connecting-IP 被无条件采信，日志写 198.51.100.9。
func TestForgedHeadersFromPublicPeerAreIgnored(t *testing.T) {
	got := runResolve(t, productionTrustedProxies, legacyTrustSnapshot, realClient+":41234", map[string]string{
		"X-Real-IP":        forgedRealIP,
		"CF-Connecting-IP": forgedCFIP,
		"X-Forwarded-For":  forgedXFF,
	})
	require.Equal(t, realClient, got)
}

// 阳性 2：探针 __probe_caddy_cfip / __probe_caddy_realip —— 走反代照样能伪造。
// Caddy 2.10.2 会重写 X-Forwarded-For，但不碰 X-Real-IP / CF-Connecting-IP，
// 而修复前的优先级恰好是 CF-Connecting-IP > X-Real-IP > XFF。
func TestForgedHeadersThroughTrustedProxyAreIgnored(t *testing.T) {
	got := runResolve(t, productionTrustedProxies, legacyTrustSnapshot, caddyPeer+":52000", map[string]string{
		"X-Forwarded-For":  realClient, // Caddy 写入的真实客户端
		"X-Real-IP":        forgedRealIP,
		"CF-Connecting-IP": forgedCFIP,
	})
	require.Equal(t, realClient, got)
}

// 阳性 3：攻击者在 XFF 左侧塞假 IP。gin 从右往左走，遇到第一个非可信 IP 就停，
// 所以左边塞多少都没用；修复前的实现取的是从左数第一个公网地址。
func TestForgedXFFPrefixCannotOutrunTrustedChain(t *testing.T) {
	got := runResolve(t, productionTrustedProxies, legacyTrustSnapshot, caddyPeer+":52001", map[string]string{
		"X-Forwarded-For": "198.51.100.77, " + realClient,
	})
	require.Equal(t, realClient, got)
}

// 阳性 4：fail-closed。没有 SessionBindingContext 注入快照时，修复前
// requestUsesLegacyForwardedIPTrust 返回 true（fail-open），转发头照读。
func TestMissingSnapshotDoesNotTrustForwardedHeaders(t *testing.T) {
	got := runResolve(t, productionTrustedProxies, nil, realClient+":41235", map[string]string{
		"X-Real-IP":        forgedRealIP,
		"CF-Connecting-IP": forgedCFIP,
	})
	require.Equal(t, realClient, got)
}

// 阴性（差分）：反代后的正常请求仍然解析出真实客户端，没有塌进 172.21.0.1 那一个桶。
// 没有这条，「一律返回 peer」的伪修复也能让上面四条阳性变绿。
func TestTrustedProxyChainStillResolvesRealClient(t *testing.T) {
	got := runResolve(t, productionTrustedProxies, legacyTrustSnapshot, caddyPeer+":52002", map[string]string{
		"X-Forwarded-For": realClient,
	})
	require.Equal(t, realClient, got)
	require.NotEqual(t, caddyPeer, got, "所有用户塌进反代地址 = 限流/审计/白名单同时失效")
}

// 阴性（差分）：两个不同的真实客户端经同一个反代来，仍然各自独立记账。
func TestTrustedProxyChainSeparatesClients(t *testing.T) {
	first := runResolve(t, productionTrustedProxies, legacyTrustSnapshot, caddyPeer+":52003", map[string]string{
		"X-Forwarded-For": "198.51.100.1",
	})
	second := runResolve(t, productionTrustedProxies, legacyTrustSnapshot, caddyPeer+":52004", map[string]string{
		"X-Forwarded-For": "198.51.100.2",
	})
	require.Equal(t, "198.51.100.1", first)
	require.Equal(t, "198.51.100.2", second)
}

// 阴性（差分）：无反代的公网直连客户端，取自己的真实地址。
func TestDirectPublicClientKeepsItsOwnAddress(t *testing.T) {
	got := runResolve(t, productionTrustedProxies, legacyTrustSnapshot, realClient+":41236", nil)
	require.Equal(t, realClient, got)
}

// API Key IP 白名单是这条缺陷最直接的受害者：修复前任何人都能用
// X-Real-IP 自报成白名单里的地址。这里同时验证「伪造被拒」与「真客户端仍放行」。
func TestAPIKeyACLCannotBeBypassedByForgedHeader(t *testing.T) {
	whitelist := []string{"203.0.113.7"}

	forged := runResolve(t, productionTrustedProxies, legacyTrustSnapshot, realClient+":41237", map[string]string{
		"X-Real-IP":        "203.0.113.7",
		"CF-Connecting-IP": "203.0.113.7",
	})
	allowed, reason := CheckIPRestriction(forged, whitelist, nil)
	require.False(t, allowed, "伪造头不得穿过 API Key IP 白名单")
	require.Equal(t, "access denied", reason)

	legit := runResolve(t, productionTrustedProxies, legacyTrustSnapshot, caddyPeer+":52005", map[string]string{
		"X-Forwarded-For": "203.0.113.7",
	})
	allowed, _ = CheckIPRestriction(legit, whitelist, nil)
	require.True(t, allowed, "白名单内的真实客户端经反代访问必须仍然放行")
}

// 自定义 CDN 头：能力保留，但只在 peer 可信时才读。
func TestCustomForwardedHeadersRequireTrustedPeer(t *testing.T) {
	snapshot := func(c *gin.Context) { SetForwardedIPSettings(c, true, []string{"X-CDN-IP"}) }

	t.Run("trusted peer honours the configured header", func(t *testing.T) {
		got := runResolve(t, productionTrustedProxies, snapshot, caddyPeer+":52006", map[string]string{
			"X-CDN-IP": "203.0.113.9",
		})
		require.Equal(t, "203.0.113.9", got)
	})

	t.Run("untrusted peer cannot self-report through it", func(t *testing.T) {
		got := runResolve(t, productionTrustedProxies, snapshot, realClient+":41238", map[string]string{
			"X-CDN-IP": "198.51.100.5",
		})
		require.Equal(t, realClient, got)
	})

	t.Run("switch off ignores the header even from a trusted peer", func(t *testing.T) {
		got := runResolve(t, productionTrustedProxies, func(c *gin.Context) {
			SetForwardedIPSettings(c, false, []string{"X-CDN-IP"})
		}, caddyPeer+":52007", map[string]string{
			"X-CDN-IP":        "203.0.113.9",
			"X-Forwarded-For": realClient,
		})
		require.Equal(t, realClient, got)
	})

	t.Run("no trusted proxy configured means no header is read", func(t *testing.T) {
		got := runResolve(t, nil, snapshot, caddyPeer+":52008", map[string]string{
			"X-CDN-IP": "203.0.113.9",
		})
		require.Equal(t, caddyPeer, got)
	})
}

func TestSetTrustedProxiesFailsClosedOnInvalidPattern(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, SetTrustedProxies(nil)) })

	require.NoError(t, SetTrustedProxies([]string{caddyPeer + "/32"}))
	require.True(t, IsTrustedProxyAddr(caddyPeer))

	require.Error(t, SetTrustedProxies([]string{caddyPeer + "/32", "not-an-ip"}))
	require.False(t, IsTrustedProxyAddr(caddyPeer), "一条规则非法就整份作废，与 gin 报错后 http.go 退回 nil 一致")
}

// 差分校验：本包的 peer 判定必须与 gin 的 isTrustedProxy 判一样，否则会出现
// 「gin 不信这个 peer，我们却读它的自定义头」的裂缝。用行为对比，不比源码。
func TestTrustedProxyDecisionMatchesGin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { require.NoError(t, SetTrustedProxies(nil)) })

	for _, tc := range []struct {
		name     string
		patterns []string
		peers    []string
	}{
		{name: "single host", patterns: []string{caddyPeer + "/32"}, peers: []string{caddyPeer, "172.21.0.2", realClient}},
		{name: "bare ip means /32", patterns: []string{caddyPeer}, peers: []string{caddyPeer, "172.21.0.2"}},
		{name: "cidr block", patterns: []string{"172.21.0.0/16"}, peers: []string{caddyPeer, "172.21.9.9", "172.22.0.1"}},
		{name: "ipv6", patterns: []string{"::1/128"}, peers: []string{"::1", "::2"}},
		{name: "empty list", patterns: nil, peers: []string{caddyPeer, realClient}},
		{name: "invalid entry", patterns: []string{"garbage"}, peers: []string{caddyPeer, realClient}},
		// 带空白的模式：gin 不 TrimSpace，所以它解析失败、谁都不信。本包若多 trim 一次
		// 就会照单全收，判这个 peer 可信 —— 那正是「本包永远不比 gin 宽松」这条不变量
		// 被打破的形态，也是 YAML 引号串带尾空格时真会走到的分支。
		{name: "leading whitespace", patterns: []string{" " + caddyPeer + "/32"}, peers: []string{caddyPeer, realClient}},
		{name: "trailing whitespace", patterns: []string{caddyPeer + "/32 "}, peers: []string{caddyPeer, realClient}},
		{name: "whitespace bare ip", patterns: []string{caddyPeer + " "}, peers: []string{caddyPeer, realClient}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			ginErr := r.SetTrustedProxies(tc.patterns)
			ourErr := SetTrustedProxies(tc.patterns)
			require.Equal(t, ginErr != nil, ourErr != nil, "接受面必须一致")
			if ginErr != nil {
				// http.go 在 gin 报错时退回 SetTrustedProxies(nil)：谁都不信。
				require.NoError(t, r.SetTrustedProxies(nil))
			}

			r.GET("/t", func(c *gin.Context) { c.String(200, c.ClientIP()) })
			for _, peer := range tc.peers {
				w := httptest.NewRecorder()
				req := httptest.NewRequest("GET", "/t", nil)
				req.RemoteAddr = joinHostPort(peer, "12345")
				req.Header.Set("X-Forwarded-For", "1.2.3.4")
				r.ServeHTTP(w, req)
				ginTrusted := w.Body.String() == "1.2.3.4"
				require.Equal(t, ginTrusted, IsTrustedProxyAddr(peer), "peer %s 的判定与 gin 不一致", peer)
			}
		})
	}
}

func joinHostPort(host, port string) string {
	if len(host) > 0 && (host[0] == ':' || countColons(host) > 1) {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

func countColons(s string) int {
	n := 0
	for _, r := range s {
		if r == ':' {
			n++
		}
	}
	return n
}
