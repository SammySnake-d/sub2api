package config

import (
	"testing"

	"github.com/spf13/viper"
)

// ControlPlaneProxyURL 是「控制面代理该用哪个地址」的唯一落点。
// 这里锁住的是取值优先级和「留空 = 直连」的兼容底线：一旦有人把回落逻辑改掉，
// 已经在用 update.proxy_url 的国内部署会在升级后悄悄退回直连（表现就是定价和
// 版本同步又开始 TLS handshake timeout），而这种回归不会有任何编译错误。
func TestControlPlaneProxyURL(t *testing.T) {
	cases := []struct {
		name         string
		controlPlane string
		update       string
		want         string
	}{
		{
			name: "both empty means direct",
			want: "",
		},
		{
			name:   "falls back to legacy update.proxy_url",
			update: "http://user:pass@45.205.28.160:2260",
			want:   "http://user:pass@45.205.28.160:2260",
		},
		{
			name:         "control_plane wins over legacy key",
			controlPlane: "http://user:pass@45.205.28.160:2260",
			update:       "http://127.0.0.1:7890",
			want:         "http://user:pass@45.205.28.160:2260",
		},
		{
			// 纯空白等于没配：容器环境里 CONTROL_PLANE_PROXY_URL="" 或带尾空格很常见，
			// 若当成「配了个空代理」就会挡住老配置项的回落。
			name:         "whitespace-only control_plane falls back",
			controlPlane: "   ",
			update:       "http://127.0.0.1:7890",
			want:         "http://127.0.0.1:7890",
		},
		{
			name:         "values are trimmed",
			controlPlane: "  http://127.0.0.1:7890  ",
			want:         "http://127.0.0.1:7890",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.ControlPlane.ProxyURL = tc.controlPlane
			cfg.Update.ProxyURL = tc.update
			if got := cfg.ControlPlaneProxyURL(); got != tc.want {
				t.Fatalf("ControlPlaneProxyURL() = %q, want %q", got, tc.want)
			}
		})
	}

	var nilCfg *Config
	if got := nilCfg.ControlPlaneProxyURL(); got != "" {
		t.Fatalf("nil config should resolve to direct, got %q", got)
	}
}

// 生产部署（deploy/docker-compose.yml）是纯环境变量驱动的，所以「能从 env 读到」
// 不能只靠 TestConfigKeysAreEnvReachable 的键名存在性推断，这里直接过一遍
// viper 的解码链。
//
// 踩过的坑：viper.Unmarshal 只解码 AllKeys() 里的键，而 AutomaticEnv 只能覆盖
// 已存在的键、不会新增。没在 setDefaults 注册过的键，环境变量会被读到然后静默丢掉 ——
// 对「配了代理还是在超时」这种故障是最难查的形态，因为配置看起来是对的。
func TestControlPlaneProxyURLIsReadableFromEnvironment(t *testing.T) {
	t.Setenv("CONTROL_PLANE_PROXY_URL", "http://user:pass@45.205.28.160:2260")

	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(envKeyReplacer())
	setDefaults()

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cfg.ControlPlaneProxyURL(); got != "http://user:pass@45.205.28.160:2260" {
		t.Fatalf("ControlPlaneProxyURL() = %q; CONTROL_PLANE_PROXY_URL was silently dropped", got)
	}
}
