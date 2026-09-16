package service_test

// 真端到端验收：不 mock 任何一层，用**后台生成的 API key** 打**线上 sub2api 实例的
// 公开网关 URL**（POST {BASE}/v1/messages?beta=true），走完
//
//	sub2api APIKeyAuth → 分组/模型放行 → claude_code_only 门 → 账号调度
//	  → mirasim 签名 → Anthropic 上游 → 回程计费与 usage 归集
//
// 的完整链路，再对真实返回体做判决。
//
// 与 internal/repository/mirasim_live_test.go 的分工必须说清楚，否则两条会被误当成
// 重复：那一条直连 relay，证明的是「签名端口本身正确」，它**绕过了 sub2api 自己的
// HTTP 入口、鉴权、分组与 claude_code_only 门**；这一条证明的是「客户实际拿到的那条
// URL + 客户实际拿到的那把 key 真的能用」。前者全绿而后者红是完全可能的——所有把
// 客户挡在门外的机制都只长在这条路径上。
//
// 凭据只从环境变量读，绝不写进源码；失败消息与日志里最多出现 key 的前 6 位加省略号，
// 完整值永不落盘（测试日志会进 CI artifact 和验收记录）。
//
// 这两条测试会消耗真实 token（各一次真实上游往返），所以默认 Skip，必须显式开门。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const (
	e2eEnableEnv  = "SUB2API_E2E"
	e2eBaseURLEnv = "SUB2API_E2E_BASE_URL"
	e2eKeyEnv     = "SUB2API_E2E_KEY"
	e2eModelEnv   = "SUB2API_E2E_MODEL"

	e2eDefaultModel = "claude-haiku-4-5"

	// 线上一次请求可能要几十秒：上游节点故障会触发换号重试，每次重试都是一个完整的
	// 上游往返。超时给短了，拿到的就不是「链路不通」而是「测试自己等不及」，那是仪器
	// 错误不是被测对象的错误。
	e2eRequestTimeout = 180 * time.Second
)

// e2eSkipHint 是三个环境变量缺任一时打印的运行说明。
// 刻意把完整命令写全（含 -timeout）：两条测试各一次最长 180s 的真实往返，用 go test
// 默认的 10m 总超时在重试路径上会不够。
const e2eSkipHint = `真端到端验收未运行（默认关闭：它会消耗真实 token）。

需要三个环境变量同时就位，key 只从环境读、绝不入库：

    SUB2API_E2E=1 \
    SUB2API_E2E_BASE_URL=http://<sub2api 实例>:<端口> \
    SUB2API_E2E_KEY=<后台生成的 API key，不是管理员 token> \
    go test ./internal/service/ -run TestMirasimE2E -count=1 -v -timeout 900s

可选：SUB2API_E2E_MODEL（默认 ` + e2eDefaultModel + `）。`

type e2eConfig struct {
	baseURL string
	key     string
	model   string
}

// e2eGate 是三个环境变量的门。缺任一都 Skip 而不是 Fail：不设环境变量跑 go test 是
// 常态（CI、本地全量跑），把它判红等于让所有人学会忽略这条测试。
func e2eGate(t *testing.T) e2eConfig {
	t.Helper()
	if os.Getenv(e2eEnableEnv) != "1" {
		t.Skip(e2eSkipHint)
	}
	base := strings.TrimSpace(os.Getenv(e2eBaseURLEnv))
	key := strings.TrimSpace(os.Getenv(e2eKeyEnv))
	if base == "" || key == "" {
		t.Skip(e2eSkipHint)
	}
	model := strings.TrimSpace(os.Getenv(e2eModelEnv))
	if model == "" {
		model = e2eDefaultModel
	}
	return e2eConfig{baseURL: strings.TrimRight(base, "/"), key: key, model: model}
}

// e2eMaskKey 只保留前 6 位。任何要把凭据写进消息的地方都必须过这个函数。
func e2eMaskKey(k string) string {
	r := []rune(k)
	if len(r) <= 6 {
		return "…"
	}
	return string(r[:6]) + "…"
}

func e2eTruncate(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…[truncated]"
}

// ── 真实 Claude Code 2.1.272 的线上形态 ─────────────────────────────────────────
//
// 线上分组开了 claude_code_only：形态不对会在 sub2api 入口被判为非 CC 客户端而拒，
// 于是「链路通不通」这个问题根本走不到上游。这些常量与 handler 包实抓记录
// （internal/handler/claude_code_only_ctx_test.go）同源。

const (
	e2eCCUserAgent = "claude-cli/2.1.272 (external, sdk-cli)"
	// system 数组第 0 块是 94 字符的身份句，独占一个 cache_control 断点。
	e2eCCIdentityLine   = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	e2eCCAnthropicBeta  = "interleaved-thinking-2025-05-14,claude-code-20250219"
	e2eCCAnthropicVer   = "2023-06-01"
	e2eCCSecondaryBlock = "\nYou are an interactive agent that helps users with software engineering tasks."
)

// e2eCCMetadataUserID 是实抓格式：JSON 串，device_id 为 64 位 hex，account_uuid 可为
// 空串，session_id 为 UUID。每次运行都现生成，不写死——写死的 session_id 会让上游
// 前缀缓存命中，input_tokens 可能整体走 cache_read，那就分不清「真往返」和「缓存回放」，
// 而这恰好是 real-content-and-usage 这条义务要区分的东西。
type e2eCCUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

func e2eRandomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

func e2eRandomUUIDv4(t *testing.T) string {
	t.Helper()
	var b [16]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func e2eMetadataUserID(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(e2eCCUserID{
		DeviceID:    e2eRandomHex(t, 32), // 32 字节 → 64 位 hex
		AccountUUID: "",
		SessionID:   e2eRandomUUIDv4(t),
	})
	require.NoError(t, err)
	return string(raw)
}

// e2eCCHeaders 是实抓的完整请求头集合（去掉 host / content-length 这类传输层头）。
// 鉴权头由调用方单独设置，好让「换一把 key」成为两次请求之间**唯一**的变量。
func e2eCCHeaders() map[string]string {
	return map[string]string{
		"User-Agent":          e2eCCUserAgent,
		"x-app":               "cli",
		"anthropic-beta":      e2eCCAnthropicBeta,
		"anthropic-version":   e2eCCAnthropicVer,
		"accept":              "application/json",
		"content-type":        "application/json",
		"x-stainless-arch":    "arm64",
		"x-stainless-lang":    "js",
		"x-stainless-os":      "MacOS",
		"x-stainless-runtime": "node",
	}
}

// e2eMessagesBody 组装真实 CC 形态的 /v1/messages 请求体。
// nonce 混进用户消息里，作用是让这次请求在上游前缀缓存里必然 miss。
func e2eMessagesBody(t *testing.T, model, nonce, metadataUserID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 64,
		"stream":     false,
		"metadata":   map[string]any{"user_id": metadataUserID},
		"system": []any{
			map[string]any{
				"type": "text", "text": e2eCCIdentityLine,
				"cache_control": map[string]any{"type": "ephemeral"},
			},
			map[string]any{
				"type": "text", "text": e2eCCSecondaryBlock,
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		},
		"messages": []any{map[string]any{
			"role": "user",
			"content": "Answer with the single word: pong. " +
				"(request nonce " + nonce + " — ignore it)",
		}},
	})
	require.NoError(t, err)
	return body
}

// e2eNewMessagesRequest 造一个指向线上实例的 /v1/messages 请求。
// credential 走 x-api-key（sub2api 的 APIKeyAuth 接受 Authorization: Bearer 与
// x-api-key 两种，这里固定一种，避免「换了头」混进变量）。
func e2eNewMessagesRequest(t *testing.T, ctx context.Context, cfg e2eConfig, credential string, body []byte) *http.Request {
	t.Helper()
	// ?beta=true 是真实 CC 会带的查询串，一并复刻。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.baseURL+"/v1/messages?beta=true", bytes.NewReader(body))
	require.NoError(t, err)
	for k, v := range e2eCCHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-api-key", credential)
	return req
}

func e2eDo(t *testing.T, req *http.Request) (int, http.Header, []byte, time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: e2eRequestTimeout}
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	require.NoError(t, err, "传输层就失败了（%s 后）：连 HTTP 状态码都没拿到，这不是被测对象的判决而是网络/地址问题", elapsed.Round(time.Millisecond))
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, raw, elapsed
}

// ─────────────────────────────────────────────────────────────────────────────

// TestMirasimE2EGeneratedKeyAuthenticates 覆盖「鉴权用的是后台生成的 key，不是管理员
// token」。
//
// 单纯「拿 key 打一次拿到 200」证明不了这条：如果入口根本不校验凭据，任何字符串都是
// 200，那条 200 就是空的。所以这里做三段判决，缺一不可：
//
//	A 正例：生成的 key → 200
//	B 差分阴性：**只**把 key 换成一个明显不是它的随机串，其余一字不差 → 必须非 200
//	C 归属证明：同一把 key 打管理面 → 必须被拒（管理员 token 在那条路径上是通的，
//	  所以这把 key 被拒 = 它不是管理员 token）
//
// 没有 B，「入口不校验」和「key 有效」无法区分；没有 C，「用的是客户 key」和「顺手拿
// 管理员 token 试的」无法区分——而 C31 的 negative 明确把后者判为不通过。
func TestMirasimE2EGeneratedKeyAuthenticates(t *testing.T) {
	// [[cov:E2E:generated-key]]
	cfg := e2eGate(t)
	t.Logf("base=%s model=%s key=%s", cfg.baseURL, cfg.model, e2eMaskKey(cfg.key))

	ctx, cancel := context.WithTimeout(context.Background(), e2eRequestTimeout)
	defer cancel()

	nonce := e2eRandomHex(t, 8)
	body := e2eMessagesBody(t, cfg.model, nonce, e2eMetadataUserID(t))

	// 前置自证：这个 body + 这组 header 在本仓库**自己的**校验器眼里确实是 Claude
	// Code。少了这一步，线上一旦 403，我们分不清是 key 的问题还是我手拼的形态不像
	// 真客户端——而形态不像正是 C31 negative 点名的失败模式。
	var bodyMap map[string]any
	require.NoError(t, json.Unmarshal(body, &bodyMap))
	shapeProbe := e2eNewMessagesRequest(t, ctx, cfg, cfg.key, body)
	require.True(t, service.NewClaudeCodeValidator().Validate(shapeProbe, bodyMap),
		"本次要发的请求连本仓库自己的 ClaudeCodeValidator 都过不了，说明形态不是真 CC；"+
			"此时线上的任何结果都归因不到 key 上")

	// ── A 正例 ──
	okStatus, okHeader, okBody, okElapsed := e2eDo(t, e2eNewMessagesRequest(t, ctx, cfg, cfg.key, body))
	t.Logf("A 生成的 key → HTTP %d in %s (request-id=%s)",
		okStatus, okElapsed.Round(time.Millisecond), okHeader.Get("request-id"))
	require.Equal(t, http.StatusOK, okStatus,
		"后台生成的 key 打 %s/v1/messages 没拿到 200，客户拿到这把 key 也用不了；响应=%s",
		cfg.baseURL, e2eTruncate(string(okBody), 600))

	// ── B 差分阴性：唯一变量是 key 的值 ──
	// 用一个全新随机串而不是「把真 key 改一个字符」：后者会把真 key 的长度和大部分
	// 内容带进代码与日志的推理链里，没必要。
	bogus := "sk-sub2api-e2e-negative-control-" + e2eRandomHex(t, 16)
	badStatus, _, badBody, _ := e2eDo(t, e2eNewMessagesRequest(t, ctx, cfg, bogus, body))
	t.Logf("B 伪造凭据 → HTTP %d  %s", badStatus, e2eTruncate(string(badBody), 240))
	require.NotEqual(t, http.StatusOK, badStatus,
		"入口对一个随机字符串也返回 200：它根本没在校验 key，于是 A 的那个 200 不构成任何证据")
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, badStatus,
		"期望鉴权类拒绝（401/403），实得 HTTP %d。特别注意 429：那是 invalid-auth 滥用限流"+
			"提前介入，这次拒绝就不是「key 不对」而是「请求被限流」，差分不成立；响应=%s",
		badStatus, e2eTruncate(string(badBody), 240))

	// ── C 归属证明：这把 key 不是管理员 token ──
	// 管理面 adminAuth 同时接受 x-api-key(管理员 API key) 与 Authorization: Bearer(管理员
	// JWT)。若 SUB2API_E2E_KEY 是管理员凭据，这里会是 200。
	adminReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.baseURL+"/api/v1/admin/dashboard/stats", nil)
	require.NoError(t, err)
	adminReq.Header.Set("x-api-key", cfg.key)
	adminReq.Header.Set("accept", "application/json")
	adminStatus, _, adminBody, _ := e2eDo(t, adminReq)
	t.Logf("C 同一把 key 打管理面 → HTTP %d  %s", adminStatus, e2eTruncate(string(adminBody), 240))
	require.NotEqual(t, http.StatusOK, adminStatus,
		"同一把凭据在管理面也通行：SUB2API_E2E_KEY 是管理员 token 而不是后台生成的客户 key，"+
			"本条验收不成立（管理员走的是另一条路径，过了不代表客户过得了）")
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, adminStatus,
		"期望管理面以 401/403 拒绝这把客户 key，实得 HTTP %d。若是 404，说明管理面路径已经"+
			"搬家，这条阴性对照照的是一个不存在的门、等于没照，必须改指向新路径而不是放过",
		adminStatus)
}

// TestMirasimE2ERealContentAndUsage 覆盖「返回真实 content，且 usage 的 input/output
// token 均非零」。
//
// 为什么 usage 全零要单独判死：全零是「拿到的是桩响应/缓存回放」的指纹——链路上任何
// 一层短路（本地 mock、命中响应缓存、上游返回空壳）都能给出一个 200 + 一段文本，但
// 没有任何一层能在不做真实上游往返的情况下给出真实的 token 计数。
//
// 另一半陷阱是 Go 的零值：字段**缺失**和字段**等于 0** 反序列化后长得一模一样，直接
// require.Greater 会把「usage 里压根没这个字段」误判成「有字段但是 0」，两者的修法完全
// 不同。所以这里先用 json.RawMessage 判存在，再判数值。
func TestMirasimE2ERealContentAndUsage(t *testing.T) {
	// [[cov:E2E:real-content-and-usage]]
	cfg := e2eGate(t)
	t.Logf("base=%s model=%s key=%s", cfg.baseURL, cfg.model, e2eMaskKey(cfg.key))

	ctx, cancel := context.WithTimeout(context.Background(), e2eRequestTimeout)
	defer cancel()

	nonce := e2eRandomHex(t, 8)
	body := e2eMessagesBody(t, cfg.model, nonce, e2eMetadataUserID(t))
	status, header, raw, elapsed := e2eDo(t, e2eNewMessagesRequest(t, ctx, cfg, cfg.key, body))
	t.Logf("HTTP %d in %s (request-id=%s) nonce=%s",
		status, elapsed.Round(time.Millisecond), header.Get("request-id"), nonce)
	require.Equal(t, http.StatusOK, status,
		"没拿到 200 就无所谓 content 与 usage 了；响应=%s", e2eTruncate(string(raw), 600))

	// 第一层：字段是否真的存在（区分「缺失」与「为零」）。
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &top), "响应不是 JSON 对象：%s", e2eTruncate(string(raw), 600))
	require.Contains(t, top, "content", "200 但响应里没有 content 字段：%s", e2eTruncate(string(raw), 600))
	require.Contains(t, top, "usage",
		"200 但响应里没有 usage 字段。注意这不等于「usage 是 0」——直接按结构体解会得到"+
			"零值并被误读成「上游返回了 0 token」，两者的排查方向完全相反：%s", e2eTruncate(string(raw), 600))

	var usageRaw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(top["usage"], &usageRaw))
	require.Contains(t, usageRaw, "input_tokens", "usage 里没有 input_tokens 字段，usage=%s", e2eTruncate(string(top["usage"]), 240))
	require.Contains(t, usageRaw, "output_tokens", "usage 里没有 output_tokens 字段，usage=%s", e2eTruncate(string(top["usage"]), 240))

	// 第二层：数值与内容。
	var parsed struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed), "解码响应失败：%s", e2eTruncate(string(raw), 600))

	var text string
	var textBlocks int
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
			textBlocks++
		}
	}
	t.Logf("type=%s role=%s model=%s stop=%s content=%q", parsed.Type, parsed.Role, parsed.Model, parsed.StopReason, text)
	t.Logf("usage: input=%d output=%d cache_read=%d cache_creation=%d",
		parsed.Usage.InputTokens, parsed.Usage.OutputTokens,
		parsed.Usage.CacheReadInputTokens, parsed.Usage.CacheCreationInputTokens)

	require.Equal(t, "message", parsed.Type, "响应 type 不是 message，这不是一条 Anthropic messages 响应")
	require.Equal(t, "assistant", parsed.Role, "响应 role 不是 assistant")
	require.NotEqual(t, "", parsed.Model, "响应没带回 model，说明它不是上游真实产出的消息对象")
	require.Greater(t, textBlocks, 0, "content 里一个 text 块都没有")
	require.Greater(t, len(strings.TrimSpace(text)), 0, "content 的 text 块存在但内容是空白")
	require.Regexp(t, `[A-Za-z]`, text, "content 里连一个字母都没有，不构成「真实文本」")

	// 「真实文本」比「非空」强一档：这句提问是本次现造的（带 nonce，上游前缀缓存必然
	// miss），能正确回答它的只可能是真的模型。一个桩响应或缓存回放不可能知道要答 pong。
	require.Contains(t, strings.ToLower(text), "pong",
		"模型没有回答被问到的那个词。若人工确认链路其余部分正常、只是模型答了别的措辞，"+
			"放宽这条断言是合理的，但必须在验收记录里写明放宽了什么——因为放宽之后，"+
			"「真实文本」就退回成「有非空字符串」，桩响应也能过：实得 content=%q", text)

	// usage 双侧非零：只判一侧不够。input 非零而 output 为零可能是流式截断或上游空回；
	// output 非零而 input 为零说明计费侧压根没数进来的 token，两种都不是一次完整的真实往返。
	require.Greater(t, parsed.Usage.InputTokens, 0,
		"input_tokens=0：字段在但计数为零，意味着这次 200 背后没有真正把 prompt 送上去过（桩/缓存的指纹）")
	require.Greater(t, parsed.Usage.OutputTokens, 0,
		"output_tokens=0：返回了文本却没有产出 token 计数，content 与 usage 自相矛盾，两者必有一个是假的")
}
