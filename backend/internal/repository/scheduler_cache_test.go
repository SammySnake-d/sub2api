package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestSchedulerMetadataAccountKeepsMirasimCooldownVisible 锁的是行为,不是白名单
// 里有没有 "provider" 这个字符串:presence 检查在实现被掏空时照样绿。
//
// 这里断言的是真实读路径的结果 —— 候选过滤读 sched:meta:<id> 投影,对这个投影调
// IsSchedulableForModel。写路径已经把家族级冷却 mirasim:7d_claude 写进 extra 了,
// 如果投影丢掉 provider,投影上的 IsMirasimAccount 就恒 false、
// mirasimModelRateLimitKeys 返回 nil、这条冷却 key 根本不会被查 —— 账号被判为可
// 调度并被反复选中(生产:账号 132 在写入冷却后 3 分钟内又被选中 12 次)。
//
// 三个断言互为对照,单独任何一条都能被劣化实现骗过:
//   - 阳性:claude 家族被冷却挡住(冷却在投影里可见)
//   - 阴性(模型维度):同一投影对 fable 家族仍可调度 —— 证明 false 是这条冷却造成
//     的,不是账号本身不可调度
//   - 阴性(身份维度):去掉 provider 标记的同款账号仍可调度 —— 证明这条冷却确实是
//     经由投影上的 mirasim 身份才被消费到的
func TestSchedulerMetadataAccountKeepsMirasimCooldownVisible(t *testing.T) {
	resetAt := time.Now().Add(5 * 24 * time.Hour).UTC().Format(time.RFC3339)
	newAccount := func(credentials map[string]any) service.Account {
		return service.Account{
			ID:          132,
			Platform:    service.PlatformAnthropic,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Credentials: credentials,
			Extra: map[string]any{
				"model_rate_limits": map[string]any{
					"mirasim:7d_claude": map[string]any{
						"rate_limited_at":     time.Now().UTC().Format(time.RFC3339),
						"rate_limit_reset_at": resetAt,
						"reason":              "mirasim_7d_claude_window_exhausted",
					},
				},
			},
		}
	}

	mirasim := newAccount(map[string]any{
		"provider":      "mirasim",
		"refresh_token": "secret-refresh-token",
		"access_token":  "secret-access-token",
	})
	metadata := buildSchedulerMetadataAccount(mirasim)

	require.False(t, metadata.IsSchedulableForModel("claude-sonnet-4-5"),
		"mirasim 家族冷却在候选投影上必须生效,否则已耗尽的账号会被反复选中")
	// claude ⊇ fable(2026-09-17 生产实证):7d_claude 耗尽时 fable 也停。
	// 这条原本是阴性对照(断言 fable 仍可调度),那正是缺陷本体 —— 调度器据此
	// 把 fable 请求反复发给已耗尽的号,一路撞上游 429。
	require.False(t, metadata.IsSchedulableForModel("claude-fable-5"),
		"7d_claude 耗尽时 fable 也必须停 —— fable 的额度包含在 claude 窗口里")
	// 换一个**不消耗 claude 窗口**的模型当阴性对照:它必须仍可调度,
	// 否则上面两条 false 可能来自"账号整体不可调度"而不是这条 scope 冷却。
	require.True(t, metadata.IsSchedulableForModel("kimi-k3"),
		"不消耗 claude 家族窗口的模型必须不受影响;否则上面的 false 不能归因到这条冷却")

	plain := newAccount(map[string]any{"refresh_token": "secret-refresh-token"})
	plainMetadata := buildSchedulerMetadataAccount(plain)
	require.True(t, plainMetadata.IsSchedulableForModel("claude-sonnet-4-5"),
		"没有 provider 标记的 anthropic 账号不消费 mirasim scope;否则上面的阳性不是经由 mirasim 身份成立的")

	// 差分阴性:provider 进白名单不得退化成"整份 credentials 原样投影",
	// 否则真凭据会被写进调度缓存,而上面的阳性断言照样绿。
	require.Empty(t, metadata.GetCredential("refresh_token"))
	require.Empty(t, metadata.GetCredential("access_token"))
	require.NotContains(t, metadata.Credentials, "refresh_token")
	require.NotContains(t, metadata.Credentials, "access_token")
}

func TestFilterSchedulerCredentialsKeepsSubscriptionPlanType(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"plan_type":     "plus",
		"access_token":  "secret-access-token",
		"refresh_token": "secret-refresh-token",
	})

	require.Equal(t, "plus", filtered["plan_type"])
	require.NotContains(t, filtered, "access_token")
	require.NotContains(t, filtered, "refresh_token")
}

func TestSchedulerMetadataAccountKeepsOpenAISubscriptionIdentity(t *testing.T) {
	account := service.Account{
		ID:       24,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"plan_type":    "plus",
			"access_token": "secret-access-token",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)

	require.True(t, metadata.IsOpenAIChatGPTSubscription())
	require.Empty(t, metadata.GetCredential("access_token"))
}

func TestSchedulerMetadataAccountProjectsUpstreamBillingProbe(t *testing.T) {
	lastError := strings.Repeat("upstream diagnostic ", 512)
	probe := map[string]any{
		"status": "ok",
		"data": map[string]any{
			"billing_scope":             "token",
			"resolved_rate_multiplier":  0.03,
			"peak_rate_enabled":         true,
			"peak_start":                "09:00",
			"peak_end":                  "18:00",
			"peak_rate_multiplier":      2.0,
			"timezone":                  "Asia/Shanghai",
			"effective_rate_multiplier": 0.03,
			"remote_diagnostic":         lastError,
		},
		"received_at":   "2026-07-13T10:00:00Z",
		"fresh_until":   "2026-07-13T11:00:00Z",
		"next_probe_at": "2026-07-13T10:30:00Z",
		"http_status":   502,
		"last_error":    lastError,
	}
	account := service.Account{
		ID: 42,
		Extra: map[string]any{
			"upstream_billing_probe": probe,
			"unused_large_field":     "drop-me",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	fullPayload, metaPayload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	filtered, ok := metadata.Extra["upstream_billing_probe"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ok", filtered["status"])
	require.Equal(t, "2026-07-13T10:00:00Z", filtered["received_at"])
	require.Equal(t, "2026-07-13T11:00:00Z", filtered["fresh_until"])
	require.Equal(t, "2026-07-13T10:30:00Z", filtered["next_probe_at"])
	require.NotContains(t, filtered, "http_status")
	require.NotContains(t, filtered, "last_error")
	filteredData, ok := filtered["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "token", filteredData["billing_scope"])
	require.Equal(t, 0.03, filteredData["resolved_rate_multiplier"])
	require.Equal(t, true, filteredData["peak_rate_enabled"])
	require.Equal(t, "09:00", filteredData["peak_start"])
	require.Equal(t, "18:00", filteredData["peak_end"])
	require.Equal(t, 2.0, filteredData["peak_rate_multiplier"])
	require.Equal(t, "Asia/Shanghai", filteredData["timezone"])
	require.NotContains(t, filteredData, "effective_rate_multiplier")
	require.NotContains(t, filteredData, "remote_diagnostic")
	require.NotContains(t, metadata.Extra, "unused_large_field")
	require.Contains(t, string(fullPayload), lastError)
	require.NotContains(t, string(metaPayload), "last_error")
	require.Less(t, len(metaPayload)*4, len(fullPayload))
}

func TestSchedulerMetadataAccountDropsInvalidUpstreamBillingProbe(t *testing.T) {
	for _, probe := range []any{
		"invalid",
		map[string]any{},
		map[string]any{"status": ""},
	} {
		metadata := buildSchedulerMetadataAccount(service.Account{
			Extra: map[string]any{service.UpstreamBillingProbeExtraKey: probe},
		})

		require.NotContains(t, metadata.Extra, service.UpstreamBillingProbeExtraKey)
	}
}
