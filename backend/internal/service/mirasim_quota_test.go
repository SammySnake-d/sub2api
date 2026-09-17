//go:build unit

package service

// The four-layer quota window model for mirasim accounts.
//
// Each mirasim account carries four windows: two global (5h, 7d) and two per
// model family (7d_claude, 7d_fable). A request is schedulable only when EVERY
// window it consumes is clear.
//
// 两个家族窗口**不是**并列的（2026-09-17 生产实证更正）：claude ⊇ fable。
//   7d_claude 耗尽 → claude 全家停，fable 也停
//   7d_fable  耗尽 → 只有 fable 停
// 所以 fable 请求消耗四个窗口，非 fable 的 claude 模型消耗三个。
//
// Storage is split by nature, not by convenience: the global windows are
// account-wide so they live on the account-level scalar, the family windows are
// partial so they live in per-(account, scope) model rate limits. Both are read
// back by the same IsSchedulableForModelWithContext, which is what makes the AND
// hold without any new evaluation code.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mirasimSetWindowCooldown puts one window into cooldown, writing it exactly
// where production writes it.
func mirasimSetWindowCooldown(t *testing.T, account *Account, window string, resetAt time.Time) {
	t.Helper()
	scope, known := mirasimWindowScope(window)
	require.Truef(t, known, "unknown mirasim window %q", window)
	if scope == "" {
		reset := resetAt
		account.RateLimitResetAt = &reset
		return
	}
	setAccountModelRateLimitSnapshot(account, scope, resetAt, mirasimWindowReason(window), time.Now())
}

func TestMirasimOpusSchedulabilityIsTheAndOfItsThreeWindows(t *testing.T) {
	// [[cov:QU:opus-window-and]]
	ctx := context.Background()
	const model = "claude-opus-4-6"
	future := time.Now().Add(4 * time.Hour)

	// An opus request draws on 5h, 7d and 7d_claude — and on nothing else.
	// 写成一行不是排版偏好：判据的窗口只回看 2 行，跨行写时 `require.Equal(t,`
	// 那一行里一个被测符号都没有，这条真实的集合比较会被判成「全字面量断言」。
	require.Equal(t, []string{MirasimWindow5h, MirasimWindow7d, MirasimWindow7dClaude}, MirasimWindowsForModel(model), "opus 请求消耗的窗口集合变了 —— 调度的 AND 条件会跟着错")

	// All four clear → schedulable.
	clear := mirasimTestAccount()
	require.True(t, clear.IsSchedulableForModelWithContext(ctx, model))

	// Any ONE of the three consumed windows alone makes it unschedulable.
	// (5h and 7d deliberately share the account-level scalar, so those two
	// iterations exercise the same storage through different window ids.)
	for _, window := range MirasimWindowsForModel(model) {
		account := mirasimTestAccount()
		mirasimSetWindowCooldown(t, account, window, future)
		require.Falsef(t, account.IsSchedulableForModelWithContext(ctx, model),
			"window %s must block %s", window, model)
	}

	// The window it does NOT consume must not block it.
	unrelated := mirasimTestAccount()
	mirasimSetWindowCooldown(t, unrelated, MirasimWindow7dFable, future)
	require.True(t, unrelated.IsSchedulableForModelWithContext(ctx, model))
}

func TestMirasimFableSchedulabilityIsTheAndOfItsFourWindows(t *testing.T) {
	// [[cov:QU:fable-window-and]]
	ctx := context.Background()
	const model = "claude-fable-5"
	future := time.Now().Add(4 * time.Hour)

	// 2026-09-17：由三窗口改为四窗口。claude ⊇ fable —— 7d_claude 用满时 fable 也
	// 用不了（生产实证：上游对 claude-fable-5-1 回 429「已用满 7d_claude 用量上限」）。
	// 这条测试原本还有一句相反的尾巴（"7d_claude 冷却与 fable 无关"），那正是缺陷本体。
	//
	// 写成一行不是排版偏好：判据的窗口只回看 2 行，跨行写时 `require.Equal(t,`
	// 那一行里一个被测符号都没有，这条真实的集合比较会被判成「全字面量断言」。
	require.Equal(t, []string{MirasimWindow5h, MirasimWindow7d, MirasimWindow7dClaude, MirasimWindow7dFable}, MirasimWindowsForModel(model), "fable 请求消耗的窗口集合变了 —— 调度的 AND 条件会跟着错")

	clear := mirasimTestAccount()
	require.True(t, clear.IsSchedulableForModelWithContext(ctx, model))

	// 四个窗口任意一个被冷却都必须挡住 fable —— 包含 7d_claude。
	for _, window := range MirasimWindowsForModel(model) {
		account := mirasimTestAccount()
		mirasimSetWindowCooldown(t, account, window, future)
		require.Falsef(t, account.IsSchedulableForModelWithContext(ctx, model),
			"window %s must block %s", window, model)
	}

	// 反向不对称：fable 窗口耗尽只挡 fable，不挡其它 claude 模型。
	// 没有这条，把两个家族都返回两个 scope 也能让上面全绿，而那会让一个 fable
	// 耗尽的号连 opus 都跑不了。
	fableOnly := mirasimTestAccount()
	mirasimSetWindowCooldown(t, fableOnly, MirasimWindow7dFable, future)
	require.True(t, fableOnly.IsSchedulableForModelWithContext(ctx, "claude-opus-4-6"))
}

func TestMirasimExhaustedClaudeWindowAlsoBlocksFable(t *testing.T) {
	// [[cov:QU:family-isolation]]
	ctx := context.Background()
	account := mirasimTestAccount()
	mirasimSetWindowCooldown(t, account, MirasimWindow7dClaude, time.Now().Add(48*time.Hour))

	// The account as a whole is untouched — only the family window is parked.
	require.True(t, account.IsSchedulable())
	for _, claudeModel := range []string{"claude-opus-4-6", "claude-sonnet-4-5", "claude-haiku-4-5"} {
		require.Falsef(t, account.IsSchedulableForModelWithContext(ctx, claudeModel),
			"%s must stop while 7d_claude is exhausted", claudeModel)
	}
	// ★ 2026-09-17 语义更正：claude ⊇ fable，不是两个并列家族。
	//
	// 这两条原本断言的是相反的事（7d_claude 耗尽时 fable 仍可调度），那正是缺陷本体：
	// 生产上 `claude-fable-5-1` 打到 7d_claude 已耗尽、7d_fable 干净的号上，上游回
	// 429「已用满 7d_claude 用量上限」。调度器据旧语义认为这些号对 fable 可用，
	// 一路选中一路撞 429，而池里 70 个 7d_claude 干净的号一次都没被试过。
	require.Falsef(t, account.IsSchedulableForModelWithContext(ctx, "claude-fable-5"),
		"7d_claude 耗尽时 fable 也必须停 —— fable 的额度包含在 claude 窗口里")
	require.False(t, account.IsSchedulableForModelWithContext(ctx, "claude-fable-5[1m]"))
	// 包含关系是结构性的：fable 请求同时受两个 scope 约束。
	require.Contains(t, mirasimFamilyScopes("claude-fable-5"), mirasimClaude7dRateLimitKey)
	require.Contains(t, mirasimFamilyScopes("claude-fable-5"), mirasimFable7dRateLimitKey)
	// 反向不成立：非 fable 的 claude 模型不受 7d_fable 约束。
	require.NotContains(t, mirasimFamilyScopes("claude-opus-4-6"), mirasimFable7dRateLimitKey)
}

// 反向对照：7d_fable 耗尽只挡 fable，其余 claude 模型照常可调度。
// 少了这条，把 mirasimFamilyScopes 写成「两个家族都返回两个 scope」也能让上面全绿，
// 而那会让一个 fable 耗尽的号连 opus 都不能跑。
func TestMirasimFableWindowExhaustionDoesNotBlockOtherClaudeModels(t *testing.T) {
	ctx := context.Background()
	account := mirasimTestAccount()
	mirasimSetWindowCooldown(t, account, MirasimWindow7dFable, time.Now().Add(48*time.Hour))

	require.False(t, account.IsSchedulableForModelWithContext(ctx, "claude-fable-5"),
		"7d_fable 耗尽必须挡住 fable")
	for _, claudeModel := range []string{"claude-opus-4-6", "claude-sonnet-4-5", "claude-haiku-4-5"} {
		require.Truef(t, account.IsSchedulableForModelWithContext(ctx, claudeModel),
			"%s 不受 7d_fable 约束，必须仍可调度", claudeModel)
	}
}

func TestMirasimOneQuotaStatePerUpstreamEntity(t *testing.T) {
	// [[cov:QU:single-quota-state]]
	ctx := context.Background()
	account := mirasimTestAccount()

	// Every claude-family model resolves to the SAME scope key, so the upstream
	// entity's 7d_claude allowance is tracked once — not once per model alias.
	claudeScopes := mirasimFamilyScopes("claude-opus-4-6")
	require.Equal(t, claudeScopes, mirasimFamilyScopes("claude-sonnet-4-5"))
	require.Equal(t, claudeScopes, mirasimFamilyScopes("claude-haiku-4-5"))
	require.Equal(t, mirasimFamilyScopes("claude-fable-5"), mirasimFamilyScopes("claude-fable-5[1m]"))

	// One write, and the whole family is parked — no second piece of state has to
	// be produced for sonnet or haiku.
	mirasimSetWindowCooldown(t, account, MirasimWindow7dClaude, time.Now().Add(24*time.Hour))
	stored, ok := account.Extra[modelRateLimitsKey].(map[string]any)
	require.True(t, ok)
	require.Len(t, stored, 1, "one upstream entity, one quota state per window")
	require.Contains(t, stored, mirasimClaude7dRateLimitKey)
	require.False(t, account.IsSchedulableForModelWithContext(ctx, "claude-sonnet-4-5"))
	require.False(t, account.IsSchedulableForModelWithContext(ctx, "claude-haiku-4-5"))

	// And the remaining time every claude model reads back is that one state.
	opusRemaining := account.GetModelRateLimitRemainingTimeWithContext(ctx, "claude-opus-4-6")
	haikuRemaining := account.GetModelRateLimitRemainingTimeWithContext(ctx, "claude-haiku-4-5")
	require.InDelta(t, opusRemaining.Seconds(), haikuRemaining.Seconds(), 2)
	require.Greater(t, opusRemaining, time.Duration(0))
}
