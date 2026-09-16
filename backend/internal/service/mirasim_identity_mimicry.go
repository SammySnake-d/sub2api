package service

// mirasim 通道的 Claude Code 身份改造。
//
// # 为什么需要它
//
// mirasim 上游按客户端身份收货：请求体里没有 Claude Code 形态的 system blocks 就拒收。
// sub2api 此前只有分组的 `claude_code_only` 应对这件事 —— 那是**提前拒绝**，不是改造：
// 非 CC 客户端在选号阶段就被 `gateway_scheduling.go:115 checkClaudeCodeRestriction`
// 挡掉，生产上一小时内产生 180 条 503。本文件补上"改造"这条路。
//
// **前置条件写清楚：本文件不动那道分组闸。** `resolveGatewayGroup` 的
// `if !group.ClaudeCodeOnly || IsClaudeCodeClient(ctx)` 原样保留，且跑在选号第一步、
// 早于转发。所以改造只对**已经关掉 claude_code_only 的分组**生效；开着限制的分组
// 仍然在选号阶段拒绝非 CC 客户端。这不是遗漏而是下面「互斥是结构性的」那一节要的
// 语义：两者互斥，由运维选一个，而不是同时生效。要让某个分组用改造，就得先把它的
// claude_code_only 关掉 —— 这一步是运维动作，代码里没有、也不该有自动关闭。

//
// # 与 OAuth 通道的关系：复用，不重写
//
// 改造所需的全部机器 sub2api 早就有了，只是被锁在 OAuth 通道上：
//
//	gateway_forward.go:196  shouldMimicClaudeCode := account.IsOAuth() && !isClaudeCode
//	gateway_forward.go:205  claudeOAuthSystemPromptInjectionSettings(ctx)
//	gateway_forward.go:207  rewriteSystemForNonClaudeCodeWithPromptBlocks(...)
//
// 而 mirasim 账号是 `type=apikey` + passthrough，在 `gateway_forward.go:113` 就提前
// return 了，连 :196 都走不到 —— 两道门都把它挡在外面。所以这里做的是**接线**：
// 调同一个 rewriter、读同一份 UI 配置（管理后台「Claude OAuth System 注入」那组
// System Blocks），不另起一套注入实现。
//
// 只取身份注入这一段，不搬整个 :196-251 块：那里面的 normalizeClaudeOAuthRequestBody
// 是 OAuth 专属的（metadata.user_id 重写、OAuth 指纹），对 apikey 通道既不需要也不正确。
//
// # 互斥是结构性的，不是配出来的
//
// 用户要的「开了 CC 客户端限制，注入就该是关的」不需要第二个开关，也不可能配矛盾：
// `checkClaudeCodeRestriction` 是 `SelectAccountWithLoadAwareness` 的第一件事
// (`gateway_scheduling.go:115`)，而选号发生在转发之前。于是：
//
//	分组开了 claude_code_only → 非 CC 在选号阶段就被拒/降级
//	                          → 到达转发路径的要么是真 CC、要么属于不限制的分组
//	                          → 这里只需判 !isClaudeCode
//
// 双保险：即使分组限制被关掉，isClaudeCode 判定仍然保护真 CC 客户端不被改写。
// 那条保护是有代价依据的（gateway_forward.go:173-177）：真 CC 的长 system prompt 被
// 替换成 ~45 tokens 的短 prompt 会低于 Anthropic 1024 token 的最低缓存门槛，
// 导致系统级缓存整个失效。
//
// # 缓存安全
//
// Anthropic prompt cache 是 tools → system → messages 的线性前缀匹配，从第一个不同的
// 字节起后面全失效。注入之所以安全，靠的是**确定性**：
//
//   - 注入内容来自固定配置，不含 per-request 变量
//   - 唯一的变量是 billing block 里的 cc_version 指纹，而
//     `computeClaudeCodeFingerprint` 取的是**第一条 user 消息**的第 4/7/20 字符
//     (gateway_billing_block.go)，一个会话里那条消息永不改变 → 指纹会话内恒定
//   - 客户端原有 system 搬进 messages 时 `cache_control` 原样带走，不拍平
//
// 代价是每个在飞会话**一次性**的前缀重建，不是持续的 miss。
// mirasim_cache_prefix_test.go 文件头记的那次 8%→0% 灾难是**另一个网关**的旧实现，
// 它把 system 拍平成字符串丢了 cache_control —— 那是本路径明确不做的事。

import (
	"context"

	"github.com/gin-gonic/gin"
)

// requestLooksLikeClaudeCode 是「这个请求是不是真 Claude Code 客户端发的」的**唯一**判据。
//
// 抽出来是因为它现在有两个消费者（OAuth 通道与 mirasim 通道）。两边各写一份的话，
// 判据一旦漂移就会出现"一条通道认它是 CC、另一条不认"的分裂状态，而两种误判的代价
// 都很贵：误判为 CC → 上游拒收；误判为非 CC → 真 CC 的缓存被改写打掉。
//
// 三级判定，任一成立即为真：
//  1. 上游中间件已经认定（IsClaudeCodeClient(ctx)）
//  2. UA 形如 claude-cli/* 且带 metadata.user_id
//  3. 被代理的真实 CC 流量：UA 已被改写成 Go-http-client 之类，但 body 仍带
//     billing attribution block。少了这一级，经 new-api 转发进来的真 CC 会被当成
//     第三方客户端重写 system，缓存前缀每轮全毁。
func (s *GatewayService) requestLooksLikeClaudeCode(ctx context.Context, c *gin.Context, parsed *ParsedRequest, body []byte) bool {
	if parsed == nil {
		return false
	}
	var clientUserAgent string
	if c != nil {
		clientUserAgent = c.GetHeader("User-Agent")
	}
	// ctx 是调用方的请求 context，不是 c.Request.Context() —— 两者在中间件改写过
	// context 之后可能不同，而 IsClaudeCodeClient 读的标记是写在前者上的。
	if IsClaudeCodeClient(ctx) || isClaudeCodeClient(clientUserAgent, parsed.MetadataUserID) {
		return true
	}
	if parsed.MetadataUserID != "" {
		return systemHasBillingAttributionBlock(body)
	}
	return false
}

// applyMirasimClaudeCodeIdentity 把非 Claude Code 客户端的请求体改造成 Claude Code 形态，
// 使 mirasim 上游接受它。不满足条件时**原样返回传入的 body**（同一个切片，不做任何往返）。
//
// 三道门，全部成立才改写：
//
//  1. 账号是 mirasim —— 这条改造是为 mirasim 的收货规则做的，不该波及别的 anthropic 账号
//  2. 请求不是真 CC —— 真 CC 自带完整 system prompt 与 cache_control 断点，
//     重写它等于把长 prompt 换成 ~45 tokens 的短 prompt，低于 Anthropic 1024 token
//     的最低缓存门槛，缓存整个失效（gateway_forward.go:173-177）
//  3. 注入开关打开 —— 复用管理后台「Claude OAuth System 注入」那一组配置
//
// 第 2 道门同时就是用户要的「开了分组 CC 限制则不注入」：开了限制的分组，非 CC 在
// `SelectAccountWithLoadAwareness` 第一步（gateway_scheduling.go:115）就被拒/降级了，
// 能走到这里的必然是真 CC，于是这道门自动为假。不需要第二个开关，也配不出矛盾。
func (s *GatewayService) applyMirasimClaudeCodeIdentity(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *ParsedRequest,
	body []byte,
) []byte {
	if s == nil || account == nil || parsed == nil || len(body) == 0 {
		return body
	}
	if !IsMirasimAccount(account) {
		return body
	}
	if s.requestLooksLikeClaudeCode(ctx, c, parsed, body) {
		return body
	}
	enabled, systemPrompt, systemPromptBlocks := s.claudeOAuthSystemPromptInjectionSettings(ctx)
	if !enabled {
		return body
	}
	systemRaw, _ := parsed.SystemValue()
	rewritten := rewriteSystemForNonClaudeCodeWithPromptBlocks(body, systemRaw, systemPrompt, systemPromptBlocks)
	if len(rewritten) == 0 {
		return body
	}
	// 注入会新增一个 cache_control 断点：3-block 形态里的"扩充提示词"块自带一个，
	// 作为稳定缓存锚点（见 rewriteSystemForNonClaudeCodeWithPromptBlocks 的注释）。
	//
	// OAuth 通道在 gateway_forward.go:258 会统一跑一次 4 断点上限守卫，但直通路径
	// 在 :113 就提前 return 了，走不到它。少了这一步，客户端本来就发满 4 个断点时
	// 注入会让它变成 5 个，被上游 400 拒绝 —— 这条是回归测试
	// TestMirasimIdentityKeepsCacheControlWithinLimit 抓出来的。
	//
	// 只在真的改写之后才跑：enforceCacheControlLimit 自己也会做 JSON 往返，
	// 对未改写的请求跑它等于平白改动缓存前缀所在的字节。
	return enforceCacheControlLimit(rewritten)
}
