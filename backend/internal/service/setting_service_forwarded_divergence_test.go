//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 这组测试锁死 2026-09-16 生产事故的「静默」那一半。
//
// 事故复盘：迁移时把旧环境的 settings 整批导入新库，api_key_acl_trust_forwarded_ip
// 被写成 true（settings.updated_at 2026-09-16 17:35:32，与 forwarded_client_ip_headers
// 的时间戳一致 → 同一次导入），而容器内 /app/data/config.yaml 写的是
// security.trust_forwarded_ip_for_api_key_acl: false。DB 值覆盖配置文件值，启动日志
// 里一个字都没提 —— 「文件说 false、线上跑 true」这条分叉就这么活着，直到有人拿
// 只读探针打出 client_ip=198.51.100.9（真实 peer 203.10.99.42）才暴露。
//
// 覆盖本身是既定行为（DB wins），这里要的是让它可见。

// captureSlog 把默认 logger 换成 JSON handler 并返回解析后的记录。
func captureSlog(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	fn()
	slog.SetDefault(previous)

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), "log line: %s", line)
		records = append(records, record)
	}
	return records
}

func divergenceRecords(records []map[string]any, settingKey string) []map[string]any {
	var matched []map[string]any
	for _, record := range records {
		if record["msg"] != "forwarded client IP setting: config file and database disagree, database wins" {
			continue
		}
		if settingKey != "" && record["setting_key"] != settingKey {
			continue
		}
		matched = append(matched, record)
	}
	return matched
}

// 阳性：完整复刻 17:35 那次导入 —— 库里 true、config.yaml 里 false。
func TestLoadForwardedClientIPSettings_WarnsWhenDatabaseOverridesConfigFile(t *testing.T) {
	repo := &forwardedIPMigrationRepoStub{values: map[string]string{
		SettingKeyAPIKeyACLTrustForwardedIP: "true",
		SettingKeyForwardedClientIPHeaders:  `[]`,
		settingKeyForwardedClientIPModeV2:   "true",
	}}
	cfg := &config.Config{}
	cfg.Security.TrustForwardedIPForAPIKeyACL = false // config.yaml 的值
	svc := NewSettingService(repo, cfg)

	records := captureSlog(t, func() {
		require.NoError(t, svc.LoadForwardedClientIPSettings(context.Background()))
	})

	matched := divergenceRecords(records, SettingKeyAPIKeyACLTrustForwardedIP)
	require.Len(t, matched, 1, "DB 覆盖配置文件必须留下一条可检索的 WARN")
	require.Equal(t, "WARN", matched[0]["level"])
	require.Equal(t, "security.trust_forwarded_ip_for_api_key_acl", matched[0]["config_key"])
	require.Equal(t, "false", matched[0]["config_says"])
	require.Equal(t, "true", matched[0]["db_says"])

	// 生效值仍然是 DB 的值：这条 WARN 只负责可见性，不改判定。
	require.True(t, cfg.TrustForwardedIPForAPIKeyACL())
}

// 阳性：headers 也是伪造面的一部分（一旦填了 X-Real-IP / CF-Connecting-IP，
// 自定义头解析就会接管），所以它的分叉同样要出声。
func TestLoadForwardedClientIPSettings_WarnsWhenHeadersDiverge(t *testing.T) {
	repo := &forwardedIPMigrationRepoStub{values: map[string]string{
		SettingKeyAPIKeyACLTrustForwardedIP: "true",
		SettingKeyForwardedClientIPHeaders:  `["Cf-Connecting-Ip"]`,
		settingKeyForwardedClientIPModeV2:   "true",
	}}
	cfg := &config.Config{}
	cfg.Security.TrustForwardedIPForAPIKeyACL = true
	cfg.SetForwardedClientIPSettings(true, []string{"X-Config-Ip"})
	svc := NewSettingService(repo, cfg)

	records := captureSlog(t, func() {
		require.NoError(t, svc.LoadForwardedClientIPSettings(context.Background()))
	})

	matched := divergenceRecords(records, SettingKeyForwardedClientIPHeaders)
	require.Len(t, matched, 1)
	require.Equal(t, "X-Config-Ip", matched[0]["config_says"])
	require.Equal(t, "Cf-Connecting-Ip", matched[0]["db_says"])

	// 差分：开关这一项没有分叉（两边都是 true），不能跟着一起报。
	require.Empty(t, divergenceRecords(records, SettingKeyAPIKeyACLTrustForwardedIP))
}

// 阴性（差分）：DB 与配置文件一致时必须安静。没有这条，「无条件每次都 WARN」
// 也能让上面两条阳性变绿，而那样等于把这条告警降噪成背景音。
func TestLoadForwardedClientIPSettings_SilentWhenConfigAndDatabaseAgree(t *testing.T) {
	for _, test := range []struct {
		name  string
		value bool
	}{
		{name: "both false", value: false},
		{name: "both true", value: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored := "false"
			if test.value {
				stored = "true"
			}
			repo := &forwardedIPMigrationRepoStub{values: map[string]string{
				SettingKeyAPIKeyACLTrustForwardedIP: stored,
				SettingKeyForwardedClientIPHeaders:  `[]`,
				settingKeyForwardedClientIPModeV2:   "true",
			}}
			cfg := &config.Config{}
			cfg.Security.TrustForwardedIPForAPIKeyACL = test.value
			svc := NewSettingService(repo, cfg)

			records := captureSlog(t, func() {
				require.NoError(t, svc.LoadForwardedClientIPSettings(context.Background()))
			})

			require.Empty(t, divergenceRecords(records, ""), "两边一致时不得告警")
			require.Equal(t, test.value, cfg.TrustForwardedIPForAPIKeyACL())
		})
	}
}

// 阴性（差分）：库里根本没有这条 setting 时走配置文件，没有覆盖也就没有分叉。
func TestLoadForwardedClientIPSettings_SilentWhenDatabaseHasNoStoredValue(t *testing.T) {
	repo := &forwardedIPMigrationRepoStub{values: map[string]string{
		SettingKeyForwardedClientIPHeaders: `[]`,
		settingKeyForwardedClientIPModeV2:  "true",
	}}
	cfg := &config.Config{}
	cfg.Security.TrustForwardedIPForAPIKeyACL = true
	svc := NewSettingService(repo, cfg)

	records := captureSlog(t, func() {
		require.NoError(t, svc.LoadForwardedClientIPSettings(context.Background()))
	})

	require.Empty(t, divergenceRecords(records, ""))
	require.True(t, cfg.TrustForwardedIPForAPIKeyACL())
}
