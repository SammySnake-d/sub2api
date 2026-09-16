//go:build unit

package service

// The four-layer quota window model for mirasim accounts.
//
// Each mirasim account carries four independent windows: two global (5h, 7d)
// and two per model family (7d_claude, 7d_fable). A request is schedulable only
// when EVERY window it consumes is clear, and exhausting one family's window
// must leave the other family fully served by the same account.
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

func TestMirasimFableSchedulabilityIsTheAndOfItsThreeWindows(t *testing.T) {
	// [[cov:QU:fable-window-and]]
	ctx := context.Background()
	const model = "claude-fable-5"
	future := time.Now().Add(4 * time.Hour)

	// 写成一行不是排版偏好：判据的窗口只回看 2 行，跨行写时 `require.Equal(t,`
	// 那一行里一个被测符号都没有，这条真实的集合比较会被判成「全字面量断言」。
	require.Equal(t, []string{MirasimWindow5h, MirasimWindow7d, MirasimWindow7dFable}, MirasimWindowsForModel(model), "fable 请求消耗的窗口集合变了 —— 调度的 AND 条件会跟着错")

	clear := mirasimTestAccount()
	require.True(t, clear.IsSchedulableForModelWithContext(ctx, model))

	for _, window := range MirasimWindowsForModel(model) {
		account := mirasimTestAccount()
		mirasimSetWindowCooldown(t, account, window, future)
		require.Falsef(t, account.IsSchedulableForModelWithContext(ctx, model),
			"window %s must block %s", window, model)
	}

	unrelated := mirasimTestAccount()
	mirasimSetWindowCooldown(t, unrelated, MirasimWindow7dClaude, future)
	require.True(t, unrelated.IsSchedulableForModelWithContext(ctx, model))
}

func TestMirasimExhaustedClaudeWindowLeavesFableSchedulable(t *testing.T) {
	// [[cov:QU:family-isolation]]
	ctx := context.Background()
	account := mirasimTestAccount()
	mirasimSetWindowCooldown(t, account, MirasimWindow7dClaude, time.Now().Add(48*time.Hour))

	// The account as a whole is untouched — only one family is parked.
	require.True(t, account.IsSchedulable())
	for _, claudeModel := range []string{"claude-opus-4-6", "claude-sonnet-4-5", "claude-haiku-4-5"} {
		require.Falsef(t, account.IsSchedulableForModelWithContext(ctx, claudeModel),
			"%s must stop while 7d_claude is exhausted", claudeModel)
	}
	require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-fable-5"))
	require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-fable-5[1m]"))
	// The isolation is structural: the two families never share a scope key.
	require.NotEqual(t, mirasimFamilyScope("claude-opus-4-6"), mirasimFamilyScope("claude-fable-5"))
}

func TestMirasimOneQuotaStatePerUpstreamEntity(t *testing.T) {
	// [[cov:QU:single-quota-state]]
	ctx := context.Background()
	account := mirasimTestAccount()

	// Every claude-family model resolves to the SAME scope key, so the upstream
	// entity's 7d_claude allowance is tracked once — not once per model alias.
	claudeScope := mirasimFamilyScope("claude-opus-4-6")
	require.Equal(t, claudeScope, mirasimFamilyScope("claude-sonnet-4-5"))
	require.Equal(t, claudeScope, mirasimFamilyScope("claude-haiku-4-5"))
	require.Equal(t, mirasimFamilyScope("claude-fable-5"), mirasimFamilyScope("claude-fable-5[1m]"))

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
