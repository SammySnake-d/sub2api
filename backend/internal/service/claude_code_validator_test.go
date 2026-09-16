package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

const claudeCodeMetadataUserIDJSON = `{"device_id":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","account_uuid":"","session_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`

func TestClaudeCodeValidator_ProbeBypass(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/1.2.3 (darwin; arm64)")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.IsMaxTokensOneHaikuRequest, true))

	ok := validator.Validate(req, map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 1,
	})
	require.True(t, ok)
}

func TestClaudeCodeValidator_ProbeBypassRequiresUA(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "curl/8.0.0")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.IsMaxTokensOneHaikuRequest, true))

	ok := validator.Validate(req, map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 1,
	})
	require.False(t, ok)
}

func TestClaudeCodeValidator_MessagesWithoutProbeStillNeedStrictValidation(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/1.2.3 (darwin; arm64)")

	// max_tokens=2 不是探测请求：没有 system prompt 仍走严格校验并被拒。
	ok := validator.Validate(req, map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 2,
	})
	require.False(t, ok)
}

func TestClaudeCodeValidator_CountTokensPathUAOnly(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages/count_tokens", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.156 (Claude Code)")

	ok := validator.Validate(req, map[string]any{
		"model": "claude-opus-4-8",
	})
	require.True(t, ok)
}

func TestClaudeCodeValidator_CountTokensPathRequiresUA(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages/count_tokens", nil)
	req.Header.Set("User-Agent", "curl/8.0.0")

	ok := validator.Validate(req, map[string]any{
		"model": "claude-opus-4-8",
	})
	require.False(t, ok)
}

func TestClaudeCodeValidator_MessagesPathFullValid(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.156 (Claude Code)")
	req.Header.Set("X-App", "claude-code")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	req.Header.Set("anthropic-version", "2023-06-01")

	ok := validator.Validate(req, map[string]any{
		"model":  "claude-opus-4-8",
		"stream": true,
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "You are Claude Code, Anthropic's official CLI for Claude.",
			},
		},
		"metadata": map[string]any{
			"user_id": "user_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_account__session_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		},
	})
	require.True(t, ok)
}

func TestClaudeCodeValidator_BillingBlockRecognizedWithoutIdentityPrompt(t *testing.T) {
	// 真实抓取的完整安全监视器 system prompt（不含身份 prose）。
	monitorPrompt, err := os.ReadFile("testdata/security_monitor_system_prompt.txt")
	require.NoError(t, err)

	validator := NewClaudeCodeValidator()

	// 前提：完整监视器正文经 Dice 相似度远低于阈值，无法被身份 prose 机制识别——
	// 故下面 Validate 的放行只可能来自计费归因块识别。
	require.Less(t, validator.bestSimilarityScore(string(monitorPrompt)), systemPromptThreshold)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.162 (external, cli)")
	req.Header.Set("X-App", "cli")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	req.Header.Set("anthropic-version", "2023-06-01")

	// Claude Code 安全监视器子请求：不携带身份 prose，但 system 数组携带计费归因块
	// cc_entrypoint=cli，应据此识别为 Claude Code 客户端。
	ok := validator.Validate(req, map[string]any{
		"model": "claude-3-5-haiku-20241022",
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.162.884; cc_entrypoint=cli; cch=d8726;",
			},
			map[string]any{
				"type": "text",
				"text": string(monitorPrompt),
			},
		},
		"metadata": map[string]any{
			"user_id": claudeCodeMetadataUserIDJSON,
		},
	})
	require.True(t, ok)
}

func TestClaudeCodeValidator_SecurityMonitorWithoutBillingBlock(t *testing.T) {
	monitorPrompt, err := os.ReadFile("testdata/security_monitor_system_prompt.txt")
	require.NoError(t, err)

	validHeaders := map[string]string{
		"User-Agent":        "claude-cli/2.1.220 (external, cli)",
		"X-App":             "cli",
		"anthropic-beta":    "claude-code-20250219",
		"anthropic-version": "2023-06-01",
	}
	validBody := func(prompt string) map[string]any {
		return map[string]any{
			"model": "claude-haiku-4-5-20251001",
			"system": []any{
				map[string]any{"type": "text", "text": prompt},
			},
			"metadata": map[string]any{"user_id": claudeCodeMetadataUserIDJSON},
		}
	}

	// 真实 CLI（2.1.220）在监视器提示词之后追加的独立会话上下文块（脱敏），
	// 随会话/环境变化，服务端不可控（见 issue #5152 抓包）。
	sessionContext := "\n\n## Session Context\n\n- **User identity**: testuser\n" +
		"- **Working directory**: /home/testuser/project\n- **Platform**: linux"

	tests := []struct {
		name       string
		headers    map[string]string
		body       map[string]any
		wantAccept bool
	}{
		{
			name:       "official classifier request",
			headers:    validHeaders,
			body:       validBody(string(monitorPrompt)),
			wantAccept: true,
		},
		{
			name:    "classifier output with category element",
			headers: validHeaders,
			body: validBody(strings.Replace(
				string(monitorPrompt),
				"<block>yes</block><reason>",
				"<block>yes</block><category>Exact BLOCK Rule Name</category><reason>",
				1,
			)),
			wantAccept: true,
		},
		{
			name: "non-Claude user agent",
			headers: map[string]string{
				"User-Agent":        "curl/8.0.0",
				"X-App":             "cli",
				"anthropic-beta":    "claude-code-20250219",
				"anthropic-version": "2023-06-01",
			},
			body: validBody(string(monitorPrompt)),
		},
		{
			name: "missing X-App",
			headers: map[string]string{
				"User-Agent":        validHeaders["User-Agent"],
				"anthropic-beta":    validHeaders["anthropic-beta"],
				"anthropic-version": validHeaders["anthropic-version"],
			},
			body: validBody(string(monitorPrompt)),
		},
		{
			name: "missing anthropic-beta",
			headers: map[string]string{
				"User-Agent":        validHeaders["User-Agent"],
				"X-App":             validHeaders["X-App"],
				"anthropic-version": validHeaders["anthropic-version"],
			},
			body: validBody(string(monitorPrompt)),
		},
		{
			name: "missing anthropic-version",
			headers: map[string]string{
				"User-Agent":     validHeaders["User-Agent"],
				"X-App":          validHeaders["X-App"],
				"anthropic-beta": validHeaders["anthropic-beta"],
			},
			body: validBody(string(monitorPrompt)),
		},
		{
			name:    "missing metadata",
			headers: validHeaders,
			body: map[string]any{
				"model":  "claude-haiku-4-5-20251001",
				"system": []any{map[string]any{"type": "text", "text": string(monitorPrompt)}},
			},
		},
		{
			name:    "invalid metadata user ID",
			headers: validHeaders,
			body: func() map[string]any {
				body := validBody(string(monitorPrompt))
				body["metadata"] = map[string]any{"user_id": "invalid"}
				return body
			}(),
		},
		{
			name:       "unrelated prompt",
			headers:    validHeaders,
			body:       validBody("You are a different security classifier for coding agents."),
			wantAccept: false,
		},
		{
			name:       "opening sentence alone",
			headers:    validHeaders,
			body:       validBody(claudeCodeSecurityMonitorPromptPrefix),
			wantAccept: false,
		},
		{
			name:    "opening sentence plus arbitrary altered suffix",
			headers: validHeaders,
			body: validBody(claudeCodeSecurityMonitorPromptPrefix + "\n\n" +
				strings.Repeat("This is arbitrary altered classifier content. ", 300)),
			wantAccept: false,
		},
		{
			// 回归 issue #5152：真实分类器请求携带 2 个 system entry
			//（监视器提示词 + 追加的会话上下文块），不得因 entry 数量拒识。
			name:    "classifier with trailing session context entry",
			headers: validHeaders,
			body: func() map[string]any {
				body := validBody(string(monitorPrompt))
				system, ok := body["system"].([]any)
				require.True(t, ok)
				body["system"] = append(system, map[string]any{
					"type": "text",
					"text": sessionContext,
				})
				return body
			}(),
			wantAccept: true,
		},
		{
			name:    "classifier with leading session context entry",
			headers: validHeaders,
			body: func() map[string]any {
				body := validBody(string(monitorPrompt))
				system, ok := body["system"].([]any)
				require.True(t, ok)
				body["system"] = append([]any{map[string]any{
					"type": "text",
					"text": sessionContext,
				}}, system...)
				return body
			}(),
			wantAccept: true,
		},
		{
			name:       "session context entry alone",
			headers:    validHeaders,
			body:       validBody(sessionContext),
			wantAccept: false,
		},
		{
			// 篡改后的长提示词（marker 缺失）即便带上会话上下文块也不得放行。
			name:    "tampered classifier with session context entry",
			headers: validHeaders,
			body: func() map[string]any {
				body := validBody(strings.ReplaceAll(
					string(monitorPrompt), "## HARD BLOCK", "## ALTERED BLOCK"))
				system, ok := body["system"].([]any)
				require.True(t, ok)
				body["system"] = append(system, map[string]any{
					"type": "text",
					"text": sessionContext,
				})
				return body
			}(),
			wantAccept: false,
		},
	}

	validator := NewClaudeCodeValidator()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}

			require.Equal(t, tt.wantAccept, validator.Validate(req, tt.body))
		})
	}
}

func TestClaudeCodeValidator_BillingBlockVSCodeEntrypointRecognized(t *testing.T) {
	// 回归：Claude Code 在 VSCode 扩展内运行时，计费块入口为 cc_entrypoint=claude-vscode
	// 而非 cli。其安全监视器子请求同样不携带身份 prose，此前写死 cc_entrypoint=cli 的
	// 快速通道无法识别它，导致 claude_code_only 分组误拒。入口值不应作为识别条件。
	monitorPrompt, err := os.ReadFile("testdata/security_monitor_system_prompt.txt")
	require.NoError(t, err)

	validator := NewClaudeCodeValidator()

	// 前提：完整监视器正文经 Dice 相似度远低于阈值，放行只可能来自计费归因块识别。
	require.Less(t, validator.bestSimilarityScore(string(monitorPrompt)), systemPromptThreshold)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.181 (external, claude-vscode, agent-sdk/0.3.181)")
	req.Header.Set("X-App", "cli")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	req.Header.Set("anthropic-version", "2023-06-01")

	ok := validator.Validate(req, map[string]any{
		"model": "claude-opus-4-8",
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.181.f17; cc_entrypoint=claude-vscode;",
			},
			map[string]any{
				"type": "text",
				"text": string(monitorPrompt),
			},
		},
		"metadata": map[string]any{
			"user_id": claudeCodeMetadataUserIDJSON,
		},
	})
	require.True(t, ok)
}

func TestClaudeCodeValidator_BillingBlockWithoutEntrypointFallsThrough(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.162 (external, cli)")
	req.Header.Set("X-App", "cli")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	req.Header.Set("anthropic-version", "2023-06-01")

	// 计费块前缀命中但完全没有 cc_entrypoint= 字段，且无身份 prose：
	// 不应凭前缀放行，应落回 Dice 检查并失败。验证 cc_entrypoint= 字段的存在仍是必要条件。
	ok := validator.Validate(req, map[string]any{
		"model": "claude-3-5-haiku-20241022",
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.162.884; cch=d8726;",
			},
			map[string]any{
				"type": "text",
				"text": "Some unrelated system prompt that does not resemble Claude Code.",
			},
		},
		"metadata": map[string]any{
			"user_id": claudeCodeMetadataUserIDJSON,
		},
	})
	require.False(t, ok)
}

func TestClaudeCodeValidator_BillingBlockStillRequiresClaudeCodeUA(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "curl/8.0.0")
	req.Header.Set("X-App", "cli")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	req.Header.Set("anthropic-version", "2023-06-01")

	// 计费块无法绕过 UA 校验：非 claude-cli 客户端在 Step 1 即被拒。
	ok := validator.Validate(req, map[string]any{
		"model": "claude-3-5-haiku-20241022",
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.162.884; cc_entrypoint=cli; cch=d8726;",
			},
		},
	})
	require.False(t, ok)
}

// 新版 Claude Code CLI 已取消 cch=... 签名字段，billing block 形如
// `x-anthropic-billing-header: cc_version=...; cc_entrypoint=cli;`（无 cch）。
// 检测依赖前缀 + cc_entrypoint=cli，不依赖 cch，故无身份 prose 的子请求仍应被识别。
// 这同时覆盖了本仓 mimicry 注入的新格式 block（见 buildBillingAttributionText）。
func TestClaudeCodeValidator_BillingBlockRecognizedWithoutCCH(t *testing.T) {
	monitorPrompt, err := os.ReadFile("testdata/security_monitor_system_prompt.txt")
	require.NoError(t, err)

	validator := NewClaudeCodeValidator()
	require.Less(t, validator.bestSimilarityScore(string(monitorPrompt)), systemPromptThreshold)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.162 (external, cli)")
	req.Header.Set("X-App", "cli")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	req.Header.Set("anthropic-version", "2023-06-01")

	ok := validator.Validate(req, map[string]any{
		"model": "claude-3-5-haiku-20241022",
		"system": []any{
			map[string]any{
				"type": "text",
				// 注意：无 cch 段，对齐新版 CLI 与本仓新的注入格式。
				"text": "x-anthropic-billing-header: cc_version=2.1.162.884; cc_entrypoint=cli;",
			},
			map[string]any{
				"type": "text",
				"text": string(monitorPrompt),
			},
		},
		"metadata": map[string]any{
			"user_id": claudeCodeMetadataUserIDJSON,
		},
	})
	require.True(t, ok, "无 cch 的新版 billing block 仍应被识别为 Claude Code")
}

// 安全回归：去掉 cch 后检测并未放松——非 claude-cli UA 即便携带无 cch 的 billing block
// 仍在 Step 1 被拒，ClaudeCodeOnly group 不会因此被仿冒绕过。
func TestClaudeCodeValidator_NoCCHBlockStillRequiresClaudeCodeUA(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "curl/8.0.0")
	req.Header.Set("X-App", "cli")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	req.Header.Set("anthropic-version", "2023-06-01")

	ok := validator.Validate(req, map[string]any{
		"model": "claude-3-5-haiku-20241022",
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.162.884; cc_entrypoint=cli;",
			},
		},
	})
	require.False(t, ok)
}

func TestClaudeCodeValidator_MessagesPathRejectsNonClaudeCodeUA(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "curl/8.0.0")
	req.Header.Set("X-App", "claude-code")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	req.Header.Set("anthropic-version", "2023-06-01")

	ok := validator.Validate(req, map[string]any{
		"model":  "claude-opus-4-8",
		"stream": true,
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "You are Claude Code, Anthropic's official CLI for Claude.",
			},
		},
		"metadata": map[string]any{
			"user_id": "user_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_account__session_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		},
	})
	require.False(t, ok)
}

func TestClaudeCodeValidator_MessagesPathWithoutSystemPromptStillRejected(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.156 (Claude Code)")
	req.Header.Set("X-App", "claude-code")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	req.Header.Set("anthropic-version", "2023-06-01")

	ok := validator.Validate(req, map[string]any{
		"model":  "claude-opus-4-8",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"metadata": map[string]any{
			"user_id": "user_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_account__session_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		},
	})
	require.False(t, ok)
}

func TestClaudeCodeValidator_NonMessagesPathUAOnly(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/models", nil)
	req.Header.Set("User-Agent", "claude-cli/1.2.3 (darwin; arm64)")

	ok := validator.Validate(req, nil)
	require.True(t, ok)
}

func TestExtractVersion(t *testing.T) {
	v := NewClaudeCodeValidator()
	tests := []struct {
		ua   string
		want string
	}{
		{"claude-cli/2.1.22 (darwin; arm64)", "2.1.22"},
		{"claude-cli/1.0.0", "1.0.0"},
		{"Claude-CLI/3.10.5 (linux; x86_64)", "3.10.5"}, // 大小写不敏感
		{"curl/8.0.0", ""},                              // 非 Claude CLI
		{"", ""},                                        // 空字符串
		{"claude-cli/", ""},                             // 无版本号
		{"claude-cli/2.1.22-beta", "2.1.22"},            // 带后缀仍提取主版本号
	}
	for _, tt := range tests {
		got := v.ExtractVersion(tt.ua)
		require.Equal(t, tt.want, got, "ExtractVersion(%q)", tt.ua)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"2.1.0", "2.1.0", 0},   // 相等
		{"2.1.1", "2.1.0", 1},   // patch 更大
		{"2.0.0", "2.1.0", -1},  // minor 更小
		{"3.0.0", "2.99.99", 1}, // major 更大
		{"1.0.0", "2.0.0", -1},  // major 更小
		{"0.0.1", "0.0.0", 1},   // patch 差异
		{"", "1.0.0", -1},       // 空字符串 vs 正常版本
		{"v2.1.0", "2.1.0", 0},  // v 前缀处理
	}
	for _, tt := range tests {
		got := CompareVersions(tt.a, tt.b)
		require.Equal(t, tt.want, got, "CompareVersions(%q, %q)", tt.a, tt.b)
	}
}

func TestSetGetClaudeCodeVersion(t *testing.T) {
	ctx := context.Background()
	require.Equal(t, "", GetClaudeCodeVersion(ctx), "empty context should return empty string")

	ctx = SetClaudeCodeVersion(ctx, "2.1.63")
	require.Equal(t, "2.1.63", GetClaudeCodeVersion(ctx))
}

func TestClaudeCodeValidator_MaxTokensOneProbeIsNotLimitedToHaiku(t *testing.T) {
	validator := NewClaudeCodeValidator()

	for _, model := range []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5"} {
		t.Run(model, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
			req.Header.Set("User-Agent", "claude-cli/2.1.260 (external, cli)")
			// No context flag, no system prompt, no extra headers: the body alone marks the probe.
			for _, mt := range []any{float64(1), 1} {
				require.True(t, validator.Validate(req, map[string]any{"model": model, "max_tokens": mt}), "max_tokens=%v (%T)", mt, mt)
			}
		})
	}
}

func TestClaudeCodeValidator_MaxTokensOneProbeStillRequiresClaudeCodeUA(t *testing.T) {
	validator := NewClaudeCodeValidator()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("User-Agent", "python-requests/2.32")

	require.False(t, validator.Validate(req, map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 1}))
}

// ─────────────────────────────────────────────────────────────────────────────
// G12 / C32 / C33 — 真实 Claude Code 2.1.272 必须通过「仅 CC」校验，
// 而伪造者不能因为放行真客户端而顺带通过。
//
// 夹具来自 2026-09-16 本机实抓（一个本地 HTTP 服务器直收 Claude Code 的请求，
// 见 /private/tmp/ccdump*.log）。头是原样抄的**全集**，body 的形态（system 为
// 长度 2 的数组、首块是 Agent SDK 身份 prose、metadata.user_id 是内嵌 JSON 串且
// account_uuid 为空串、max_tokens=32000）也是实抓值。
// 只有 device_id / session_id 换成了同形状的合成值：它们是那台机器的指纹，
// 校验逻辑只看「非空」，换值不改变任何判据。
// ─────────────────────────────────────────────────────────────────────────────

// 实抓 UA。注意后缀是 "(external, sdk-cli)" 而不是旧版的 "(external, cli)"。
const realClaudeCodeUA2_1_272 = "claude-cli/2.1.272 (external, sdk-cli)"

// 实抓 system[0].text 全文（94 字符）。这是 Agent SDK 形态的身份 prose，
// 比 2.1.78 之前的短句长，Dice 相似度必须仍然过阈值。
const realClaudeCodeAgentSDKSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."

// 实抓请求体的线上形态。用 JSON 原文而不是手拼 map：手拼的 map 形态与真客户端
// 不同，过了不代表客户过得了（见 ACCEPTANCE.yaml C31 的 negative）。
const realClaudeCode2_1_272BodyJSON = `{
  "model": "claude-haiku-4-5",
  "max_tokens": 32000,
  "temperature": 1,
  "stream": true,
  "system": [
    {"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.","cache_control":{"type":"ephemeral"}},
    {"type":"text","text":"\nYou are a Claude agent, built on Anthropic's Claude Agent SDK.\n\n# Environment\nWorking directory: /Users/example/project\nPlatform: darwin\n\n# Tools\nUse the tools available to complete the task.\n","cache_control":{"type":"ephemeral"}}
  ],
  "messages": [
    {"role":"user","content":[{"type":"text","text":"ping"}]}
  ],
  "metadata": {"user_id": "{\"device_id\":\"9f3c17ab55e24d0e8c6b1f4a20d7e8930c5a6b7d8e9f0a1b2c3d4e5f60718293\",\"account_uuid\":\"\",\"session_id\":\"4e41a0a4-2d25-49c0-8b9e-9bf1728e387f\"}"}
}`

// realClaudeCode2_1_272Request 复现实抓到的**全部**请求头，一个不少，
// 包括 Validate 不看的那些（X-Stainless-*、Accept 等）——多余的头不能让判据翻车。
func realClaudeCode2_1_272Request() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://relay.example.com/v1/messages?beta=true", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer sk-placeholder")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", realClaudeCodeUA2_1_272)
	req.Header.Set("X-Claude-Code-Session-Id", "4e41a0a4-2d25-49c0-8b9e-9bf1728e387f")
	req.Header.Set("X-Stainless-Arch", "arm64")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-OS", "MacOS")
	req.Header.Set("X-Stainless-Package-Version", "0.112.1")
	req.Header.Set("X-Stainless-Retry-Count", "0")
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Stainless-Runtime-Version", "v26.3.0")
	req.Header.Set("X-Stainless-Timeout", "600")
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14,claude-code-20250219")
	req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
	req.Header.Set("anthropic-version", "2023-06-01")
	// 实抓里这个头是小写的 "x-app"；Go 的 header map 会规范化成 "X-App"。
	req.Header.Set("x-app", "cli")
	return req
}

func realClaudeCode2_1_272Body(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(realClaudeCode2_1_272BodyJSON), &body))
	return body
}

func TestClaudeCodeValidator_RealClaudeCode2_1_272RequestPasses(t *testing.T) {
	// [[cov:CC:real-client-passes]] 真实 Claude Code 2.1.272 的完整请求
	// （实抓头全集 + 实抓 body 形态）必须通过「仅 CC」校验。
	// 逐步读数先落地，这样一旦将来红了，报告里能直接看出是哪一步翻的，
	// 而不是只知道「总判据 false」。
	validator := NewClaudeCodeValidator()
	req := realClaudeCode2_1_272Request()
	body := realClaudeCode2_1_272Body(t)

	// Step 1：UA。
	require.True(t, validator.ValidateUserAgent(realClaudeCodeUA2_1_272),
		"claudeCodeUAPattern 必须匹配实抓 UA %q", realClaudeCodeUA2_1_272)
	require.Equal(t, "2.1.272", validator.ExtractVersion(realClaudeCodeUA2_1_272))

	// Step 4.1：身份 prose 的 Dice 相似度。实抓的是 Agent SDK 长句，
	// 必须落在 systemPromptThreshold 之上（模板表里有精确项时为 1.0）。
	require.GreaterOrEqual(t,
		validator.bestSimilarityScore(realClaudeCodeAgentSDKSystemPrompt),
		systemPromptThreshold,
		"Agent SDK 身份 prose 的最佳相似度低于 systemPromptThreshold，门会误拒真客户端")
	require.True(t, validator.IncludesClaudeCodeSystemPrompt(body),
		"hasClaudeCodeSystemPrompt 没在实抓 system 数组里认出身份 prose")

	// Step 4.3：metadata.user_id 是内嵌 JSON 串，account_uuid 为空串。
	metadata, ok := body["metadata"].(map[string]any)
	require.True(t, ok)
	rawUserID, ok := metadata["user_id"].(string)
	require.True(t, ok)
	parsedUserID := ParseMetadataUserID(rawUserID)
	require.NotNil(t, parsedUserID, "ParseMetadataUserID 解不出实抓的 user_id")
	require.True(t, parsedUserID.IsNewFormat)
	require.NotEmpty(t, parsedUserID.DeviceID)
	require.NotEmpty(t, parsedUserID.SessionID)
	require.Empty(t, parsedUserID.AccountUUID, "实抓里 account_uuid 就是空串，它不能成为拒绝理由")

	// 反例保护：放行不能是探测请求豁免顺带给的。实抓 max_tokens=32000，
	// 既不满足 isMaxTokensOneBody，context 里也没有探测标记，
	// 所以下面的 Validate 只可能是走完整的严格校验过的。
	require.False(t, isMaxTokensOneBody(body), "实抓 max_tokens=32000，不该被当成 max_tokens=1 探测")
	probeFlag, probeFlagSet := IsMaxTokensOneHaikuRequestFromContext(req.Context())
	require.False(t, probeFlagSet && probeFlag, "context 里不该有探测豁免标记")

	require.True(t, validator.Validate(req, body),
		"真实 Claude Code 2.1.272 被 ClaudeCodeValidator.Validate 判为非 Claude Code")
}

func TestClaudeCodeValidator_ImpostorsStillRejected(t *testing.T) {
	// [[cov:CC:impostor-still-fails]] 伪造者仍被拒 —— 放真客户端进来不能顺手把门拆了。
	// 每个用例都从**同一份实抓夹具**出发，只改一处：这样"被拒"只能归因到被改的
	// 那一处，而不是夹具本身就不合格。
	validator := NewClaudeCodeValidator()

	t.Run("ua_ok_but_no_system", func(t *testing.T) {
		body := realClaudeCode2_1_272Body(t)
		delete(body, "system")
		require.False(t, validator.IncludesClaudeCodeSystemPrompt(body))
		require.False(t, validator.Validate(realClaudeCode2_1_272Request(), body),
			"UA 对但 body 里没有 system，仍必须被拒")
	})

	t.Run("ua_ok_but_foreign_system_prompt", func(t *testing.T) {
		// 实测读数（2026-09-16）：这句 0.4237，在 systemPromptThreshold=0.5 之下。
		// 注意选材：随手写的 "You are a helpful assistant..." 实测 0.5564，
		// 反而**过得了**这道门（模板表里有 "You are a helpful AI assistant tasked
		// with summarizing conversations."），拿它当伪造样本会把测试写成谎报。
		const foreign = "You are ChatGPT, a large language model trained by OpenAI."
		// 先证明这段文本确实过不了相似度门，再证明 Validate 因此拒绝——
		// 否则"被拒"可能是别的原因。
		require.Less(t, validator.bestSimilarityScore(foreign), systemPromptThreshold)
		require.False(t, strings.HasPrefix(foreign, claudeCodeBillingHeaderPrefix))

		body := realClaudeCode2_1_272Body(t)
		body["system"] = []any{map[string]any{"type": "text", "text": foreign}}
		require.False(t, validator.IncludesClaudeCodeSystemPrompt(body))
		require.False(t, validator.Validate(realClaudeCode2_1_272Request(), body),
			"UA 对但 system prompt 对不上，仍必须被拒")
	})

	t.Run("ua_ok_but_metadata_user_id_malformed", func(t *testing.T) {
		for _, bad := range []string{
			"not-a-user-id", // 两种格式都不是
			`{"device_id":"","account_uuid":"","session_id":"4e41a0a4-2d25-49c0-8b9e-9bf1728e387f"}`, // device_id 空
			`{"device_id":"9f3c17ab","account_uuid":"","session_id":""}`,                             // session_id 空
			`{"device_id":"9f3c17ab","account_uuid":"",`,                                             // 截断的 JSON
			"", // 干脆没有
		} {
			require.Nil(t, ParseMetadataUserID(bad), "ParseMetadataUserID 不该接受 %q", bad)

			body := realClaudeCode2_1_272Body(t)
			body["metadata"] = map[string]any{"user_id": bad}
			require.False(t, validator.Validate(realClaudeCode2_1_272Request(), body),
				"UA 对但 metadata.user_id=%q 非法，仍必须被拒", bad)
		}
	})

	t.Run("ua_ok_but_metadata_missing", func(t *testing.T) {
		body := realClaudeCode2_1_272Body(t)
		delete(body, "metadata")
		require.False(t, validator.Validate(realClaudeCode2_1_272Request(), body),
			"UA 对但整个 metadata 缺失，仍必须被拒")
	})

	t.Run("perfect_body_but_non_claude_cli_ua", func(t *testing.T) {
		body := realClaudeCode2_1_272Body(t)
		// 这份 body 是真客户端的，上一个测试证明它自己能过；这里只换 UA。
		for _, ua := range []string{
			"curl/8.7.1",
			"Go-http-client/1.1",
			"python-requests/2.32.3",
			"claude-cli", // 没有版本号
			"x-claude-cli/2.1.272 (external, sdk-cli)", // 前缀不在开头
			"",
		} {
			require.False(t, validator.ValidateUserAgent(ua), "ValidateUserAgent 不该接受 %q", ua)
			req := realClaudeCode2_1_272Request()
			req.Header.Set("User-Agent", ua)
			require.False(t, validator.Validate(req, body),
				"body 完美但 UA=%q 不是官方 CLI，仍必须被拒", ua)
		}
	})

	t.Run("ua_ok_but_required_headers_stripped", func(t *testing.T) {
		body := realClaudeCode2_1_272Body(t)
		for _, header := range []string{"X-App", "anthropic-beta", "anthropic-version"} {
			req := realClaudeCode2_1_272Request()
			req.Header.Del(header)
			require.False(t, validator.Validate(req, body),
				"缺少必需头 %s 时仍必须被拒", header)
		}
	})
}
