package httpclient

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// 控制面出站 HTTP 客户端。
//
// 「控制面出站」指与 AI 账号无关的自运维流量：定价数据/哈希同步、GitHub Release
// 检查、Codex 客户端版本跟随。它和网关转发 AI 请求是两类完全不同的流量，之前却
// 在 pricing_service.go 与 github_release_service.go 里各写了一份几乎逐行相同的
// 客户端构造 + 失败兜底。同一个决策有两个落点必然漂，而且已经漂了：两处的
// 「回退直连」分支一条日志都不打，代理初始化失败后静默直连，运维只看得到「没走
// 代理」的结果、看不到原因。这里把这个决策收成唯一落点。
//
// 为什么控制面需要能配代理：生产部署在腾讯云国内 VPS，上面这些目标全在境外，
// 直连反复超时，不是偶发抖动（生产日志实录）：
//
//	error [Pricing] Failed to fetch remote hash: Get "https://ghfast.top/.../model_prices_and_context_window.sha256": net/http: TLS handshake timeout
//	warn  openai_codex_version_sync_fetch_failed error=Get "https://api.github.com/repos/openai/codex/releases?per_page=30": dial tcp 20.205.243.168:443: i/o timeout
//
// 根因不是网络不通，是这些客户端没复用同一套部署里已经可用的海外出口：resin
// （45.205.28.160:2260，HTTP CONNECT，AI 账号流量已经在走它，实测经它到境外
// p50 185ms、到 api 域名 2.2s）。控制面之前没复用，只是因为压根没有地方填。
// 把 control_plane.proxy_url 指向 resin 就够了，不需要再引入新的出网设施。

const defaultControlPlaneTimeout = 30 * time.Second

// ErrControlPlaneProxyInit 控制面代理初始化失败且不允许回退直连。
// 用 sentinel 而不是包装原始错误：原始错误链会携带明文代理凭据，见 scrubProxyError。
var ErrControlPlaneProxyInit = errors.New("control plane proxy client init failed")

// ControlPlaneOptions 描述一个控制面出站客户端。
type ControlPlaneOptions struct {
	// Service 日志里的服务标识（如 "pricing" / "github_release"），用于区分是哪条
	// 控制面链路在兜底。
	Service string
	// ProxyURL 代理地址，支持 http/https/socks5/socks5h。
	// 留空 = 直连（兼容底线：没配代理的部署行为完全不变）。
	// 注意：这个值会带凭据，禁止直接进日志，必须过 RedactProxyURL。
	ProxyURL string
	// Timeout 请求总超时，<=0 时取 defaultControlPlaneTimeout。
	Timeout time.Duration
	// AllowDirectOnProxyError 代理不可用时是否回退直连。
	// 由 security.proxy_fallback.allow_direct_on_error 提供，默认 false（fail closed），
	// 因为无声回退会把服务器真实 IP 暴露给上游。开启时回退一定伴随 warn 日志，
	// 不静默吞错误。
	AllowDirectOnProxyError bool
}

// NewControlPlaneClient 构造控制面出站客户端。
//
// 语义（三种结果，调用方只需区分 err 是否为 nil）：
//   - ProxyURL 留空 → 直连客户端，nil 错误。这是兼容底线，不打任何日志。
//   - 代理可用 → 走代理的客户端。若 AllowDirectOnProxyError 为真，还会挂上运行时
//     兜底（代理连不上时改走直连并 warn）。
//   - 代理不可用且不允许回退 → (nil, ErrControlPlaneProxyInit 包装的错误)，调用方
//     自行降级成「所有请求都失败」的占位实现。返回的错误已脱敏，可以安全进日志。
func NewControlPlaneClient(opts ControlPlaneOptions) (*http.Client, error) {
	proxyURL := strings.TrimSpace(opts.ProxyURL)
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultControlPlaneTimeout
	}

	client, err := GetClient(Options{
		Timeout:  timeout,
		ProxyURL: proxyURL,
	})
	if err != nil {
		// 直连配置本身出错（不带代理时 GetClient 几乎不会失败）与代理配置出错分开处理：
		// 前者没有「回退直连」可言，直接上报。
		if proxyURL == "" {
			return nil, fmt.Errorf("%w: %s", ErrControlPlaneProxyInit, scrubProxyError(err, proxyURL))
		}
		if !opts.AllowDirectOnProxyError {
			slog.Warn("control_plane_proxy_init_failed_fail_closed",
				"service", opts.Service,
				"proxy", RedactProxyURL(proxyURL),
				"error", scrubProxyError(err, proxyURL),
			)
			return nil, fmt.Errorf("%w: %s; set security.proxy_fallback.allow_direct_on_error=true to allow fallback",
				ErrControlPlaneProxyInit, scrubProxyError(err, proxyURL))
		}
		// 显式允许回退：必须留痕。历史实现在这里什么都不打，是本次修掉的静默点。
		slog.Warn("control_plane_proxy_init_failed_direct_fallback",
			"service", opts.Service,
			"proxy", RedactProxyURL(proxyURL),
			"error", scrubProxyError(err, proxyURL),
		)
		direct, directErr := GetClient(Options{Timeout: timeout})
		if directErr != nil {
			return nil, fmt.Errorf("%w: %s", ErrControlPlaneProxyInit, scrubProxyError(directErr, ""))
		}
		return direct, nil
	}

	if proxyURL == "" || !opts.AllowDirectOnProxyError {
		return client, nil
	}

	direct, err := GetClient(Options{Timeout: timeout})
	if err != nil {
		// 代理本身可用，只是拿不到兜底客户端；保留主路径，不因为兜底失败而整体失败。
		return client, nil
	}
	return withDirectFallback(client, direct, opts.Service, proxyURL), nil
}

// withDirectFallback 给走代理的客户端挂上运行时兜底。
//
// GetClient 返回的是全局缓存的共享实例，必须浅拷贝一份再换 Transport，
// 否则会把兜底行为塞给所有复用同一配置的调用方。
func withDirectFallback(proxied, direct *http.Client, service, proxyURL string) *http.Client {
	cloned := *proxied
	cloned.Transport = &directFallbackTransport{
		proxied:  transportOf(proxied),
		direct:   transportOf(direct),
		service:  service,
		redacted: RedactProxyURL(proxyURL),
	}
	return &cloned
}

func transportOf(client *http.Client) http.RoundTripper {
	if client == nil || client.Transport == nil {
		return http.DefaultTransport
	}
	return client.Transport
}

// directFallbackTransport 代理请求失败时改走直连。
//
// 只在 AllowDirectOnProxyError 显式开启时才会挂上：默认不挂，因为回退会把服务器
// 真实 IP 暴露给上游。
type directFallbackTransport struct {
	proxied  http.RoundTripper
	direct   http.RoundTripper
	service  string
	redacted string
}

func (t *directFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.proxied.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if !canReplay(req) {
		return resp, err
	}
	// 调用方自己取消/超时了就别重放：那不是代理的问题，重放只会拖长本就已经放弃的等待。
	if ctx := req.Context(); ctx != nil && ctx.Err() != nil {
		return resp, err
	}
	slog.Warn("control_plane_proxy_request_failed_direct_fallback",
		"service", t.service,
		"proxy", t.redacted,
		"target", req.URL.Host,
		"error", scrubProxyError(err, ""),
	)
	return t.direct.RoundTrip(req)
}

// canReplay 判断这个请求能否原样重发。
//
// 控制面出站全是无副作用的 GET/HEAD 且 Body 为 nil，所以这个门在生产里总是放行；
// 写成显式判断是为了防止以后有人把带 Body 的请求接进来 —— Body 是一次性
// io.Reader，代理侧已经读过就重放不出来，会静默发出截断的请求体。
func canReplay(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if req.Body != nil && req.GetBody == nil {
		return false
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, "":
		return true
	default:
		return false
	}
}

// RedactProxyURL 把代理地址压成只剩 scheme://host:port，供日志使用。
//
// 刻意不走 url.Parse：这个函数的输入常常正是「parse 失败的那个字符串」，一旦解析
// 失败就没有 host 可取，最容易的写法（原样打印）恰好是最糟的。纯字符串切割对任何
// 输入都不会把 userinfo 漏出去。
// 空输入返回空串（表示直连），无法识别出 host 时返回固定占位而非原文。
func RedactProxyURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	scheme := ""
	rest := trimmed
	if idx := strings.Index(rest, "://"); idx >= 0 {
		scheme = rest[:idx]
		rest = rest[idx+3:]
	}
	// 先切掉 path/query/fragment，避免把 "@" 判断范围扩到 authority 之外。
	if idx := strings.IndexAny(rest, "/?#"); idx >= 0 {
		rest = rest[:idx]
	}
	// authority 里最后一个 "@" 之前全是 userinfo（密码本身可能含 "@"，故取 LastIndex）。
	if idx := strings.LastIndex(rest, "@"); idx >= 0 {
		rest = rest[idx+1:]
	}
	if strings.TrimSpace(rest) == "" {
		return "(redacted)"
	}
	if scheme == "" {
		return rest
	}
	return scheme + "://" + rest
}

// proxyCredentialsPattern 匹配任意 "scheme://userinfo@" 片段中的 userinfo。
var proxyCredentialsPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s"']*@`)

// scrubProxyError 把错误消息里的代理凭据擦掉，返回可以安全落日志/外传的字符串。
//
// 为什么非做不可：proxyurl.Parse 在 url.Parse 失败时用 %v 而不是 %w，注释说这样
// 「避免底层错误消息泄漏原始 URL」—— 这个判断是错的。url.Parse 返回 *url.Error，
// 它的 Error() 本身就是 `parse "<完整原始URL>": <原因>`，%v 打出来一字不少。实测：
//
//	url.Parse("http://user:s3cr3tPass@45.205.28.160:2260\x7f")
//	→ parse "http://user:s3cr3tPass@45.205.28.160:2260\x7f": net/url: invalid control character in URL
//
// 另一条分支用 parsed.Redacted()，Go 只把密码换成 xxxxx，用户名照样带出来。
// 结论：上游错误链不可信，脱敏必须在出口这一侧做，而且要做两层 —— 已知原文做精确
// 替换，未知形态交给正则兜底（这层同时覆盖 Redacted() 留下的用户名）。
func scrubProxyError(err error, rawProxyURL string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if raw := strings.TrimSpace(rawProxyURL); raw != "" {
		msg = strings.ReplaceAll(msg, raw, RedactProxyURL(raw))
	}
	return proxyCredentialsPattern.ReplaceAllString(msg, "${1}")
}
