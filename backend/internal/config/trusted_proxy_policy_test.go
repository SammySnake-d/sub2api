package config

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
)

// 2026-09-16 生产事故的拓扑：Caddy 经 docker MASQUERADE 打到 6699 时源地址被改写成
// 网桥网关 172.21.0.1（nsenter 采样 6699 上 100% 的 established socket peer 都是它）；
// 公网直连的包不过 MASQUERADE，源地址保留（探针实测 203.10.99.42）。
// 所以「谁的转发头可以被读」这条判据必须且只能落在 peer 上。
const (
	policyCaddyPeer  = "172.21.0.1"
	policyRealClient = "203.10.99.42"
)

func resetTrustedProxyPolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { require.NoError(t, ip.SetTrustedProxies(nil)) })
	require.NoError(t, ip.SetTrustedProxies(nil))
}

// 配置里的可信代理必须真的推到 internal/pkg/ip。少了这一步，ip 包保持
// fail-closed（谁都不信），反代送来的自定义 CDN 头永远读不到。
func TestApplyTrustedProxyPolicyPushesConfiguredProxies(t *testing.T) {
	resetTrustedProxyPolicy(t)

	cfg := &Config{Server: ServerConfig{
		TrustedProxies:           []string{policyCaddyPeer + "/32"},
		TrustedProxiesConfigured: true,
	}}
	require.NoError(t, cfg.ApplyTrustedProxyPolicy())

	require.True(t, ip.IsTrustedProxyAddr(policyCaddyPeer))
	// 差分：公网客户端绝不能因为「配了可信代理」就跟着变可信，否则 X-Real-IP /
	// CF-Connecting-IP 又回到人人可自报的状态。
	require.False(t, ip.IsTrustedProxyAddr(policyRealClient))
	require.False(t, ip.IsTrustedProxyAddr("172.21.0.2"))
}

// 没有显式配置 server.trusted_proxies 时必须 fail-closed。这正是 config.yaml 写着
// trusted_proxies: [] 的生产现状，也是 http.go configureTrustedProxies 的语义。
func TestApplyTrustedProxyPolicyFailsClosedWhenUnconfigured(t *testing.T) {
	resetTrustedProxyPolicy(t)
	require.NoError(t, ip.SetTrustedProxies([]string{policyCaddyPeer + "/32"}))
	require.True(t, ip.IsTrustedProxyAddr(policyCaddyPeer))

	cfg := &Config{Server: ServerConfig{
		TrustedProxies:           []string{policyCaddyPeer + "/32"},
		TrustedProxiesConfigured: false,
	}}
	require.NoError(t, cfg.ApplyTrustedProxyPolicy())

	require.False(t, ip.IsTrustedProxyAddr(policyCaddyPeer),
		"未显式配置就必须谁都不信，不能沿用上一次推入的快照")
}

// 非法规则：整份作废，与 gin 报错后 http.go 退回 SetTrustedProxies(nil) 一致。
// 绝不能出现「ip 包比 gin 宽松」的裂缝。
func TestApplyTrustedProxyPolicyFailsClosedOnInvalidEntry(t *testing.T) {
	resetTrustedProxyPolicy(t)

	cfg := &Config{Server: ServerConfig{
		TrustedProxies:           []string{policyCaddyPeer + "/32", "not-an-ip"},
		TrustedProxiesConfigured: true,
	}}
	require.Error(t, cfg.ApplyTrustedProxyPolicy())
	require.False(t, ip.IsTrustedProxyAddr(policyCaddyPeer))
}

// 端到端：Load() 必须自己把策略推下去，不能依赖调用方记得多调一步。
func TestLoadAppliesTrustedProxyPolicy(t *testing.T) {
	resetTrustedProxyPolicy(t)
	resetViperWithJWTSecret(t)
	viper.Set("server.trusted_proxies", []string{policyCaddyPeer + "/32"})

	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.Server.TrustedProxiesConfigured)
	require.True(t, ip.IsTrustedProxyAddr(policyCaddyPeer))
	require.False(t, ip.IsTrustedProxyAddr(policyRealClient))
}

// 差分：显式空列表（生产 config.yaml 的原值）= 谁都不信。
func TestLoadExplicitEmptyTrustedProxiesTrustsNobody(t *testing.T) {
	resetTrustedProxyPolicy(t)
	resetViperWithJWTSecret(t)
	require.NoError(t, ip.SetTrustedProxies([]string{policyCaddyPeer + "/32"}))
	viper.Set("server.trusted_proxies", []string{})

	_, err := Load()
	require.NoError(t, err)
	require.False(t, ip.IsTrustedProxyAddr(policyCaddyPeer))
	require.False(t, ip.IsTrustedProxyAddr(policyRealClient))
}

// Validate() 是程序化构造配置的入口（测试与嵌入式用法走这条），同样要刷新快照。
func TestValidateAppliesTrustedProxyPolicy(t *testing.T) {
	resetTrustedProxyPolicy(t)

	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, ip.IsTrustedProxyAddr(policyCaddyPeer), "未配置时 Load 就该留下空策略")

	cfg.Server.TrustedProxies = []string{policyCaddyPeer + "/32"}
	cfg.Server.TrustedProxiesConfigured = true
	require.NoError(t, cfg.Validate())
	require.True(t, ip.IsTrustedProxyAddr(policyCaddyPeer))
	require.False(t, ip.IsTrustedProxyAddr(policyRealClient))
}
