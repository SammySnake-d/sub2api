package service

// Anthropic prompt cache 是 tools → system → messages 的**线性前缀**匹配：
// 从第一个不同的字节起，后面全部缓存失效。所以本文件守的不是「代码整洁」，
// 而是一个已在另一网关上实测过的故障模式：
//
//	一处把 system 搬进 messages 的改写 → 8% 请求从 >90% 命中掉到 0%，
//	中位重建 87K token，最坏 470-760K token / 105-222s TTFT，
//	客户端等不住取消 → 取消丢弃 in-flight 缓存写入 → 重试从零开始
//	→ 连续 23 次 ~50s 取消的活锁。
//
// 被测路径：api_key + extra.anthropic_passthrough=true，即
// gateway_forward.go:113-131 提前 return 进入 gateway_anthropic_passthrough.go。
// 该路径上对 body 的改写只有这几处（顺序即真实调用顺序）：
//
//	:82  StripEmptyTextBlocks                       (gateway_request.go:517)
//	:87  FilterWebSearchHistoryBlocks               (gateway_websearch_block_filter.go:46)
//	:302 stripDeferredToolCacheControl              (gateway_tool_rewrite.go:309)
//	:324 sanitizeAnthropicBodyForBetaTokens         (gateway_request.go:940)
//	:330 clampOllamaCloudAnthropicMessagesMaxTokens (ollama_cloud_messages_max_tokens.go:17)
//
// 后三处都在 buildUpstreamRequestAnthropicAPIKeyPassthrough 内部，因此本文件
// 直接调真实的 build 函数取它写进 http.Request 的字节，不自造简化版重实现。
//
// 其中 StripEmptyTextBlocks 与 FilterWebSearchHistoryBlocks 的形状是
// json.Unmarshal(messages) → []any/map[string]any → json.Marshal → sjson.SetRawBytes。
// map round-trip 的实测代价：对象 key 按字母序重排、`<` `>` `&` 被 HTML-escape 成
// 6 字节的 u003c / u003e / u0026 形态、大整数经 float64 丢精度。system 段本身 0 处改写。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ============================================================================
// Fixtures：按 Claude Code 真实发包形态构造
// ============================================================================

// mirasimTestModel 是 anthropic-strict 协议族（ResolveThinkingProtocol），
// 保证 FilterWebSearchHistoryBlocks 不会对真块做额外剥离。
const mirasimTestModel = "claude-sonnet-4-6"

// tools 段：含一个 custom.defer_loading 工具（Claude Code 确实这么发）与
// 末位工具上的 cache_control 断点。deferred 工具不带 cache_control，
// 因此 stripDeferredToolCacheControl 在真实形态下应是字节 no-op。
const mirasimToolsSegment = `"tools":[` +
	`{"name":"Read","description":"Read a file from disk","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"offset":{"type":"integer"}},"required":["file_path"]}},` +
	`{"name":"Bash","description":"Run a shell command","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]},"custom":{"defer_loading":true}},` +
	`{"name":"Edit","description":"Edit a file in place","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]},"cache_control":{"type":"ephemeral","ttl":"5m"}}` +
	`]`

// system 段：数组形态 + 第二块上的 cache_control 断点（Claude Code 的标准形状）。
// 正文含 `<` / `>` / `&`，用来暴露 HTML 转义。
const mirasimSystemSegment = `"system":[` +
	`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},` +
	`{"type":"text","text":"Repo convention: prefer a < b && c > d over the verbose form.","cache_control":{"type":"ephemeral","ttl":"5m"}}` +
	`]`

// 各轮对话。key 顺序 role→content 是客户端真实顺序，且**非字母序**
// （字母序会把 content 排到 role 前面），因此足以暴露 map round-trip 的重排。
const (
	mirasimTurnUserAsk = `{"role":"user","content":[{"type":"text","text":"check whether a < b && c > d holds"}]}`

	mirasimTurnAssistantText = `{"role":"assistant","content":[{"type":"text","text":"Reading the file now."}]}`

	// 工具入参里的大整数：12345678901234567890 与 9007199254740993 都超出
	// float64 的精确整数范围，round-trip 会把它们改写成 ...567000 / ...992。
	mirasimTurnAssistantToolUse = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01AbCdEf","name":"Read","input":{"file_path":"/tmp/report.txt","offset":12345678901234567890,"limit":9007199254740993}}]}`

	// tool_result 里带一个空 text 块 —— 这是 StripEmptyTextBlocks 的触发条件，
	// 也是整条 messages 数组被 map round-trip 的唯一入口。
	mirasimTurnUserToolResultWithEmptyText = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01AbCdEf","content":[{"type":"text","text":""},{"type":"text","text":"line1 <b>bold</b> & more"}]}]}`
)

// mirasimInboundBody 按给定轮次拼出入站 body。tools / system 两段在所有轮次间
// 完全相同，因此「第 2 轮 = 第 1 轮 + 追加轮次」在构造上就是纯追加。
func mirasimInboundBody(turns ...string) []byte {
	return []byte(`{"model":"` + mirasimTestModel + `","max_tokens":8192,` +
		mirasimToolsSegment + `,` + mirasimSystemSegment + `,` +
		`"messages":[` + strings.Join(turns, ",") + `]}`)
}

// mirasimPassthroughAccount 构造命中 gateway_forward.go:113 提前 return 的账号形态：
// api_key + extra.anthropic_passthrough=true。
func mirasimPassthroughAccount() *Account {
	return &Account{
		ID:          9001,
		Name:        "mirasim-cache-prefix-test",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "upstream-key"},
		Extra:       map[string]any{"anthropic_passthrough": true},
		Status:      StatusActive,
		Schedulable: true,
	}
}

// mirasimOutboundBody 跑完整条 passthrough 改写链，返回**真正写进 http.Request
// 的字节**。前两步直调真实导出函数，后三步交给真实的 build 函数。
func mirasimOutboundBody(t *testing.T, inbound []byte) []byte {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	// 透传路径原样转发客户端 anthropic-beta；给一个不触发任何 body 净化分支的值。
	c.Request.Header.Set("Anthropic-Beta", "oauth-2025-04-20")

	// gateway_anthropic_passthrough.go:82 / :87
	b := StripEmptyTextBlocks(inbound)
	b = FilterWebSearchHistoryBlocks(b, mirasimTestModel)

	// gateway_anthropic_passthrough.go:302 / :324 / :330 均在 build 内部
	svc := &GatewayService{cfg: &config.Config{}}
	req, wireBody, err := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(
		context.Background(), c, mirasimPassthroughAccount(), b, "upstream-key",
	)
	require.NoError(t, err)
	require.NotNil(t, req.Body)
	onWire, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, string(wireBody), string(onWire),
		"build 返回的 wireBody 必须与真正写进 http.Request 的字节一致")
	return onWire
}

// mirasimCachePrefix 按 Anthropic 线性前缀缓存的匹配顺序 tools → system → messages
// 把 body 拼成一段字节序列。消息之间用 "," 连接、不含数组括号，因此「追加一轮对话」
// 在字节上就是「在末尾追加」——bytes.HasPrefix 成立当且仅当前面每一段一字未动。
func mirasimCachePrefix(body []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(gjson.GetBytes(body, "tools").Raw)
	buf.WriteString(gjson.GetBytes(body, "system").Raw)
	for i, msg := range gjson.GetBytes(body, "messages").Array() {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(msg.Raw)
	}
	return buf.Bytes()
}

// mirasimObjectKeyOrder 返回 JSON 对象的 key 在文档中的出现顺序。
func mirasimObjectKeyOrder(res gjson.Result) []string {
	keys := make([]string, 0, 4)
	res.ForEach(func(k, _ gjson.Result) bool {
		keys = append(keys, k.String())
		return true
	})
	return keys
}

// ============================================================================
// 义务
// ============================================================================

func TestMirasimCachePrefixSystemBytewise(t *testing.T) {
	// [[cov:CP:system-bytewise]] 出站 system 与入站逐字节相同，cache_control 断点在原位
	inbound := mirasimInboundBody(
		mirasimTurnUserAsk,
		mirasimTurnAssistantText,
		mirasimTurnAssistantToolUse,
		mirasimTurnUserToolResultWithEmptyText,
	)
	inSystem := gjson.GetBytes(inbound, "system").Raw

	require.Equal(t, inSystem, gjson.GetBytes(StripEmptyTextBlocks(inbound), "system").Raw,
		"StripEmptyTextBlocks 只重写 messages，绝不得波及 system 段")

	outbound := mirasimOutboundBody(t, inbound)
	require.Equal(t, inSystem, gjson.GetBytes(outbound, "system").Raw,
		"出站 system 必须与入站逐字节相同：system 是缓存前缀的第二段，改一个字节后面全废")

	require.Equal(t, "ephemeral", gjson.GetBytes(outbound, "system.1.cache_control.type").String(),
		"cache_control 断点必须仍在 system[1]（原位），不得漂移或丢失")
	require.False(t, gjson.GetBytes(outbound, "system.0.cache_control").Exists(),
		"system[0] 原本无断点，不得被凭空插入断点")
}

func TestMirasimCachePrefixToolsBytewise(t *testing.T) {
	// [[cov:CP:tools-bytewise]] 出站 tools 与入站逐字节相同（tools 是前缀最前一段，动它整条链全废）
	inbound := mirasimInboundBody(
		mirasimTurnUserAsk,
		mirasimTurnAssistantText,
		mirasimTurnAssistantToolUse,
		mirasimTurnUserToolResultWithEmptyText,
	)
	inTools := gjson.GetBytes(inbound, "tools").Raw

	require.Equal(t, inTools, gjson.GetBytes(stripDeferredToolCacheControl(inbound), "tools").Raw,
		"deferred 工具本就不带 cache_control，stripDeferredToolCacheControl 必须是字节 no-op")

	outbound := mirasimOutboundBody(t, inbound)
	require.Equal(t, inTools, gjson.GetBytes(outbound, "tools").Raw,
		"出站 tools 必须与入站逐字节相同：tools 是缓存前缀最前一段，动它整条链全废")

	require.Equal(t, "ephemeral", gjson.GetBytes(outbound, "tools.2.cache_control.type").String(),
		"末位工具上的 cache_control 断点必须仍在 tools[2]")
	require.True(t, gjson.GetBytes(outbound, "tools.1.custom.defer_loading").Bool(),
		"deferred 标记必须原样保留，不得被净化掉")
}

func TestMirasimCachePrefixPrefixMonotonic(t *testing.T) {
	// [[cov:CP:prefix-monotonic]] 同一会话第 2 轮出站的 tools+system+messages 是第 1 轮的字节前缀
	//
	// 真实故障形态：第 1 轮干净（无空 text 块）→ StripEmptyTextBlocks 走快路径原样返回；
	// 第 2 轮追加的 tool_result 带了空 text 块 → 整条 messages 数组被 map round-trip
	// 重新序列化 → 第 1 轮那几条消息的 key 顺序/转义一并被改写 → 前缀断裂 → 整会话缓存归零。
	round1 := mirasimInboundBody(
		mirasimTurnUserAsk,
		mirasimTurnAssistantText,
	)
	round2 := mirasimInboundBody(
		mirasimTurnUserAsk,
		mirasimTurnAssistantText,
		mirasimTurnAssistantToolUse,
		mirasimTurnUserToolResultWithEmptyText,
	)

	require.Equal(t, string(round1), string(StripEmptyTextBlocks(round1)),
		"第 1 轮不含空 text 块，StripEmptyTextBlocks 必须走快路径原样返回（这是前缀基准）")

	prefix1 := mirasimCachePrefix(mirasimOutboundBody(t, round1))
	prefix2 := mirasimCachePrefix(mirasimOutboundBody(t, round2))

	require.True(t, bytes.HasPrefix(prefix2, prefix1),
		"第 2 轮出站的 tools+system+messages 必须以第 1 轮的为字节前缀（纯追加）。\n"+
			"round1 前缀 (%d B): %s\nround2 前缀 (%d B): %s",
		len(prefix1), string(prefix1), len(prefix2), string(prefix2))
}

func TestMirasimCachePrefixNoKeyReorder(t *testing.T) {
	// [[cov:CP:no-key-reorder]] 出站 body 的对象 key 顺序与入站相同
	inbound := mirasimInboundBody(
		mirasimTurnUserAsk,
		mirasimTurnAssistantText,
		mirasimTurnAssistantToolUse,
		mirasimTurnUserToolResultWithEmptyText,
	)
	wantOrder := mirasimObjectKeyOrder(gjson.GetBytes(inbound, "messages.0"))
	require.Equal(t, []string{"role", "content"}, wantOrder, "fixture 自检：入站 key 顺序非字母序")

	require.Equal(t, wantOrder,
		mirasimObjectKeyOrder(gjson.GetBytes(StripEmptyTextBlocks(inbound), "messages.0")),
		"StripEmptyTextBlocks 的 map[string]any round-trip 会把对象 key 按字母序重排，"+
			"content 会被排到 role 前面 —— 任何一条历史消息被重排，它之后的缓存全部失效")

	outbound := mirasimOutboundBody(t, inbound)
	require.Equal(t, wantOrder, mirasimObjectKeyOrder(gjson.GetBytes(outbound, "messages.0")),
		"出站 messages[0] 的 key 顺序必须与入站相同")
}

func TestMirasimCachePrefixNoHTMLEscape(t *testing.T) {
	// [[cov:CP:no-html-escape]] 正文里的 < > & 不被转义
	inbound := mirasimInboundBody(
		mirasimTurnUserAsk,
		mirasimTurnAssistantText,
		mirasimTurnAssistantToolUse,
		mirasimTurnUserToolResultWithEmptyText,
	)
	const literal = `"check whether a < b && c > d holds"`
	require.Contains(t, string(inbound), literal, "fixture 自检：入站正文里是裸的 < > &")

	afterStrip := string(StripEmptyTextBlocks(inbound))
	// 期望的失败形态是 6 字节转义序列（反斜杠 + u003c / u003e / u0026）。
	for _, escaped := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		require.NotContains(t, afterStrip, escaped,
			"StripEmptyTextBlocks 走 json.Marshal（默认 SetEscapeHTML(true)），会把 < > & 转义成 %q —— "+
				"同一段正文换了字节表示，缓存即失配", escaped)
	}

	outbound := mirasimOutboundBody(t, inbound)
	require.Contains(t, string(outbound), literal,
		"出站正文必须保持裸的 < > &，不得出现 \\u003c / \\u003e / \\u0026")
}

func TestMirasimCachePrefixBigintPrecision(t *testing.T) {
	// [[cov:CP:bigint-precision]] 工具入参里的大整数不丢精度
	inbound := mirasimInboundBody(
		mirasimTurnUserAsk,
		mirasimTurnAssistantText,
		mirasimTurnAssistantToolUse,
		mirasimTurnUserToolResultWithEmptyText,
	)
	const offsetPath = "messages.2.content.0.input.offset"
	const limitPath = "messages.2.content.0.input.limit"
	require.Equal(t, "12345678901234567890", gjson.GetBytes(inbound, offsetPath).Raw,
		"fixture 自检：入站是超出 float64 精确整数范围的字面量")

	afterStrip := StripEmptyTextBlocks(inbound)
	require.Equal(t, "12345678901234567890", gjson.GetBytes(afterStrip, offsetPath).Raw,
		"StripEmptyTextBlocks 把 JSON 数字解成 float64 再 Marshal，12345678901234567890 会变成 ...567000")
	require.Equal(t, "9007199254740993", gjson.GetBytes(afterStrip, limitPath).Raw,
		"同理 9007199254740993（2^53+1）会退化成 ...992")

	outbound := mirasimOutboundBody(t, inbound)
	require.Equal(t, "12345678901234567890", gjson.GetBytes(outbound, offsetPath).Raw,
		"出站工具入参的大整数必须逐字节保真")
}
