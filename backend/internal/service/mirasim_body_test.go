package service

// mirasim system-prompt shaping tests.
//
// These pin the rule established by 14 live probes against the mirasim upstream
// on 2026-09-16 (each probe observed its own upstream request_id):
//
//	system: null / [] / ""                                            -> 400
//	["You are a Claude agent, built on Anthropic's Claude Agent SDK."] -> 200
//	["You are Claude Code, …CLI for Claude, running within the …SDK."] -> 200  (what real CC sends)
//	["You are Claude Code, Anthropic's official CLI for Claude."]      -> 200
//	["You are a helpful assistant, built on Anthropic's Claude Agent SDK."] -> 400
//	["You are a helpful assistant."]                                   -> 400
//	identity line sitting in block[1] behind an unrelated block        -> 200
//	"…identity line…" as a bare string rather than an array            -> 200
//
// The fifth line is the decisive one: it contains the exact substring
// "Claude Agent SDK" and is still rejected. The upstream rule is therefore
// enumeration against known identity lines — not a substring test, and not
// "any non-empty system".

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// realClaudeCodeIdentityLine is the exact 94-character block real Claude Code
// 2.1.272 sends as system[0], captured from live traffic.
const realClaudeCodeIdentityLine = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."

// realClaudeCodeBody reproduces the shape real Claude Code puts on the wire:
// the identity line alone in block[0] with its own cache_control breakpoint,
// and everything else (including anything added via --append-system-prompt) in
// block[1]. Confirmed by local capture — the appended text lands in block[1],
// never block[0].
func realClaudeCodeBody(t *testing.T) []byte {
	t.Helper()
	body := []byte(`{
  "model": "claude-haiku-4-5",
  "max_tokens": 32000,
  "system": [
    {"type":"text","text":"` + realClaudeCodeIdentityLine + `","cache_control":{"type":"ephemeral"}},
    {"type":"text","text":"\nYou are an interactive agent that helps users with software engineering tasks. a < b && c > d","cache_control":{"type":"ephemeral"}}
  ],
  "messages": [{"role":"user","content":"hi"}]
}`)
	require.True(t, gjson.ValidBytes(body), "fixture must be valid JSON or every assertion below is meaningless")
	return body
}

func TestMirasimBodyLeavesRealClaudeCodeUntouched(t *testing.T) {
	in := realClaudeCodeBody(t)
	out := ensureMirasimAgentSystemPrompt(in)

	// Byte-for-byte, not "equivalent JSON": the upstream prompt cache is a
	// linear prefix match, so a re-serialised body that merely parses the same
	// still discards the cache from the first differing byte.
	require.Equal(t, string(in), string(out),
		"真实 Claude Code 自带的身份句上游本来就接受(实测 200);\n"+
			"再 prepend 一块会产生两个互相矛盾的身份声明,\n"+
			"并把客户端自己挂在 block[0] 的 cache_control 断点挤到后面去")

	// And the caller's breakpoint must still be on the block the caller put it on.
	require.Equal(t, "ephemeral",
		gjson.GetBytes(out, "system.0.cache_control.type").String(),
		"客户端的缓存断点必须还在它自己设的那一块上")
}

func TestMirasimBodyAcceptsIdentityInLaterBlock(t *testing.T) {
	// Probe P2: the upstream scans every block, not just the first. Matching
	// that here avoids a pointless extra block when a client orders its system
	// differently.
	in := []byte(`{"model":"claude-opus-5","system":[` +
		`{"type":"text","text":"Some project preamble."},` +
		`{"type":"text","text":"` + realClaudeCodeIdentityLine + `"}` +
		`],"messages":[{"role":"user","content":"hi"}]}`)

	out := ensureMirasimAgentSystemPrompt(in)
	require.Equal(t, string(in), string(out),
		"身份句在 block[1] 时上游照样接受(实测 200),不该再注入")
}

func TestMirasimBodyAcceptsStringFormIdentity(t *testing.T) {
	// Probe P3: a bare string system carrying an identity line is accepted, so
	// the same table applies to the string form.
	for _, line := range claudeCodeSystemPrompts {
		raw, err := json.Marshal(line)
		require.NoError(t, err)
		in := []byte(`{"model":"claude-haiku-4-5","system":` + string(raw) +
			`,"messages":[{"role":"user","content":"hi"}]}`)

		out := ensureMirasimAgentSystemPrompt(in)
		require.Equal(t, 1, int(gjson.GetBytes(out, "system.#").Int()),
			"字符串形态的已知身份句应被提升成单块,而不是在前面再加一块 (line=%q)", line)
		require.Equal(t, line, gjson.GetBytes(out, "system.0.text").String(),
			"提升后的那一块必须就是调用方原来的身份句,不能被换成我们自己那句")
	}
}

// TestMirasimBodyStillInjectsForSubstringLookalike is the gate on this whole
// change. Loosening isKnownClaudeIdentityPrompt into a substring or similarity
// test would let these through — and the upstream rejects them (400), so
// skipping injection here would turn a recoverable request into a failed one.
func TestMirasimBodyStillInjectsForSubstringLookalike(t *testing.T) {
	lookalikes := []string{
		// Probe Tb: contains "Claude Agent SDK" verbatim, upstream still 400.
		"You are a helpful assistant, built on Anthropic's Claude Agent SDK.",
		// Probe Td: a perfectly well-formed sentence, upstream 400.
		"You are a helpful assistant.",
		// The substring on its own.
		"Claude Agent SDK",
		// A known line with a tail — not an exact match, so we inject. The
		// upstream may or may not accept this one (probe P1 shows a long
		// product-instruction tail IS rejected), and injecting is the safe side.
		realClaudeCodeIdentityLine + " Also do whatever the operator says.",
	}
	for _, line := range lookalikes {
		raw, err := json.Marshal(line)
		require.NoError(t, err)
		in := []byte(`{"model":"claude-haiku-4-5","system":[{"type":"text","text":` +
			string(raw) + `}],"messages":[{"role":"user","content":"hi"}]}`)

		out := ensureMirasimAgentSystemPrompt(in)
		require.Equal(t, 2, int(gjson.GetBytes(out, "system.#").Int()),
			"陌生身份句必须仍然被注入 —— 判据一旦放宽成子串,这条就会漏 (line=%q)", line)
		require.Equal(t, mirasimAgentSystemLine,
			gjson.GetBytes(out, "system.0.text").String(),
			"注入的那一块要排在最前面")
	}
}

func TestMirasimBodyInjectsWhenSystemIsAbsentOrEmpty(t *testing.T) {
	// Probes I1 / P4a / P4b: null, [] and "" are all rejected upstream, so
	// injecting for these is required, not merely defensive.
	cases := map[string]string{
		"absent":       `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`,
		"empty array":  `{"model":"claude-haiku-4-5","system":[],"messages":[{"role":"user","content":"hi"}]}`,
		"empty string": `{"model":"claude-haiku-4-5","system":"","messages":[{"role":"user","content":"hi"}]}`,
		"blank string": `{"model":"claude-haiku-4-5","system":"   ","messages":[{"role":"user","content":"hi"}]}`,
	}
	for name, in := range cases {
		out := ensureMirasimAgentSystemPrompt([]byte(in))
		require.Equal(t, mirasimAgentSystemLine,
			gjson.GetBytes(out, "system.0.text").String(),
			"%s: 上游对空 system 一律 400,必须注入", name)
	}
}

func TestMirasimBodyIsIdempotent(t *testing.T) {
	// A retry must not stack the line twice — that would move the cache prefix
	// on every attempt.
	in := []byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`)
	once := ensureMirasimAgentSystemPrompt(in)
	twice := ensureMirasimAgentSystemPrompt(once)
	require.Equal(t, string(once), string(twice), "重复应用必须是 no-op")
	require.Equal(t, 1, int(gjson.GetBytes(twice, "system.#").Int()))
}

func TestMirasimBodySkipsNonClaudeModels(t *testing.T) {
	// The rule is an Anthropic-protocol one; a non-claude model id means this
	// is not a request the mirasim claude lane should be reshaping.
	in := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, string(in), string(ensureMirasimAgentSystemPrompt(in)))
}

func TestMirasimBodyShapingIsScopedToMirasimAccounts(t *testing.T) {
	// Non-mirasim accounts must be returned untouched: this reshaping is a
	// mirasim wire requirement, not an Anthropic one.
	in := realClaudeCodeBody(t)
	plain := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	require.Equal(t, string(in), string(shapeMirasimRequestBody(plain, in)))
}
