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
// settings that make the resolved client IP forgeable.
//
// An absent or explicitly empty server.trusted_proxies is NOT such a state: it
// makes Gin resolve the client IP from the direct peer address only, which is
// exactly right for a listener that is reached directly with no reverse proxy
// in front. The previous warning fired on that safe default and pushed
// operators toward inventing a trusted-proxy value; a wrong non-empty value is
// strictly worse than none, because every peer matching it may then dictate its
// own client IP through X-Forwarded-For and walk past IP rate limiting, audit
// attribution and API key IP allowlists.
//
// What does deserve a warning is the legacy compatibility switch: while
// security.trust_forwarded_ip_for_api_key_acl is enabled, raw forwarding
// headers take over client-IP resolution regardless of the trusted-proxy chain
// (internal/pkg/ip/ip.go GetClientIP / GetSecurityClientIP), so any client that
// can reach the listener can forge its client IP. That is reported for every
// deployment shape, including one behind a real reverse proxy, because the
// headers are trusted there without verifying they came from that proxy.
func clientIPTrustWarnings(srv config.ServerConfig, forwardedTrustEnabled bool) []string {
	if srv.Mode != "release" {
		return nil
	}
	var warnings []string
	if forwardedTrustEnabled {
		warnings = append(warnings,
			"security.trust_forwarded_ip_for_api_key_acl is enabled; raw X-Forwarded-For/X-Real-IP/CF-Connecting-IP headers override server.trusted_proxies when resolving the client IP for rate limiting, audit logs, session binding and API key IP allowlists, so any client that can reach this listener can forge its client IP. Set security.trust_forwarded_ip_for_api_key_acl=false unless a trusted reverse proxy is the only path to this port.")
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
