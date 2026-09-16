package handler

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 真实 Claude Code 2.1.272 的线上形态，2026-09-16 用本地抓包服务器实抓
// （`claude --settings '{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:<port>"}}' -p ...`）。
//
// 为什么这条测试必须存在于 handler 包、而不是复用 service 包里的校验器测试：
// claude_code_only 的判定不是一次函数调用，是一条**跨层的 context 传递链**：
//
//	SetClaudeCodeClientContext(gateway_helper.go:47)
//	  → 用 ParsedRequest **重建**的 bodyMap（不是原始 body！）喂 Validate
//	  → service.SetClaudeCodeClient 写 ctx
//	  → c.Request = c.Request.WithContext(ctx)
//	  → resolveGatewayGroup(gateway_scheduling.go:940) 读 IsClaudeCodeClient(ctx)
//
// service 包的校验器测试只覆盖第二步，喂的是手写的完整 bodyMap。真正会误拒的
// 缺口在**重建**那一步：claudeCodeBodyMapFromParsedRequest 只搬 model /
// max_tokens / system / metadata 四个字段，任何一个没被 ParsedRequest 解析出来，
// 校验器就会看到一个缺字段的 body 并判 false —— 而校验器自己的测试永远发现不了。
const (
	realCCUserAgent = "claude-cli/2.1.272 (external, sdk-cli)"
	// 实抓：block[0] 是 94 字符的身份句，独占一个 cache_control 断点。
	realCCIdentityLine = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	// 实抓格式：JSON 串，device_id 为 64 位 hex，account_uuid 可为空串，session_id 为 UUID。
	realCCMetadataUserID = `{"device_id":"1a87ea8d2db01477db2115f4900b2e95b85df90280389e4edf579e0631c0c001","account_uuid":"","session_id":"a2fb3b6b-314b-4e47-bf94-572dbfb99dda"}`
)

// realCCHeaders 是实抓的完整请求头集合（去掉 authorization / host / content-length
// 这类与身份判定无关的传输层头）。
func realCCHeaders() map[string]string {
	return map[string]string{
		"User-Agent":          realCCUserAgent,
		"x-app":               "cli",
		"anthropic-beta":      "interleaved-thinking-2025-05-14,claude-code-20250219",
		"anthropic-version":   "2023-06-01",
		"accept":              "application/json",
		"content-type":        "application/json",
		"x-stainless-arch":    "arm64",
		"x-stainless-lang":    "js",
		"x-stainless-os":      "MacOS",
		"x-stainless-runtime": "node",
	}
}

func realCCBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 32000,
		"stream":     true,
		"metadata":   map[string]any{"user_id": realCCMetadataUserID},
		"system": []any{
			map[string]any{"type": "text", "text": realCCIdentityLine,
				"cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "\nYou are an interactive agent that helps users with software engineering tasks.",
				"cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	})
	require.NoError(t, err)
	return body
}

// TestClaudeCodeOnlyGateAcceptsRealClaudeCode 钉住 claude_code_only 分组必须放行真 CC。
//
// 误拒的代价是不对称的：漏放一个冒充者只是少一层限制，而误拒真 CC 会让**整个分组
// 对唯一的合法客户端不可用**。所以这条走的是完整的生产链路，而不是直接调
// validator.Validate。
func TestClaudeCodeOnlyGateAcceptsRealClaudeCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := realCCBody(t)

	parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef(body), "anthropic")
	require.NoError(t, err, "真 CC 的 body 必须能被生产解析器解析")

	// 重建路径的前提：这四个字段一个都不能丢，丢任何一个都会让下面的门判 false。
	require.Equal(t, "claude-haiku-4-5", parsed.Model)
	require.Equal(t, 32000, parsed.MaxTokens)
	require.True(t, parsed.HasSystem, "system 没被解析出来 → 重建的 bodyMap 缺 system → 相似度门必假")
	require.Equal(t, realCCMetadataUserID, parsed.MetadataUserID,
		"metadata.user_id 没被解析出来 → 重建的 bodyMap 缺 metadata → 4.3 门必假")

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages?beta=true", bytes.NewReader(body))
	for k, v := range realCCHeaders() {
		c.Request.Header.Set(k, v)
	}

	SetClaudeCodeClientContext(c, body, parsed)

	require.True(t, service.IsClaudeCodeClient(c.Request.Context()),
		"真实 Claude Code 2.1.272 被 claude_code_only 门误拒 —— 该分组会对唯一的合法客户端完全不可用")
	require.Equal(t, "2.1.272", service.GetClaudeCodeVersion(c.Request.Context()),
		"版本号必须从 UA 提取并写入 ctx，否则最低版本门拿不到版本")
}

// TestClaudeCodeOnlyGateStillRejectsPlainAPIClient 是上面那条的差分阴性对照：
// 同一个 body，只把 UA 换成普通 API 客户端，门必须判 false。
// 没有这一条，把 SetClaudeCodeClientContext 改成无条件 true 也能让上面那条转绿。
func TestClaudeCodeOnlyGateStillRejectsPlainAPIClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := realCCBody(t)
	parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef(body), "anthropic")
	require.NoError(t, err)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages?beta=true", bytes.NewReader(body))
	for k, v := range realCCHeaders() {
		c.Request.Header.Set(k, v)
	}
	c.Request.Header.Set("User-Agent", "python-httpx/0.27.0")

	SetClaudeCodeClientContext(c, body, parsed)
	require.False(t, service.IsClaudeCodeClient(c.Request.Context()),
		"非 Claude CLI 的 UA 必须判 false，否则 claude_code_only 形同虚设")
}
