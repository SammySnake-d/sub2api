//go:build unit

package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func ingressTestConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Host:               "127.0.0.1",
			ReadHeaderTimeout:  1,
			IdleTimeout:        5,
			MaxHeaderBytes:     8 * 1024,
			MaxRequestBodySize: 1024,
		},
		Gateway: config.GatewayConfig{MaxBodySize: 1024},
	}
}

func TestProvideHTTPServerAppliesIngressLimits(t *testing.T) {
	srv := ProvideHTTPServer(ingressTestConfig(), gin.New())
	require.Equal(t, 8*1024, srv.MaxHeaderBytes)
	require.Equal(t, time.Second, srv.ReadHeaderTimeout)
	require.Equal(t, 5*time.Second, srv.IdleTimeout)
}

func TestProvideHTTPServerEnablesBoundedH2C(t *testing.T) {
	cfg := ingressTestConfig()
	cfg.Server.H2C = config.H2CConfig{
		Enabled:                      true,
		MaxConcurrentStreams:         25,
		IdleTimeout:                  30,
		MaxReadFrameSize:             64 * 1024,
		MaxUploadBufferPerConnection: 1024 * 1024,
		MaxUploadBufferPerStream:     256 * 1024,
	}
	srv := ProvideHTTPServer(cfg, gin.New())
	require.NotNil(t, srv.Protocols)
	require.True(t, srv.Protocols.UnencryptedHTTP2())
	require.True(t, srv.Protocols.HTTP1())
}

func TestConfigureTrustedProxies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		cfg  config.ServerConfig
		want string
	}{
		{
			name: "configured proxy resolves forwarded client",
			cfg: config.ServerConfig{
				TrustedProxies:           []string{"9.9.9.9/32"},
				TrustedProxiesConfigured: true,
			},
			want: "1.2.3.4",
		},
		{
			name: "explicit empty list ignores forwarded client",
			cfg: config.ServerConfig{
				TrustedProxiesConfigured: true,
			},
			want: "9.9.9.9",
		},
		{
			name: "invalid proxy list fails closed",
			cfg: config.ServerConfig{
				TrustedProxies:           []string{"not-a-cidr"},
				TrustedProxiesConfigured: true,
			},
			want: "9.9.9.9",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			configureTrustedProxies(r, tc.cfg)
			r.GET("/t", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/t", nil)
			req.RemoteAddr = "9.9.9.9:12345"
			req.Header.Set("X-Forwarded-For", "1.2.3.4")
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, tc.want, w.Body.String())
		})
	}
}

func TestHTTPServerRejectsOversizedHTTP1Header(t *testing.T) {
	r := gin.New()
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	srv := ProvideHTTPServer(ingressTestConfig(), r)
	addr, stop := serveIngressTestServer(t, srv)
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\nX-Fill: "+strings.Repeat("a", 32*1024)+"\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusRequestHeaderFieldsTooLarge, resp.StatusCode)
}

func TestHTTPServerClosesSlowIncompleteHeader(t *testing.T) {
	r := gin.New()
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	srv := ProvideHTTPServer(ingressTestConfig(), r)
	addr, stop := serveIngressTestServer(t, srv)
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\nX-Slow:")
	require.NoError(t, err)
	time.Sleep(1200 * time.Millisecond)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = bufio.NewReader(conn).ReadByte()
	require.Error(t, err)
}

func TestHTTPServerGlobalBodyLimit(t *testing.T) {
	r := gin.New()
	r.POST("/", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				c.Status(http.StatusRequestEntityTooLarge)
				return
			}
		}
		c.Status(http.StatusOK)
	})
	srv := ProvideHTTPServer(ingressTestConfig(), r)
	req, err := http.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 1025)))
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func serveIngressTestServer(t *testing.T, srv *http.Server) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// TestClientIPTrustWarnings locks the predicate for the client-IP trust
// startup warnings: the warning must track "the resolved client IP is
// forgeable", not "trusted_proxies is unset".
func TestClientIPTrustWarnings(t *testing.T) {
	const release = "release"

	t.Run("silent when no proxy is in front and forwarded trust is off", func(t *testing.T) {
		// Direct-peer deployment: nothing in front of the listener, so Gin
		// resolves the client IP from the peer address. This is correct and
		// must not be reported as a missing configuration.
		require.Empty(t, clientIPTrustWarnings(
			config.ServerConfig{Mode: release}, false))
		// Explicitly empty is the same safe state.
		require.Empty(t, clientIPTrustWarnings(
			config.ServerConfig{Mode: release, TrustedProxies: []string{}, TrustedProxiesConfigured: true}, false))
	})

	t.Run("warns when forwarded trust makes the client IP forgeable", func(t *testing.T) {
		warnings := clientIPTrustWarnings(config.ServerConfig{Mode: release}, true)
		require.Len(t, warnings, 1)
		require.Contains(t, warnings[0], "trust_forwarded_ip_for_api_key_acl")
		require.Contains(t, warnings[0], "forge")
	})

	// Differential negative: a deployment that really is behind a reverse proxy
	// must still be warned while raw forwarding headers can override the
	// trusted-proxy chain.
	t.Run("still warns behind a configured reverse proxy", func(t *testing.T) {
		warnings := clientIPTrustWarnings(config.ServerConfig{
			Mode:                     release,
			TrustedProxies:           []string{"10.0.0.7/32"},
			TrustedProxiesConfigured: true,
		}, true)
		require.Len(t, warnings, 1)
		require.Contains(t, warnings[0], "trust_forwarded_ip_for_api_key_acl")
	})

	t.Run("silent behind a reverse proxy once forwarded trust is off", func(t *testing.T) {
		require.Empty(t, clientIPTrustWarnings(config.ServerConfig{
			Mode:                     release,
			TrustedProxies:           []string{"10.0.0.7/32"},
			TrustedProxiesConfigured: true,
		}, false))
	})

	t.Run("warns on catch-all trusted proxy ranges", func(t *testing.T) {
		for _, entry := range []string{"0.0.0.0/0", "::/0", "*"} {
			warnings := clientIPTrustWarnings(config.ServerConfig{
				Mode:                     release,
				TrustedProxies:           []string{entry},
				TrustedProxiesConfigured: true,
			}, false)
			require.Len(t, warnings, 1, entry)
			require.Contains(t, warnings[0], "catch-all", entry)
			require.Contains(t, warnings[0], entry)
		}
	})

	t.Run("specific proxy ranges are not catch-all", func(t *testing.T) {
		require.Empty(t, catchAllTrustedProxies([]string{"10.0.0.0/8", "127.0.0.1", "192.168.1.0/24", "not-a-cidr", ""}))
	})

	t.Run("debug mode stays quiet", func(t *testing.T) {
		require.Empty(t, clientIPTrustWarnings(config.ServerConfig{Mode: "debug"}, true))
	})
}
