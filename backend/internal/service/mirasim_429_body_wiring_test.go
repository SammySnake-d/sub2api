//go:build unit

package service

// 429 body 窗口解析的**接线**测试。
//
// 与 mirasim_429_body_window_test.go 的分工:
//
//	那个文件   —— 解析与组装函数本身对不对（单元）
//	这个文件   —— 那两个函数**真的会被 HandleUpstreamError 走到**（接线）
//
// 分开是因为它们的失效方式完全不同。解析函数可以完全正确，而接线那 3 行放错了
// 位置（比如放在 header 分支之后却被 early return 跳过、或者放在 mirasim 判定之外
// 波及了普通账号），单元测试一条都不会红。
//
// 这正是本轮生产验证暴露的问题：修复部署之后 40 分钟里我的新日志 0 次命中，
// 因为当时所有耗尽账号都已被冷却挡住、请求根本走不到它们 —— 生产上"没红"既不能
// 证明修复生效，也不能证明它不生效。接线只能在这里证明。

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mirasim429Account 构造一个生产同形的 mirasim 账号:
// platform=anthropic + credentials.provider=mirasim + type=apikey。
// 三者缺一 IsMirasimAccount 就不成立，而那会让整个 mirasim 分支被跳过。
func mirasim429Account(id int64, probeWindows []any) *Account {
	acc := &Account{
		ID:       id,
		Type:     AccountTypeAPIKey,
		Platform: PlatformAnthropic,
		Credentials: map[string]any{
			"provider": "mirasim",
		},
	}
	if probeWindows != nil {
		acc.Extra = map[string]any{
			"mirasim_quota_probe": map[string]any{
				"status":  "ok",
				"windows": probeWindows,
			},
		}
	}
	return acc
}

// TestHandleUpstreamError_Mirasim429BodyWritesWindowScopedCooldown 是主接线断言。
//
// 生产实录的形态:429，**没有任何 anthropic-ratelimit-unified-* 头**（mirasim 不发
// 这组头），窗口信息只在 body 的 error.code 里。修复前这条路必然落到 5 秒的账号级
// 兜底；修复后必须落到 7d_claude 的 scope 冷却上。
func TestHandleUpstreamError_Mirasim429BodyWritesWindowScopedCooldown(t *testing.T) {
	probeReset := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	acc := mirasim429Account(135, []any{
		map[string]any{
			"name":        MirasimWindow7dClaude,
			"utilization": 1.0,
			"reset_at":    probeReset.Format(time.RFC3339),
		},
	})

	repo := &anthropicWindowLimitRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)

	svc.HandleUpstreamError(
		context.Background(),
		acc,
		http.StatusTooManyRequests,
		http.Header{}, // 刻意为空：mirasim 不发 unified-ratelimit 头，这是生产的真实形态
		[]byte(mirasimCreditExhausted7dClaudeBody),
	)

	require.Equal(t, 1, repo.modelRateLimitCalls,
		"429 body 里已经写明 credit_exhausted_7d_claude，必须写一条 per-window 冷却。"+
			"调用数为 0 说明接线没走到（那 3 行放错了位置或被 early return 跳过）")
	require.Equal(t, mirasimClaude7dRateLimitKey, repo.lastModelRateLimitScope,
		"必须写到 7d_claude 的 scope")
	require.WithinDuration(t, probeReset, repo.lastModelRateLimitReset, time.Second,
		"reset 必须取自 /v1/limits 探测快照（上游给的真实时间），不是兜底常量")

	// 这条是本文件最重要的一条。修复前的行为就是走 SetRateLimited 写 5 秒**账号级**
	// 冷却，而那会连坐 fable：一个只是 claude 耗尽的号，在那 5 秒里 fable 请求也
	// 调度不到它 —— 与 mirasim_scheduling.go 顶部写明的设计意图直接相反。
	require.Zero(t, repo.rateLimitCalls,
		"不得再写账号级全局冷却。写了就意味着 5 秒兜底那条路仍在生效，"+
			"或者两条路同时生效把 fable 也连坐了")
}

// TestHandleUpstreamError_Mirasim429HeaderStillWins 是优先级断言。
//
// header 是更权威的来源（它带 reset 与 utilization，body 只有一个 code）。
// body 这条新路只能是**兜底**，不能抢在 header 前面 —— 否则等于用一个更弱的信号
// 覆盖更强的信号。
func TestHandleUpstreamError_Mirasim429HeaderStillWins(t *testing.T) {
	headerReset := time.Now().Add(5 * time.Hour).Truncate(time.Second)
	probeReset := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1.0")
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(headerReset.Unix(), 10))

	acc := mirasim429Account(136, []any{
		map[string]any{
			"name":        MirasimWindow7dClaude,
			"utilization": 1.0,
			"reset_at":    probeReset.Format(time.RFC3339),
		},
	})

	repo := &anthropicWindowLimitRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)

	// header 说 5h 被拒，body 说 7d_claude 耗尽。header 更权威，应该按 5h 处理。
	svc.HandleUpstreamError(
		context.Background(),
		acc,
		http.StatusTooManyRequests,
		headers,
		[]byte(mirasimCreditExhausted7dClaudeBody),
	)

	require.NotEqual(t, mirasimClaude7dRateLimitKey, repo.lastModelRateLimitScope,
		"header 指明了 5h，body 这条兜底路不该抢先把冷却写到 7d_claude 上 —— "+
			"那是用更弱的信号覆盖更强的信号")
}

// TestHandleUpstreamError_Mirasim429WithoutBodyCodeStillFallsBack 是差分阴性。
//
// 没有这一条，把接线写成"429 就无条件写 7d_claude 冷却"也能让上面的主断言全绿，
// 而那会把每一个 429（包括并发过高、上游过载这类与额度无关的）都错误地冷却成
// 天级窗口耗尽 —— 那比原来的 5 秒兜底危害大得多。
func TestHandleUpstreamError_Mirasim429WithoutBodyCodeStillFallsBack(t *testing.T) {
	acc := mirasim429Account(137, nil)

	repo := &anthropicWindowLimitRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)

	svc.HandleUpstreamError(
		context.Background(),
		acc,
		http.StatusTooManyRequests,
		http.Header{},
		// 一个不带 credit_exhausted_* 的 429：与额度无关，不该被当成窗口耗尽
		[]byte(`{"error":{"type":"rate_limit_error","message":"too many concurrent requests"},"type":"error"}`),
	)

	require.Zero(t, repo.modelRateLimitCalls,
		"body 里没有 credit_exhausted_* 时不得凭空写窗口冷却 —— "+
			"把并发过载当成天级额度耗尽，比原来的 5 秒兜底危害大得多")
	require.Equal(t, 1, repo.rateLimitCalls,
		"这种 429 应该继续走原来的秒级兜底，那条路不该被这次修改拆掉")
}

// TestHandleUpstreamError_NonMirasim429BodyUntouched 是第二条差分阴性。
//
// credit_exhausted_* 是 mirasim 的方言。一个普通 anthropic 账号即使碰巧收到形状
// 相似的 body，也不该被写 mirasim 的 scope 冷却 —— 那些 scope key 只有 mirasim
// 的调度判据会读，写到别的账号上是一条永远不会被消费的死数据。
func TestHandleUpstreamError_NonMirasim429BodyUntouched(t *testing.T) {
	acc := &Account{
		ID:          138,
		Type:        AccountTypeOAuth,
		Platform:    PlatformAnthropic,
		Credentials: map[string]any{}, // 没有 provider=mirasim
	}

	repo := &anthropicWindowLimitRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)

	svc.HandleUpstreamError(
		context.Background(),
		acc,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(mirasimCreditExhausted7dClaudeBody),
	)

	require.NotEqual(t, mirasimClaude7dRateLimitKey, repo.lastModelRateLimitScope,
		"非 mirasim 账号不得被写 mirasim 的窗口 scope —— 那是只有 mirasim 调度判据"+
			"才会读的 key，写到别处是永远不会被消费的死数据")
}
