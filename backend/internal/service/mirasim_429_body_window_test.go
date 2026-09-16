package service

// 429 响应体窗口解析的回归锁。
//
// 被钉住的不变量:**上游在 429 body 里指明了耗尽窗口时,冷却必须落到那个窗口的
// scope 上、并且用真实的 reset 时间 —— 不能退化成秒级的账号级兜底。**
//
// 这条在生产上破过(2026-09-16 实录):
//
//	rate_limit_429_fallback_used  account_id=135  reason=mirasim_no_window_headers  using_default=5s
//
// 3 小时内触发 57 次,全部是同一个 reason。后果是一次渠道探测在 21 秒里换了 15 个
// 账号全部 429,而同期 143 个号中有 125 个的 7d_claude 用量还不到 80%。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const mirasimCreditExhausted7dClaudeBody = `{"error":{"code":"credit_exhausted_7d_claude",` +
	`"message":"已用满 7d_claude 用量上限，可切换其它模型继续 ` +
	`(this model family's allowance is spent; other models still work)",` +
	`"type":"rate_limit_error"}}`

func TestMirasimExhaustedWindowFromBody(t *testing.T) {
	t.Run("生产实录的那条 body", func(t *testing.T) {
		require.Equal(t, MirasimWindow7dClaude,
			mirasimExhaustedWindowFromBody([]byte(mirasimCreditExhausted7dClaudeBody)),
			"这正是线上 57 次兜底里每一次的 body —— 解不出窗口就等于这个修复没生效")
	})

	t.Run("四个窗口都要认得", func(t *testing.T) {
		for _, w := range []string{MirasimWindow5h, MirasimWindow7d, MirasimWindow7dClaude, MirasimWindow7dFable} {
			body := []byte(`{"error":{"code":"credit_exhausted_` + w + `","type":"rate_limit_error"}}`)
			require.Equal(t, w, mirasimExhaustedWindowFromBody(body), "窗口 %s 没被认出来", w)
		}
	})

	// 差分阴性。没有这一组,把函数写成"永远返回 7d_claude"也能让上面全绿 ——
	// 而那会让任何一个 429 都去冷却 claude 窗口,包括 fable 耗尽的那些。
	t.Run("不该解出窗口的输入", func(t *testing.T) {
		for name, body := range map[string]string{
			"空 body":             ``,
			"不是 JSON":            `rate limited`,
			"没有 error 段":         `{"detail":"too many requests"}`,
			"code 不是额度耗尽":        `{"error":{"code":"overloaded","type":"rate_limit_error"}}`,
			"只有 message 没有 code": `{"error":{"message":"已用满 7d_claude 用量上限","type":"rate_limit_error"}}`,
			"未知窗口名":              `{"error":{"code":"credit_exhausted_7d_unknown","type":"rate_limit_error"}}`,
		} {
			require.Equal(t, "", mirasimExhaustedWindowFromBody([]byte(body)),
				"%s 不该解出窗口", name)
		}
	})

	// 这一条单独拎出来:判据只认 error.code,**不认 message**。
	// message 是给人看的、会随文案调整而变,拿它做调度判据等于把行为绑在一句中文提示上。
	t.Run("只认 code 不认 message", func(t *testing.T) {
		onlyMessage := `{"error":{"message":"credit_exhausted_7d_claude 已用满","type":"rate_limit_error"}}`
		require.Equal(t, "", mirasimExhaustedWindowFromBody([]byte(onlyMessage)),
			"message 里出现窗口名不该被当作判据 —— 文案一改行为就变")
	})
}

func TestMirasimWindowLimitFromBodyUsesProbeResetWhenAvailable(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	probeReset := now.Add(3 * time.Hour)

	acc := &Account{
		ID: 135,
		Extra: map[string]any{
			"mirasim_quota_probe": map[string]any{
				"status": "ok",
				"windows": []any{
					map[string]any{
						"name":        MirasimWindow7dClaude,
						"utilization": 1.0,
						"reset_at":    probeReset.Format(time.RFC3339),
					},
				},
			},
		},
	}

	limit, ok := mirasimWindowLimitFromBody(acc, []byte(mirasimCreditExhausted7dClaudeBody), now)
	require.True(t, ok)
	require.Equal(t, MirasimWindow7dClaude, limit.window)
	require.Equal(t, mirasimClaude7dRateLimitKey, limit.scope,
		"必须落到 7d_claude 的 scope —— 写成账号级全局标量会连坐 fable，"+
			"而那与 mirasim_scheduling.go 顶部写明的设计意图相反")
	require.WithinDuration(t, probeReset, limit.resetAt, time.Second,
		"有探测快照时必须用上游给的 reset 时间，而不是兜底常量")
}

func TestMirasimWindowLimitFromBodyFallsBackToHourScaleNotSeconds(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	// 没有探测快照 —— 只知道"哪个窗口满了"，不知道"什么时候恢复"。
	acc := &Account{ID: 135}

	limit, ok := mirasimWindowLimitFromBody(acc, []byte(mirasimCreditExhausted7dClaudeBody), now)
	require.True(t, ok, "查不到 reset 时间不该让整条冷却作废 —— 那会退回 5 秒兜底")
	require.Equal(t, mirasimClaude7dRateLimitKey, limit.scope)

	cooldown := limit.resetAt.Sub(now)
	// 这条断言的形状是刻意的:钉的是**数量级**,不是具体数字。
	// 值本身可以调，但"7 天窗口耗尽只冷却几秒"这个形态必须永远红。
	require.Greater(t, cooldown, time.Minute,
		"兜底冷却必须是分钟级以上。线上那个 5 秒兜底让耗尽的账号 5 秒后就回到候选池，"+
			"21 秒内换了 15 个账号全部 429")
	require.LessOrEqual(t, cooldown, time.Hour,
		"兜底也不该过长 —— 它只是在不知道确切 reset 时的保守值")
}

func TestMirasimWindowLimitFromBodyIgnoresStaleProbeReset(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	for name, resetAt := range map[string]time.Time{
		"已经过去的 reset（快照陈旧）": now.Add(-2 * time.Hour),
		"离谱的未来 reset":       now.Add(400 * 24 * time.Hour),
	} {
		acc := &Account{
			ID: 135,
			Extra: map[string]any{
				"mirasim_quota_probe": map[string]any{
					"status": "ok",
					"windows": []any{
						map[string]any{
							"name":        MirasimWindow7dClaude,
							"utilization": 1.0,
							"reset_at":    resetAt.Format(time.RFC3339),
						},
					},
				},
			},
		}
		limit, ok := mirasimWindowLimitFromBody(acc, []byte(mirasimCreditExhausted7dClaudeBody), now)
		require.True(t, ok, "%s: 仍应产出冷却", name)
		cooldown := limit.resetAt.Sub(now)
		require.Greater(t, cooldown, time.Minute, "%s: 应回落到兜底而不是照用", name)
		require.LessOrEqual(t, cooldown, time.Hour, "%s: 不该照用离谱的未来值", name)
	}
}
