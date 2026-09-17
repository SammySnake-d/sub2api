package service

// mirasim 的「探活请求」本地拦截。
//
// # 上游的规则(2026-09-17 三臂实测确定，不是推断)
//
// mirasim 把「输出上限 ≤ 1 token」的请求读成探活，直接 400：
//
//	this request asks for at most one token of output and carries no session,
//	so it is read as an availability probe rather than work. Use GET /v1/limits
//	to check availability; it costs no upstream call and is not rate limited per model
//
// 实测三臂（法国生产实例，claude-sonnet-5，走真实账号）：
//
//	max_tokens=1 + 无 metadata          → 400
//	max_tokens=1 + 合法 metadata.user_id → **仍然 400**
//	max_tokens=2 + 无 metadata          → 200
//
// 第二臂是关键：文案里那句 "carries no session" 会让人以为「补上 session 就能过」，
// 实测证伪 —— 带了完整合法的 metadata.user_id 照样被拒。所以真正起作用的门槛只有
// max_tokens ≤ 1 这一条，没有任何我们能补的东西可以让它成功。
//
// # 为什么要在本地拦
//
// 既然这类请求在上游 100% 不可能成功，转上去只有两个后果，都是纯损失：
//
//  1. 白烧一次真实的上游往返（还占一次调度、一条代理出口链路）；
//  2. 把账号往「探活器」这个特征上推。mirasim 明确在看这个形状 —— 它专门为此
//     写了一句错误文案，并在里面推荐改用 GET /v1/limits。我们自己的账号激活流程
//     （ma-relay activate.go）也早就因为同一条规则刻意避开 max_tokens:1。
//
// 实测暴露面（2026-09-17，容器重启后约 1 小时）：19 次这类请求，全部来自同一个
// 客户 key，集中打在 2 个账号上（mirasim-112 ×17、mirasim-137 ×2），
// 占同期 /v1/messages 总量的约 28%。
//
// # 为什么不改写 max_tokens 让它成功
//
// 把 1 改成 2 确实能让它返回 200（第三臂已证），但那是偷改客户声明的参数：
// max_tokens 是调用方的契约，一个真的只要 1 个 token 的调用方会拿到 2 个。
// 用一个静默的语义违规去换一条探测请求的成功，不划算。
//
// # 为什么不本地合成一个假的 200
//
// 那会向客户报告一次从未发生的模型调用。探活场景下看着无害，但只要有一个调用方
// 真的在读那 1 个 token 的内容，我们返回的就是编造的。

import (
	"github.com/tidwall/gjson"
)

// mirasimAvailabilityProbeMaxTokens 是上游判定「探活」的输出上限门槛。
// ≤ 这个值且走 mirasim 通道的请求，上游必拒。
const mirasimAvailabilityProbeMaxTokens = 1

// mirasimAvailabilityProbeMessage 是这类请求的拒绝文案。
//
// 这里原本是上游原文的逐字复刻，理由是「客户端看到的东西应与直连时完全一致」。
// 2026-09-18 那条理由被推翻：上游原文里带 "upstream" 和 "GET /v1/limits"，
// 前者暴露我们是个代理，后者是我们根本不对外暴露的端点、本身就是上游指纹。
// 客户端文案一律不许暴露上游是谁——判据焊在
// TestClientVisibleMessagesDoNotNameTheUpstream。
//
// 改写不丢信息：真正要告诉调用方的只有「为什么被拒」和「怎么改」，
// 两样都在。ma-relay 的 bad_request_reason.go 匹配的是**上游返回**的原文，
// 不经过这里，所以这次改写不影响它。
const mirasimAvailabilityProbeMessage = "Invalid request: max_tokens is at most 1, which reads as an " +
	"availability probe rather than real work, so this request was not served. " +
	"Raise max_tokens to at least 2 for a real completion."

// isMirasimAvailabilityProbe 判断这条请求会不会被 mirasim 当成探活拒掉。
//
// 只看 max_tokens：实测证明 session / metadata 那一半我们补不动（见文件头三臂实验）。
// 判据刻意保守 —— 读不到 max_tokens 就返回 false，让请求照常走上游。
// 宁可漏拦一条（代价是一次注定失败的往返，也就是今天的现状），
// 也不能误拦一条本来能成功的请求。
func isMirasimAvailabilityProbe(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	maxTokens := gjson.GetBytes(body, "max_tokens")
	if !maxTokens.Exists() || maxTokens.Type != gjson.Number {
		return false
	}
	// 非整数或负数一律不拦：那是另一类坏 body，该由上游给出它自己的判定。
	n := maxTokens.Int()
	return n >= 0 && n <= mirasimAvailabilityProbeMaxTokens
}

// MirasimAvailabilityProbeError 表示这条请求会被 mirasim 当成探活拒掉，我们在本地
// 提前用同一个 400 回掉，不发上游。
//
// 做成哨兵错误而不是在 service 里直接写响应，是照 BetaBlockedError 的既有形状
// （gateway_upstream_request.go:631）：Forward 的成功路径上，handler 会无条件
// 调用 submitForwardUsage(result) 并立刻解引用 result（gateway_handler.go:1108，
// 那里**没有判空**）。所以 service 绝不能返回 (nil, nil) —— 那会在生产上 panic。
// 走错误路径还顺带拿到两个正确行为：不触发 failover（换账号同样会被拒，
// 每次重试都是一次白烧的上游往返），也不进计费。
type MirasimAvailabilityProbeError struct {
	Message string
}

func (e *MirasimAvailabilityProbeError) Error() string { return e.Message }

// mirasimAvailabilityProbeBlock 在这条请求注定被上游判成探活时返回一个哨兵错误。
// 返回 nil 表示放行。
func mirasimAvailabilityProbeBlock(account *Account, body []byte) *MirasimAvailabilityProbeError {
	if !IsMirasimAccount(account) {
		return nil
	}
	if !isMirasimAvailabilityProbe(body) {
		return nil
	}
	return &MirasimAvailabilityProbeError{Message: mirasimAvailabilityProbeMessage}
}

// mirasimAvailabilityProbeModelHint 只用于日志，避免把整个 body 打进去。
func mirasimAvailabilityProbeModelHint(body []byte) string {
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		return "(未记录模型)"
	}
	return model
}
