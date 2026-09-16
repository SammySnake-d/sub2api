//go:build unit

package middleware

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSessionBindingContextFollowsForwardedIPSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name string
		// trustForwarded 是后台那个开关；forwardedHeaders 是它生效时才会被读的
		// 自定义头列表。两者分开写，是因为缺陷本体恰恰在「开关为真时头怎么被读」，
		// 只给开关不给头，resolveClientIP 的 len(headers) > 0 会提前短路，
		// peer 闸根本不会被穿过，断言就锁不住任何东西。
		trustForwarded   bool
		forwardedHeaders []string
		// ipTrustedProxies 喂 ip 包自己的可信链（peer 闸），ginTrustedProxies 喂 gin 的。
		// 两条必须分开验：gin 那条早就存在，本次改动新增的是 ip 这条。
		ipTrustedProxies  []string
		ginTrustedProxies []string
		wantIP            string
	}{
		// 这一条锁的是缺陷本体：开关**单独**不再构成信任。老语义下它是 1.2.3.4
		// —— 只要开关为 true 就读裸 X-Real-IP，与 peer 是谁无关，于是任何能连上
		// 监听口的客户端都能自报 IP。现在读不读转发头由 peer 是否落在
		// trusted_proxies 决定，开关退化成「可信 peer 送来的头要不要读」。
		{
			name:             "switch alone no longer trusts an untrusted peer",
			trustForwarded:   true,
			forwardedHeaders: []string{"X-Real-IP"},
			wantIP:           "127.0.0.1",
		},
		// 正面对照：同样的开关与同样的头，只把 peer 挪进可信链，结果立刻变成 1.2.3.4。
		// 有它在，上面那条红才能被读成「peer 不可信」而不是「转发头被整个关掉了」。
		{
			name:             "switch honours headers from a trusted peer",
			trustForwarded:   true,
			forwardedHeaders: []string{"X-Real-IP"},
			ipTrustedProxies: []string{"127.0.0.1"},
			wantIP:           "1.2.3.4",
		},
		{name: "disabled switch ignores untrusted headers", trustForwarded: false, wantIP: "127.0.0.1"},
		{name: "disabled switch uses configured Gin proxy", trustForwarded: false, ginTrustedProxies: []string{"127.0.0.1"}, wantIP: "1.2.3.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.SetForwardedClientIPSettings(tc.trustForwarded, tc.forwardedHeaders)

			require.NoError(t, ip.SetTrustedProxies(tc.ipTrustedProxies))
			t.Cleanup(func() { require.NoError(t, ip.SetTrustedProxies(nil)) })

			r := gin.New()
			require.NoError(t, r.SetTrustedProxies(tc.ginTrustedProxies))
			r.Use(SessionBindingContext(cfg))
			r.GET("/t", func(c *gin.Context) {
				binding := service.SessionBindingFromContext(c.Request.Context())
				require.NotNil(t, binding)
				require.Equal(t, tc.wantIP, binding.IP)
				require.Equal(t, "test-agent", binding.UserAgent)
				require.Equal(t, tc.wantIP, SecurityClientIP(c))
				c.Status(200)
			})

			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/t", nil)
			req.RemoteAddr = "127.0.0.1:54321"
			req.Header.Set("X-Real-IP", "1.2.3.4")
			req.Header.Set("User-Agent", "test-agent")
			r.ServeHTTP(w, req)

			require.Equal(t, 200, w.Code)
		})
	}
}

func TestSessionBindingContextSnapshotsForwardedModeAndHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.SetForwardedClientIPSettings(true, []string{"X-Initial-IP"})

	// 这条测的是「请求内快照不被中途改配置影响」，自定义头那条分支必须真的走到，
	// 否则断言会在「peer 不可信 → 直接回落 peer」上凑绿，变成空转。
	// gin 的可信链保持 nil：要验的是 ip 包自己的 peer 闸，不是 gin 的。
	require.NoError(t, ip.SetTrustedProxies([]string{"9.9.9.9"}))
	t.Cleanup(func() { require.NoError(t, ip.SetTrustedProxies(nil)) })

	r := gin.New()
	require.NoError(t, r.SetTrustedProxies(nil))
	r.Use(SessionBindingContext(cfg))
	r.GET("/t", func(c *gin.Context) {
		binding := service.SessionBindingFromContext(c.Request.Context())
		require.NotNil(t, binding)
		require.Equal(t, "1.2.3.4", binding.IP)

		cfg.SetForwardedClientIPSettings(false, []string{"X-Changed-IP"})
		require.Equal(t, "1.2.3.4", ip.GetSecurityClientIP(c, false))
		c.Status(200)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/t", nil)
	req.RemoteAddr = "9.9.9.9:12345"
	req.Header.Set("X-Initial-IP", "1.2.3.4")
	req.Header.Set("X-Changed-IP", "4.4.4.4")
	req.Header.Set("X-Real-IP", "8.8.8.8")
	r.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code)
	runtimeSettings := cfg.ForwardedClientIPSettings()
	require.False(t, runtimeSettings.TrustForwardedIP)
	require.Equal(t, []string{"X-Changed-IP"}, runtimeSettings.Headers)
}

func TestSessionBindingContextBoundsPersistedUserAgent(t *testing.T) {
	cfg := &config.Config{}
	r := gin.New()
	r.Use(SessionBindingContext(cfg))
	r.GET("/t", func(c *gin.Context) {
		binding := service.SessionBindingFromContext(c.Request.Context())
		require.Len(t, binding.UserAgent, maxPersistentUserAgentBytes)
		require.Equal(t, binding.UserAgent, c.Request.UserAgent())
		c.Status(200)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/t", nil)
	req.Header.Set("User-Agent", strings.Repeat("u", 2048))
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
}

// 未经过 SessionBindingContext 注入时（异常挂载顺序/单测直调），回退 trusted_proxies 链，
// 等价于开关关闭时的历史行为。
func TestSecurityClientIPFallsBackWithoutInjectedBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	require.NoError(t, r.SetTrustedProxies(nil))
	r.GET("/t", func(c *gin.Context) {
		c.String(200, SecurityClientIP(c))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/t", nil)
	req.RemoteAddr = "9.9.9.9:12345"
	req.Header.Set("X-Real-IP", "1.2.3.4")
	r.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code)
	require.Equal(t, "9.9.9.9", w.Body.String())
}

func TestRequestSessionBindingPrefersInjectedBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.SetTrustForwardedIPForAPIKeyACL(true)

	r := gin.New()
	require.NoError(t, r.SetTrustedProxies([]string{"127.0.0.1"}))
	r.Use(SessionBindingContext(cfg))
	r.GET("/t", func(c *gin.Context) {
		issued := &service.SessionBinding{IP: "1.2.3.4", UserAgent: "test-agent"}
		require.Equal(t, issued.Hash(), requestSessionBinding(c).Hash())
		c.Status(200)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/t", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Real-IP", "1.2.3.4")
	req.Header.Set("User-Agent", "test-agent")
	r.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code)
}
