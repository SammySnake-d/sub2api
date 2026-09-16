// Package server provides HTTP server initialization and configuration.
package server

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/websearch"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/http2"
)

// ProviderSet 提供服务器层的依赖
var ProviderSet = wire.NewSet(
	ProvideRouter,
	ProvideHTTPServer,
)

// ProvideRouter 提供路由器
func ProvideRouter(
	cfg *config.Config,
	handlers *handler.Handlers,
	jwtAuth middleware2.JWTAuthMiddleware,
	optionalJWTAuth middleware2.OptionalJWTAuthMiddleware,
	adminAuth middleware2.AdminAuthMiddleware,
	apiKeyAuth middleware2.APIKeyAuthMiddleware,
	auditLog middleware2.AuditLogMiddleware,
	stepUpAuth middleware2.StepUpAuthMiddleware,
	apiKeyService *service.APIKeyService,
	subscriptionService *service.SubscriptionService,
	opsService *service.OpsService,
	settingService *service.SettingService,
	compositeResolver *service.CompositeRouteResolver,
	redisClient *redis.Client,
) *gin.Engine {
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(middleware2.Recovery())
	configureTrustedProxies(r, cfg.Server)
	// Emitted here rather than inside configureTrustedProxies because the live
	// forwarded-IP trust value is only known after SettingService has loaded it
	// from the database (service.ProvideSettingService ->
	// LoadForwardedClientIPSettings), which happens before the router is built.
	warnClientIPTrust(cfg)

	// Wire up websearch Manager builder so it initializes on startup and rebuilds on config save.
	settingService.SetWebSearchManagerBuilder(context.Background(), func(cfg *service.WebSearchEmulationConfig, proxyURLs map[int64]string) {
		if cfg == nil || !cfg.Enabled || len(cfg.Providers) == 0 {
			service.SetWebSearchManager(nil)
			return
		}
		configs := make([]websearch.ProviderConfig, 0, len(cfg.Providers))
		for _, p := range cfg.Providers {
			if p.APIKey == "" {
				continue
			}
			pc := websearch.ProviderConfig{
				Type:       p.Type,
				APIKey:     p.APIKey,
				QuotaLimit: derefInt64(p.QuotaLimit),
				ExpiresAt:  p.ExpiresAt,
			}
			if p.SubscribedAt != nil {
				pc.SubscribedAt = p.SubscribedAt
			}
			if p.ProxyID != nil {
				pc.ProxyID = *p.ProxyID
				if u, ok := proxyURLs[*p.ProxyID]; ok {
					pc.ProxyURL = u
				} else {
					// Proxy configured but not found — skip this provider to prevent direct connection.
					slog.Warn("websearch: proxy not found for provider, skipping",
						"provider", p.Type, "proxy_id", *p.ProxyID)
					continue
				}
			}
			configs = append(configs, pc)
		}
		service.SetWebSearchManager(websearch.NewManager(configs, redisClient))
	})

	return SetupRouter(r, handlers, jwtAuth, optionalJWTAuth, adminAuth, apiKeyAuth, auditLog, stepUpAuth, apiKeyService, subscriptionService, opsService, settingService, compositeResolver, cfg, redisClient)
}

func configureTrustedProxies(r *gin.Engine, cfg config.ServerConfig) {
	if cfg.TrustedProxiesConfigured {
		if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
			log.Printf("Failed to set trusted proxies: %v", err)
			_ = r.SetTrustedProxies(nil)
		}
	} else {
		if err := r.SetTrustedProxies(nil); err != nil {
			log.Printf("Failed to disable trusted proxies: %v", err)
		}
	}
}

// clientIPTrustWarnings returns the startup warnings for client-IP trust
// settings that make the resolved client IP wrong or forgeable.
//
// An absent or explicitly empty server.trusted_proxies is NOT forgeable: Gin and
// internal/pkg/ip both resolve the client IP from the direct peer address only,
// which is exactly right for a listener reached with no reverse proxy in front.
// A wrong non-empty value is strictly worse than none, because every peer
// matching it may then dictate its own client IP through X-Forwarded-For and
// walk past IP rate limiting, audit attribution and API key IP allowlists.
//
// 这里的判据在 2026-09-17 换过一次，旧文案现在是**错的**，不要照抄回来。
// 旧实现把「读不读转发头」挂在 security.trust_forwarded_ip_for_api_key_acl 这个
// peer 无关的全局布尔上，于是那条 WARN 说「开了它，任何能连上监听口的客户端都能
// 伪造自己的 IP，请设成 false」。判据改成按直连 peer 判定之后：
//   - 「任何客户端都能伪造」不再成立 —— 不可信 peer 的头没有任何出路；
//   - 「请设成 false」成了**有害处方**：照做只会把可信代理送来的自定义 CDN 头
//     也一并关掉，换不来任何安全提升。
//
// 现在真正值得出声的是相反那个状态：**前面有反代、却没配 server.trusted_proxies**。
// 那时 PeerIsTrustedProxy 恒为假，每个请求都被记成反代的落点地址（例如 172.21.0.1），
// 后果是 API Key 的 IP 白名单全员 403、限流桶全站塌进一个、会话绑定全绑到同一个 IP。
// 它不可能被完全自动识别（我们无从得知前面有没有反代），所以在「开关开着但可信代理
// 列表为空」这个明确自相矛盾的组合上提示：开关声明了要读代理送来的头，却没有任何
// peer 被认定为代理。
func clientIPTrustWarnings(srv config.ServerConfig, forwardedTrustEnabled bool) []string {
	if srv.Mode != "release" {
		return nil
	}
	var warnings []string
	if forwardedTrustEnabled && len(srv.TrustedProxies) == 0 {
		warnings = append(warnings,
			"security.trust_forwarded_ip_for_api_key_acl is enabled but server.trusted_proxies is empty, so no peer is treated as a proxy and forwarded client-IP headers are ignored. If a reverse proxy fronts this listener, every request is attributed to the proxy's own address, which breaks API key IP allowlists, per-IP rate limiting and session binding: set server.trusted_proxies to the proxy's address. If nothing fronts this listener, turn the switch off instead.")
	}
	if catchAll := catchAllTrustedProxies(srv.TrustedProxies); len(catchAll) > 0 {
		warnings = append(warnings,
			"server.trusted_proxies contains catch-all range(s) "+strings.Join(catchAll, ", ")+"; every peer is treated as a trusted proxy, so forwarded client IPs can be forged.")
	}
	return warnings
}

// catchAllTrustedProxies returns the configured entries that match every
// address (a zero-length prefix such as 0.0.0.0/0 or ::/0).
func catchAllTrustedProxies(proxies []string) []string {
	var catchAll []string
	for _, entry := range proxies {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		if trimmed == "*" {
			catchAll = append(catchAll, trimmed)
			continue
		}
		if _, network, err := net.ParseCIDR(trimmed); err == nil {
			if ones, _ := network.Mask.Size(); ones == 0 {
				catchAll = append(catchAll, trimmed)
			}
		}
	}
	return catchAll
}

func warnClientIPTrust(cfg *config.Config) {
	if cfg == nil {
		return
	}
	for _, warning := range clientIPTrustWarnings(cfg.Server, cfg.ForwardedClientIPTrustEnabled()) {
		log.Printf("Warning: %s", warning)
	}
}

// ProvideHTTPServer 提供 HTTP 服务器
func ProvideHTTPServer(cfg *config.Config, router *gin.Engine) *http.Server {
	httpHandler := http.Handler(router)
	server := &http.Server{
		Addr:           cfg.Server.Address(),
		Handler:        httpHandler,
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
		// ReadHeaderTimeout: 读取请求头的超时时间，防止慢速请求头攻击
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeout) * time.Second,
		// IdleTimeout: 空闲连接超时时间，释放不活跃的连接资源
		IdleTimeout: time.Duration(cfg.Server.IdleTimeout) * time.Second,
		// 注意：不设置 WriteTimeout，因为流式响应可能持续十几分钟
		// 不设置 ReadTimeout，因为大请求体可能需要较长时间读取
	}

	globalMaxSize := cfg.Server.MaxRequestBodySize
	if globalMaxSize <= 0 {
		globalMaxSize = cfg.Gateway.MaxBodySize
	}
	if globalMaxSize > 0 {
		httpHandler = http.MaxBytesHandler(httpHandler, globalMaxSize)
		log.Printf("Global max request body size: %d bytes (%.2f MB)", globalMaxSize, float64(globalMaxSize)/(1<<20))
	}

	// 根据配置决定是否启用 H2C
	if cfg.Server.H2C.Enabled {
		h2cConfig := cfg.Server.H2C
		if err := http2.ConfigureServer(server, &http2.Server{
			MaxConcurrentStreams:         h2cConfig.MaxConcurrentStreams,
			IdleTimeout:                  time.Duration(h2cConfig.IdleTimeout) * time.Second,
			MaxReadFrameSize:             uint32(h2cConfig.MaxReadFrameSize),
			MaxUploadBufferPerConnection: int32(h2cConfig.MaxUploadBufferPerConnection),
			MaxUploadBufferPerStream:     int32(h2cConfig.MaxUploadBufferPerStream),
		}); err != nil {
			log.Printf("Failed to configure HTTP/2 Cleartext (h2c): %v", err)
		} else {
			protocols := new(http.Protocols)
			protocols.SetHTTP1(true)
			protocols.SetUnencryptedHTTP2(true)
			server.Protocols = protocols
			log.Printf("HTTP/2 Cleartext (h2c) enabled: max_concurrent_streams=%d, idle_timeout=%ds, max_read_frame_size=%d, max_upload_buffer_per_connection=%d, max_upload_buffer_per_stream=%d",
				h2cConfig.MaxConcurrentStreams,
				h2cConfig.IdleTimeout,
				h2cConfig.MaxReadFrameSize,
				h2cConfig.MaxUploadBufferPerConnection,
				h2cConfig.MaxUploadBufferPerStream,
			)
		}
	}

	server.Handler = httpHandler
	return server
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
