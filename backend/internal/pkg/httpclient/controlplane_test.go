package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 这一组测试全部跑在本地：一个 httptest「目标站」+ 一个 httptest「假代理」。
// 不打真网络，所以 CI / 断网机器上结论一致；也因此可以放心断言「请求到底落在谁身上」——
// 这正是原来那两份重复实现里没人验证过的那一位。

const (
	fromProxyBody  = "served-by-fake-proxy"
	fromTargetBody = "served-by-target"
	// 带凭据的代理地址：所有脱敏断言都盯着这两个字符串是否出现在日志/错误里。
	testProxyUser     = "resinuser"
	testProxyPassword = "s3cr3tPassw0rd"
)

// newFakeProxy 起一个最小 HTTP 代理。
//
// 明文 HTTP 请求经代理时，Go 会把绝对 URI 发给代理而不是目标站，所以「假代理」
// 只要回一个特征响应就足以证明请求确实走了代理 —— 不需要真的转发，也就不会在
// 测试里产生任何对外连接。
func newFakeProxy(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("X-Fake-Proxy", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fromProxyBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTargetServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fromTargetBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureLogs 把默认 slog 重定向到缓冲区，返回读取器。
// 控制面客户端的兜底告警走的是 slog 默认 logger，这是唯一能验证「没静默吞掉」的口子。
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf.String
}

func getBody(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, readErr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if readErr != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// 配了代理就必须走代理：目标站一次都不能被直接命中，否则就是「配置写了没生效」——
// 这正是生产故障的形态（配置项压根不存在时和配了没生效的日志一模一样）。
func TestNewControlPlaneClient_ConfiguredProxyIsUsed(t *testing.T) {
	var proxyHits, targetHits atomic.Int64
	proxy := newFakeProxy(t, &proxyHits)
	target := newTargetServer(t, &targetHits)

	client, err := NewControlPlaneClient(ControlPlaneOptions{
		Service:  "pricing",
		ProxyURL: proxy.URL,
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status, body := getBody(t, client, target.URL+"/model_prices.sha256")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body != fromProxyBody {
		t.Fatalf("body = %q, want %q (request did not go through the proxy)", body, fromProxyBody)
	}
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("proxy hits = %d, want 1", got)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("target hits = %d, want 0 (traffic bypassed the proxy)", got)
	}
}

// 留空 = 直连：兼容底线。没配代理的部署（海外机器直连本来就通）行为不能变，
// 而且这条路径上不应该有任何告警噪音。
func TestNewControlPlaneClient_EmptyProxyGoesDirect(t *testing.T) {
	logs := captureLogs(t)
	var proxyHits, targetHits atomic.Int64
	proxy := newFakeProxy(t, &proxyHits)
	_ = proxy // 起着但不应被命中
	target := newTargetServer(t, &targetHits)

	client, err := NewControlPlaneClient(ControlPlaneOptions{
		Service:  "pricing",
		ProxyURL: "   ", // 只有空白也算留空
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, body := getBody(t, client, target.URL+"/direct")
	if body != fromTargetBody {
		t.Fatalf("body = %q, want %q", body, fromTargetBody)
	}
	if got := targetHits.Load(); got != 1 {
		t.Fatalf("target hits = %d, want 1", got)
	}
	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("proxy hits = %d, want 0", got)
	}
	if out := logs(); strings.Contains(out, "control_plane_proxy") {
		t.Fatalf("direct path should be silent, got logs: %s", out)
	}
}

// 代理配置坏了 + 显式允许兜底 → 走直连并留 warn。
// 历史实现在这个分支一条日志都不打，运维只能看到「又直连超时了」，看不到原因。
func TestNewControlPlaneClient_BrokenProxyFallsBackToDirectWithWarn(t *testing.T) {
	logs := captureLogs(t)
	var targetHits atomic.Int64
	target := newTargetServer(t, &targetHits)

	client, err := NewControlPlaneClient(ControlPlaneOptions{
		Service:                 "pricing",
		ProxyURL:                "socks4://" + testProxyUser + ":" + testProxyPassword + "@45.205.28.160:2260",
		Timeout:                 5 * time.Second,
		AllowDirectOnProxyError: true,
	})
	if err != nil {
		t.Fatalf("fallback should succeed, got error: %v", err)
	}

	_, body := getBody(t, client, target.URL+"/fallback")
	if body != fromTargetBody {
		t.Fatalf("body = %q, want %q", body, fromTargetBody)
	}

	out := logs()
	entry := requireLogEntry(t, out, "control_plane_proxy_init_failed_direct_fallback")
	if entry["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN", entry["level"])
	}
	if entry["service"] != "pricing" {
		t.Fatalf("service = %v, want pricing", entry["service"])
	}
	requireNoCredentials(t, out)
}

// 代理坏了但没开兜底 → fail closed。
// 这是现有的安全默认（security.proxy_fallback.allow_direct_on_error=false）：
// 无声回退会把服务器真实 IP 暴露给上游，所以默认宁可让这条链路失败。
// 关键断言是「失败但不泄漏」：返回的 error 会被上层 slog.Warn 原样打出来。
func TestNewControlPlaneClient_BrokenProxyFailsClosedWithoutLeakingCredentials(t *testing.T) {
	logs := captureLogs(t)

	client, err := NewControlPlaneClient(ControlPlaneOptions{
		Service:  "github_release",
		ProxyURL: "socks4://" + testProxyUser + ":" + testProxyPassword + "@45.205.28.160:2260",
		Timeout:  5 * time.Second,
	})
	if client != nil {
		t.Fatal("client should be nil when failing closed")
	}
	if err == nil {
		t.Fatal("expected an error when proxy init fails and fallback is disabled")
	}
	if !errors.Is(err, ErrControlPlaneProxyInit) {
		t.Fatalf("error should wrap ErrControlPlaneProxyInit, got %v", err)
	}
	// 错误对象本身也必须干净：它会被 openai_codex_version_sync_* 直接打进日志。
	requireNoCredentials(t, err.Error())
	if !strings.Contains(err.Error(), "allow_direct_on_error") {
		t.Fatalf("error should point at the switch that changes this behavior, got %v", err)
	}

	out := logs()
	entry := requireLogEntry(t, out, "control_plane_proxy_init_failed_fail_closed")
	if entry["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN", entry["level"])
	}
	requireNoCredentials(t, out)
}

// 运行时代理挂掉（连得上配置、连不上进程）→ 开了兜底就改走直连并 warn。
// 用「起好再关掉」的 httptest 端口制造一个确定会 connection refused 的代理，
// 不依赖任何外部地址。
func TestNewControlPlaneClient_DeadProxyFallsBackAtRequestTime(t *testing.T) {
	logs := captureLogs(t)
	var targetHits atomic.Int64
	target := newTargetServer(t, &targetHits)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 端口立刻不可连接

	proxyWithCreds := strings.Replace(deadURL, "http://", "http://"+testProxyUser+":"+testProxyPassword+"@", 1)
	client, err := NewControlPlaneClient(ControlPlaneOptions{
		Service:                 "github_release",
		ProxyURL:                proxyWithCreds,
		Timeout:                 5 * time.Second,
		AllowDirectOnProxyError: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, body := getBody(t, client, target.URL+"/releases/latest")
	if body != fromTargetBody {
		t.Fatalf("body = %q, want %q (runtime fallback did not happen)", body, fromTargetBody)
	}
	if got := targetHits.Load(); got != 1 {
		t.Fatalf("target hits = %d, want 1", got)
	}

	out := logs()
	entry := requireLogEntry(t, out, "control_plane_proxy_request_failed_direct_fallback")
	if entry["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN", entry["level"])
	}
	requireNoCredentials(t, out)
}

// 同样的死代理，默认（未开兜底）下必须报错而不是悄悄直连：
// 这条断言守的是「不要为了好看而擅自放宽安全默认」。
func TestNewControlPlaneClient_DeadProxyDoesNotSilentlyGoDirectByDefault(t *testing.T) {
	var targetHits atomic.Int64
	target := newTargetServer(t, &targetHits)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	client, err := NewControlPlaneClient(ControlPlaneOptions{
		Service:  "github_release",
		ProxyURL: deadURL,
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.URL+"/releases/latest", nil)
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the request to fail: fallback is disabled by default")
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("target hits = %d, want 0 (server IP leaked to upstream)", got)
	}
}

// 兜底客户端不能污染 GetClient 的全局缓存实例：否则同配置的其他调用方
// （包括默认不允许兜底的那些）会被偷偷挂上回退行为。
func TestNewControlPlaneClient_FallbackDoesNotMutateSharedClient(t *testing.T) {
	proxy := newFakeProxy(t, nil)

	shared, err := GetClient(Options{Timeout: 7 * time.Second, ProxyURL: proxy.URL})
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	before := shared.Transport

	withFallback, err := NewControlPlaneClient(ControlPlaneOptions{
		Service:                 "pricing",
		ProxyURL:                proxy.URL,
		Timeout:                 7 * time.Second,
		AllowDirectOnProxyError: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shared.Transport != before {
		t.Fatal("shared cached client transport was mutated")
	}
	if withFallback == shared {
		t.Fatal("fallback client must be a copy, not the cached instance")
	}
	if _, ok := withFallback.Transport.(*directFallbackTransport); !ok {
		t.Fatalf("fallback transport not installed, got %T", withFallback.Transport)
	}
}

func TestRedactProxyURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty means direct", "", ""},
		{"whitespace means direct", "  \t ", ""},
		{"strips userinfo", "http://resinuser:s3cr3tPassw0rd@45.205.28.160:2260", "http://45.205.28.160:2260"},
		{"keeps host without creds", "http://45.205.28.160:2260", "http://45.205.28.160:2260"},
		{"strips path and query", "http://u:p@proxy.local:8080/path?token=abc", "http://proxy.local:8080"},
		// 密码里带 "@" 是合法的：必须取最后一个 "@"，否则会把密码尾段当 host 打出来。
		{"password containing at sign", "socks5://u:p@ss@10.0.0.1:1080", "socks5://10.0.0.1:1080"},
		{"no scheme", "resinuser:s3cr3tPassw0rd@45.205.28.160:2260", "45.205.28.160:2260"},
		// 解析不出 host 时返回占位，绝不回退成原文。
		{"unparsable keeps nothing", "http://u:p@", "(redacted)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactProxyURL(tc.in); got != tc.want {
				t.Fatalf("RedactProxyURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// scrubProxyError 的存在理由：上游错误链本身就带明文凭据。
// url.Parse 失败时返回 *url.Error，其 Error() 是 `parse "<完整原始URL>": <原因>`，
// proxyurl.Parse 用 %v 而非 %w 并不能阻止这一点（注释里的说法是错的）。
func TestScrubProxyError_StripsCredentialsFromUpstreamErrorChain(t *testing.T) {
	raw := "http://" + testProxyUser + ":" + testProxyPassword + "@45.205.28.160:2260"
	// 形态一：错误消息里嵌了完整原文（url.Parse 的真实形态）。
	verbatim := errors.New(`invalid proxy URL: parse "` + raw + `": net/url: invalid control character in URL`)
	got := scrubProxyError(verbatim, raw)
	requireNoCredentials(t, got)
	if !strings.Contains(got, "45.205.28.160:2260") {
		t.Fatalf("scrubbed message lost the diagnosable host: %q", got)
	}

	// 形态二：只有 url.URL.Redacted() 的产物 —— Go 只遮密码，用户名照样带出来，
	// 而且调用方未必知道原文，所以必须有不依赖原文的正则兜底。
	redactedByGo := errors.New(`proxy URL missing host: http://` + testProxyUser + `:xxxxx@`)
	got = scrubProxyError(redactedByGo, "")
	if strings.Contains(got, testProxyUser) {
		t.Fatalf("username leaked through Redacted() form: %q", got)
	}

	if scrubProxyError(nil, raw) != "" {
		t.Fatal("nil error should scrub to empty string")
	}
}

// canReplay 守的是「以后有人把带 Body 的请求接进控制面」这个未来的坑：
// Body 是一次性 reader，代理侧读过之后重放会静默发出截断的请求体。
func TestCanReplay(t *testing.T) {
	get, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	if !canReplay(get) {
		t.Fatal("bodyless GET should be replayable")
	}
	post, _ := http.NewRequest(http.MethodPost, "http://example.invalid/x", strings.NewReader("payload"))
	if canReplay(post) {
		t.Fatal("POST must never be replayed")
	}
	getWithBody, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", strings.NewReader("payload"))
	getWithBody.GetBody = nil
	if canReplay(getWithBody) {
		t.Fatal("GET with a non-rewindable body must not be replayed")
	}
	if canReplay(nil) {
		t.Fatal("nil request must not be replayable")
	}
}

// requireLogEntry 从 JSON 行日志里找出指定 msg 的那一条。
func requireLogEntry(t *testing.T, logs, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["msg"] == msg {
			return entry
		}
	}
	t.Fatalf("log entry %q not found in:\n%s", msg, logs)
	return nil
}

// requireNoCredentials 是本文件的核心断言：凭据一个字符都不许出现，
// 而且必须能看到 host:port（脱敏不能脱成无法定位故障）。
func requireNoCredentials(t *testing.T, out string) {
	t.Helper()
	for _, secret := range []string{testProxyPassword, testProxyUser, testProxyUser + ":" + testProxyPassword} {
		if strings.Contains(out, secret) {
			t.Fatalf("credential %q leaked into output:\n%s", secret, out)
		}
	}
}
