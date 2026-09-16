package service

// 429 响应**体**里的窗口信息。
//
// ## 缺陷本体（生产实录，2026-09-16）
//
// mirasim 在窗口耗尽时返回 429，而"哪个窗口耗尽"只写在 body 里：
//
//	{"error":{"code":"credit_exhausted_7d_claude",
//	          "message":"已用满 7d_claude 用量上限，可切换其它模型继续
//	                     (this model family's allowance is spent; other models still work)",
//	          "type":"rate_limit_error"}}
//
// 而冷却写入这一侧此前**只读 header**（selectMirasimExhaustedWindows）。header 里没有
// 窗口信息时就落到秒级兜底，线上日志长这样：
//
//	rate_limit_429_fallback_used  account_id=135  reason=mirasim_no_window_headers  using_default=5s
//
// **5 秒对一个 7 天窗口是荒谬的**，后果是一个可观测的死循环：耗尽的账号 5 秒后回到
// 候选池 → 被选中 → 又 429 → 又只冷却 5 秒。实测一次渠道探测在 21 秒内换了 15 个账号
// 全部 429，而同一时刻 143 个账号里有 125 个的 7d_claude 用量还不到 80% —— 宽裕的号
// 一直在，只是耗尽的号因为 5 秒冷却反复插队。
//
// ## 为什么落在这里而不是扩展 header 解析
//
// persistMirasimWindowLimitSet 的注释写得很清楚：它是 source-agnostic 的写入半边，
// 存在的理由就是让不同来源（429 header / /v1/limits 快照）落进同一套存储与优先级。
// body 是**第三个来源**，正好接在同一个口上 —— 不新增第二条写路径。
//
// ## reset 时间从哪来
//
// body 只说了"哪个窗口满了"，没说"什么时候恢复"。所以这里去读该账号最近一次
// /v1/limits 探测快照里那个窗口的 reset_at —— 那是同一个上游对同一个窗口给出的
// 权威时间。探测快照缺失或过期时**不猜**，退回一个保守的小时级兜底，理由见
// mirasimBodyExhaustedFallbackCooldown。

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
)

// mirasimCreditExhaustedCodePrefix 是 mirasim 表达"某个窗口的额度用尽"的 code 前缀。
// 完整形态是 credit_exhausted_<window>，window 取值与 /v1/limits 的窗口名同一套
// （5h / 7d / 7d_claude / 7d_fable），所以可以直接喂给 mirasimWindowScope。
const mirasimCreditExhaustedCodePrefix = "credit_exhausted_"

// mirasimBodyExhaustedFallbackCooldown 是"body 说了窗口、但我们查不到 reset 时间"时的兜底。
//
// 30 分钟的依据不是精确性，而是**两侧的代价不对称**：
//   - 配短了（比如原来的 5s）→ 耗尽的账号反复插队，把整个候选池的调度搅乱，
//     而且每次插队都要赔掉一次真实的上游往返。这是线上真实发生的形态。
//   - 配长了 → 最坏是一个其实已经恢复的账号被多晾一会儿，而它的窗口是**天级**的，
//     多等 30 分钟对可用容量的影响可以忽略（143 个账号里只有十几个会同时处于这个状态）。
//
// 所以这个值刻意偏保守。它只在"探测快照也没有 reset"时才会被用到 —— 正常情况下
// reset 来自快照，是上游给的真实时间。
const mirasimBodyExhaustedFallbackCooldown = 30 * time.Minute

// mirasimExhaustedWindowFromBody 从 429 响应体里抠出耗尽的窗口名。
//
// 返回 "" 表示这个 body 没有表达窗口信息 —— 调用方据此决定要不要继续走秒级兜底。
// 刻意只认 error.code 这一个字段：message 是给人看的、会随文案调整而变，
// 用它做判据等于把调度行为绑在一句中文提示上。
func mirasimExhaustedWindowFromBody(responseBody []byte) string {
	if len(responseBody) == 0 {
		return ""
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return ""
	}
	code := strings.TrimSpace(payload.Error.Code)
	if !strings.HasPrefix(code, mirasimCreditExhaustedCodePrefix) {
		return ""
	}
	window := strings.TrimPrefix(code, mirasimCreditExhaustedCodePrefix)
	// 只接受四个已知窗口。认不出来的窗口名**不能**当成"没有窗口信息"静默忽略 ——
	// 那正是 mirasim_unrecognized_window_headers 那条日志存在的理由：上游新增了一个
	// 窗口而我们的列表没跟上，是"我们的表错了"，不是"上游没说"。
	if _, ok := mirasimWindowScope(window); !ok {
		slog.Warn("mirasim_unrecognized_window_in_429_body",
			"code", code,
			"window", window,
			"detail", "429 body named a window token that is not one of the four we model; "+
				"our window list is probably stale and a real cooldown is about to be downgraded")
		return ""
	}
	return window
}

// mirasimWindowLimitFromBody 把 body 里的窗口名 + 探测快照里的 reset 时间，
// 组装成一条可写入的冷却。
//
// ok=false 表示 body 没说窗口（此时调用方该走原来的秒级兜底）。
// ok=true 但 resetAt 来自兜底常量时，limit 仍然有效 —— "不知道确切恢复时间"不该
// 退化成"几乎不冷却"。
func mirasimWindowLimitFromBody(
	account *Account,
	responseBody []byte,
	now time.Time,
) (limit mirasimWindowLimit, ok bool) {
	window := mirasimExhaustedWindowFromBody(responseBody)
	if window == "" {
		return mirasimWindowLimit{}, false
	}
	scope, _ := mirasimWindowScope(window) // 上面已校验过 ok

	resetAt := now.Add(mirasimBodyExhaustedFallbackCooldown)
	resetSource := "fallback"

	// 优先用最近一次 /v1/limits 探测里该窗口的 reset_at：那是同一个上游对同一个
	// 窗口给出的权威时间，比任何常量都准。
	if snapshot := DecodeMirasimQuotaProbeSnapshot(account.Extra); snapshot != nil {
		for _, w := range snapshot.Windows {
			if w.Name != window || w.ResetAt == nil {
				continue
			}
			candidate := *w.ResetAt
			// 只接受"在未来、且不离谱地远"的值。过去的 reset 说明快照已经陈旧；
			// 离谱的未来值在 quota probe 那侧已经有同款校验，这里保持一致的判据，
			// 免得两条路径对同一个数字给出不同结论。
			if candidate.After(now) && !candidate.After(now.Add(mirasimQuotaMaxCooldown)) {
				resetAt = candidate
				resetSource = "quota_probe_snapshot"
			}
			break
		}
	}

	slog.Info("mirasim_429_window_from_body",
		"account_id", account.ID,
		"window", window,
		"scope", scope,
		"reset_at", resetAt.UTC().Format(time.RFC3339),
		"reset_source", resetSource,
		"detail", "429 响应体里的 credit_exhausted_* 指明了耗尽窗口；"+
			"此前这条信息被忽略，冷却退化成秒级兜底")

	return mirasimWindowLimit{window: window, scope: scope, resetAt: resetAt}, true
}

// persistMirasimWindowLimitFromBody 是接进 handleMirasimUpstreamError 的入口。
//
// 返回 true 表示冷却已按窗口写入，调用方不应再走秒级兜底。
func (s *RateLimitService) persistMirasimWindowLimitFromBody(
	ctx context.Context,
	account *Account,
	responseBody []byte,
) bool {
	if s == nil || s.accountRepo == nil || account == nil {
		return false
	}
	now := time.Now()
	limit, ok := mirasimWindowLimitFromBody(account, responseBody, now)
	if !ok {
		return false
	}
	// 复用 source-agnostic 的写入半边：同一套存储、同一套优先级规则
	// （shouldPersistAnthropicWindowLimit 不会让一个新冷却缩短一个仍在生效的旧冷却）。
	return s.persistMirasimWindowLimitSet(ctx, account, []mirasimWindowLimit{limit}, now)
}
