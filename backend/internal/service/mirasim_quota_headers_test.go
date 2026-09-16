package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// collectRateLimitHeaders 的口径测试。它是观测口的全部逻辑，
// 观测结论（"mirasim 到底回哪些头"）的可信度直接取决于它收全没收全。
func TestCollectRateLimitHeadersIsCaseInsensitiveAndOrdered(t *testing.T) {
	h := http.Header{}
	// Go 的 http.Header.Set 会把头名规范化成 Anthropic-Ratelimit-...，
	// 真实上游发的是全小写。两种写法都必须被收到。
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.42")
	h.Set("Anthropic-RateLimit-Unified-5h-Status", "allowed")
	h["anthropic-ratelimit-unified-7d_oi-reset"] = []string{"1789000000"}

	got := collectRateLimitHeaders(h)
	require.Equal(t, []string{
		"anthropic-ratelimit-unified-5h-status=allowed",
		"anthropic-ratelimit-unified-7d-utilization=0.42",
		"anthropic-ratelimit-unified-7d_oi-reset=1789000000",
	}, got, "必须大小写无关地收全，并按头名字典序排列（多次观测要能逐字比对）")
}

// 差分阴性：非额度头一个都不能被收进来 —— 这条日志会进生产日志，
// 收错前缀就等于把无关请求头写进日志文件。
func TestCollectRateLimitHeadersExcludesEverythingElse(t *testing.T) {
	h := http.Header{}
	h.Set("authorization", "Bearer must-not-appear")
	h.Set("x-api-key", "must-not-appear")
	h.Set("x-mirasim-enc", "must-not-appear")
	h.Set("anthropic-organization-id", "must-not-appear")
	h.Set("ratelimit-remaining", "must-not-appear") // 前缀不完整
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed")

	got := collectRateLimitHeaders(h)
	require.Equal(t, []string{"anthropic-ratelimit-unified-5h-status=allowed"}, got,
		"只有 anthropic-ratelimit- 前缀的头能进日志；收多一个就是把无关头写进生产日志")
}

func TestCollectRateLimitHeadersOnEmptyIsNilNotPanic(t *testing.T) {
	require.Nil(t, collectRateLimitHeaders(nil))
	require.Empty(t, collectRateLimitHeaders(http.Header{}),
		"一个额度头都没有时返回空 —— 这本身就是有效观测结果（该上游不走响应头通道）")
}

// ---------------------------------------------------------------------------
// hasAnyUnifiedRateLimitHeader —— UpdateSessionWindow 早退判据的正/反对照
// ---------------------------------------------------------------------------

// 这是把「mirasim 的额度信息被整段丢弃」这个缺陷钉死的那条测试。
// 实测形态：只有 5h/7d 的 utilization 与 reset，没有 -status。
func TestUnifiedHeaderDetectionAcceptsMirasimObservedShape(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-reset", "1789536033")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0")
	h.Set("anthropic-ratelimit-unified-7d-reset", "1790091192")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.009351933214285714")

	require.Empty(t, h.Get("anthropic-ratelimit-unified-5h-status"),
		"前提：实测 mirasim 不发 -status；这条前提不成立时下面的断言没有意义")
	require.True(t, hasAnyUnifiedRateLimitHeader(h),
		"mirasim 每次 200 都带 5h/7d 的 utilization 与 reset —— "+
			"把它判成「没有额度信息」会让 5h 窗口边界与被动用量全部丢失，"+
			"调度器只能等 429 才知道额度耗尽")
}

// 差分阴性：真的一个 unified 头都没有时必须判 false，否则早退条件形同虚设，
// 每个响应都会往下走一遍无谓的 DB 写入。
func TestUnifiedHeaderDetectionRejectsResponsesWithoutWindows(t *testing.T) {
	h := http.Header{}
	h.Set("content-type", "application/json")
	h.Set("request-id", "req_abc")
	// 同前缀但不是 unified 家族：推不出窗口，不该把早退撑开。
	h.Set("anthropic-ratelimit-requests-remaining", "42")

	require.False(t, hasAnyUnifiedRateLimitHeader(h),
		"只有非 unified 家族的额度头时必须判 false —— 它们推不出窗口")
	require.False(t, hasAnyUnifiedRateLimitHeader(http.Header{}))
	require.False(t, hasAnyUnifiedRateLimitHeader(nil))
}

// 官方 Anthropic 形态（带 -status）当然也要判 true，否则这次改动会让原有上游回归。
func TestUnifiedHeaderDetectionStillAcceptsOfficialAnthropicShape(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1789536033")
	require.True(t, hasAnyUnifiedRateLimitHeader(h))
}
