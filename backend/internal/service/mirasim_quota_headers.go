package service

// 额度窗口响应头的观测口。
//
// 为什么需要它：sub2api 对 Anthropic 额度窗口的解析（5h / 7d / 7d_oi）是针对
// **Anthropic 官方上游**实测出来的。mirasim 是一个独立上游，它是否逐字沿用同一套
// 响应头名，在本仓里一直是**推断**而非实测 —— 而调度策略（哪个模型族因为哪个窗口
// 耗尽而不可调度）完全建立在这些头名之上。头名一旦对不上，表现不是报错，是
// **静默失效**：窗口永远读不到，账号永远"看起来有额度"，429 只能靠事后补救。
//
// 所以这里留一个只读观测口，默认关闭，开启后把上游回来的所有 anthropic-ratelimit-*
// 头原样记一条日志。它不改变任何调度行为，只是把"我们以为的头名"和"实际收到的头名"
// 摆到一起，让推断可以被证实或证伪。
//
// 开启方式：SUB2API_DEBUG_RATELIMIT_HEADERS=1
//
// 安全性：只记 anthropic-ratelimit- 前缀的头。该前缀下没有任何凭据类字段
//（authorization / x-api-key / cookie / x-mirasim-* 一律不在前缀内），
// 所以这条日志不会泄露密钥。

import (
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// rateLimitHeaderPrefix 是被观测的头名前缀（小写）。
const rateLimitHeaderPrefix = "anthropic-ratelimit-"

var debugRateLimitHeaders atomic.Bool

func init() {
	debugRateLimitHeaders.Store(parseDebugEnvBool(os.Getenv("SUB2API_DEBUG_RATELIMIT_HEADERS")))
}

// SetDebugRateLimitHeaders 供测试与运行时开关使用。
func SetDebugRateLimitHeaders(enabled bool) { debugRateLimitHeaders.Store(enabled) }

// collectRateLimitHeaders 返回 headers 里所有 anthropic-ratelimit-* 头，
// 按头名字典序排列成 "name=value" 切片。
//
// 排序不是为了好看：这条日志的用途是把多次观测拿来**逐字比对**，
// Go 的 http.Header 是 map，不排序的话同一组头每次打印顺序都不同，
// 比对会退化成人眼找茬。
func collectRateLimitHeaders(headers http.Header) []string {
	if headers == nil {
		return nil
	}
	out := make([]string, 0, 8)
	for name, values := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, rateLimitHeaderPrefix) {
			continue
		}
		for _, v := range values {
			out = append(out, lower+"="+v)
		}
	}
	sort.Strings(out)
	return out
}

// logObservedRateLimitHeaders 记录一次上游响应里出现的额度窗口头。
//
// 刻意**不**在没有任何额度头时静默返回：一条 "observed=[]" 的记录恰恰是最有价值的
// 观测结果 —— 它说明这个上游根本不走响应头通道，窗口只能靠 /v1/limits 探测拿。
// 静默返回会让这种情况和"功能没开"长得一模一样。
func logObservedRateLimitHeaders(account *Account, headers http.Header) {
	if !debugRateLimitHeaders.Load() || account == nil {
		return
	}
	observed := collectRateLimitHeaders(headers)
	slog.Info("ratelimit_headers_observed",
		"account_id", account.ID,
		"platform", account.Platform,
		"is_mirasim", IsMirasimAccount(account),
		"count", len(observed),
		"observed", observed,
	)
}

// hasAnyUnifiedRateLimitHeader 报告响应里是否带了任意一个 unified 额度窗口头。
//
// 它存在的唯一理由是给 UpdateSessionWindow 一个比
// "anthropic-ratelimit-unified-5h-status 是否存在" 更准的早退判据：
// 实测 mirasim 从不发 -status，但每次 200 都发 5h/7d 的 -utilization 与 -reset。
// 用 -status 当"这个上游有没有额度信息"的代理指标，会把一个信息量完整的响应
// 判成"什么都没有"。
//
// 前缀取 unified- 这一层（而不是更宽的 anthropic-ratelimit-），是因为只有
// unified 家族是窗口参数化的、下游解析得动；别的 anthropic-ratelimit-* 头
// 进来也推不出窗口，不该让它们把早退撑开。
func hasAnyUnifiedRateLimitHeader(headers http.Header) bool {
	if headers == nil {
		return false
	}
	const unifiedPrefix = rateLimitHeaderPrefix + "unified-"
	for name := range headers {
		if strings.HasPrefix(strings.ToLower(name), unifiedPrefix) {
			return true
		}
	}
	return false
}
