package service

// mirasim body shaping.
//
// mirasim rejects a /v1/messages request whose `system` does not open with the
// Claude Agent SDK identity line — the upstream answers a bare
// `{"error":{"message":"the request was rejected as invalid"}}` with no hint as
// to which field was wrong. This was confirmed empirically on 2026-09-16: the
// identical request differing ONLY by the presence of that system block went
// from 400 to 200 with real content and usage.
//
// ma-relay carries the same logic (ensureClaudeAgentSystemPrompt), but its
// implementation unmarshals the whole body into map[string]any and re-marshals
// it. That is not byte-stable — Go sorts object keys, escapes < > &, and routes
// numbers through float64 — and Anthropic's prompt cache is a linear prefix
// match over tools → system → messages. So this port does the same job with
// surgical sjson edits: every pre-existing system element keeps its original
// bytes, and only one element is prepended.

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// mirasimAgentSystemLine is the identity line this gateway injects when the
// caller brought none of its own.
//
// It is NOT "the" line mirasim requires — six live probes (2026-09-16) showed
// the upstream accepts a SET of known identity lines, including the one real
// Claude Code sends. What it rejects is an unknown line, even one containing
// the exact substring "Claude Agent SDK". See isKnownClaudeIdentityPrompt.
const mirasimAgentSystemLine = "You are a Claude agent, built on Anthropic's Claude Agent SDK."

// mirasimAgentSystemBlockRaw is the pre-serialised block, kept as a literal so
// the emitted bytes never depend on Go's map ordering.
const mirasimAgentSystemBlockRaw = `{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."}`

// ensureMirasimAgentSystemPrompt guarantees the outbound body's `system` opens
// with the agent identity line.
//
// It is idempotent: a body already in that shape is returned unchanged (byte for
// byte), so a retry cannot stack the line twice — which would itself move the
// cache prefix and force a full rebuild.
func ensureMirasimAgentSystemPrompt(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	if !strings.HasPrefix(model, "claude-") {
		return body
	}

	sys := gjson.GetBytes(body, "system")

	switch {
	case !sys.Exists():
		out, err := sjson.SetRawBytes(body, "system", []byte("["+mirasimAgentSystemBlockRaw+"]"))
		if err != nil {
			return body
		}
		return out

	case sys.Type == gjson.String:
		s := sys.String()
		if strings.TrimSpace(s) == "" {
			out, err := sjson.SetRawBytes(body, "system", []byte("["+mirasimAgentSystemBlockRaw+"]"))
			if err != nil {
				return body
			}
			return out
		}
		if isKnownClaudeIdentityPrompt(s) {
			// Already an accepted identity line — promote it to the block form
			// the upstream is known to take, without prepending a second,
			// contradicting one.
			promoted, err := sjson.SetBytes([]byte(`{"type":"text"}`), "text", s)
			if err != nil {
				return body
			}
			out, err := sjson.SetRawBytes(body, "system", buildJSONArrayRaw([][]byte{promoted}))
			if err != nil {
				return body
			}
			return out
		}
		// Keep the caller's text as its own block rather than concatenating:
		// concatenation would change the caller's bytes, and the cache prefix
		// with them.
		second, err := sjson.SetBytes([]byte(`{"type":"text"}`), "text", s)
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "system",
			buildJSONArrayRaw([][]byte{[]byte(mirasimAgentSystemBlockRaw), second}))
		if err != nil {
			return body
		}
		return out

	case sys.IsArray():
		elems := sys.Array()
		// The upstream scans EVERY block, not just the first (probe P2: an
		// identity line sitting in block[1] behind an unrelated block was
		// accepted). Matching that here means a client which puts its identity
		// line second does not get a redundant block prepended.
		for _, e := range elems {
			if isKnownClaudeIdentityPrompt(e.Get("text").String()) {
				// The caller already carries an identity line the upstream
				// accepts. Injecting would add a SECOND, contradicting claim:
				// system[0] saying "I am the Agent SDK" ahead of the client's
				// own "I am the Claude Code CLI". Returning the input untouched
				// is also what makes this idempotent across retries.
				return body
			}
		}
		// buildJSONArrayRaw is a plain append of raw element bytes — it never
		// goes through json.Marshal, so the caller's blocks (including their
		// cache_control breakpoints and any < > & in the text) survive verbatim.
		items := make([][]byte, 0, len(elems)+1)
		items = append(items, []byte(mirasimAgentSystemBlockRaw))
		for _, e := range elems {
			items = append(items, []byte(e.Raw))
		}
		out, err := sjson.SetRawBytes(body, "system", buildJSONArrayRaw(items))
		if err != nil {
			return body
		}
		return out
	}

	return body
}

// isKnownClaudeIdentityPrompt reports whether text is one of the identity lines
// the mirasim upstream accepts.
//
// The upstream's rule was established empirically (2026-09-16, six probes
// against the live endpoint, each observing its own request_id):
//
//	system: null                                                     -> 400
//	"You are a Claude agent, built on Anthropic's Claude Agent SDK."  -> 200
//	"You are Claude Code, …CLI for Claude, running within the …SDK."  -> 200  (what real CC sends)
//	"You are Claude Code, Anthropic's official CLI for Claude."       -> 200
//	"You are a helpful assistant, built on Anthropic's Claude Agent SDK." -> 400
//	"You are a helpful assistant."                                    -> 400
//
// The fifth probe is the decisive one: it contains the exact substring
// "Claude Agent SDK" and is still rejected. So the rule is NOT a substring
// test and NOT "any non-empty system" — it is enumeration against a set of
// known identity lines. All three accepted lines are already present in
// sub2api's own claudeCodeSystemPrompts, which is why this reuses that table
// rather than starting a second, drift-prone copy.
//
// Matching is exact (after trimming) rather than fuzzy: a substring or
// similarity rule would let through the very string the upstream rejected.
// If the table is missing a line the upstream would have accepted, the worst
// case is the old behaviour — one redundant injected block — not a rejection.
func isKnownClaudeIdentityPrompt(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	for _, known := range claudeCodeSystemPrompts {
		if t == strings.TrimSpace(known) {
			return true
		}
	}
	return false
}

// shapeMirasimRequestBody applies every mirasim-specific body requirement.
// Non-mirasim accounts are returned untouched.
func shapeMirasimRequestBody(account *Account, body []byte) []byte {
	if !IsMirasimAccount(account) {
		return body
	}
	_ = mirasim.ClientVersion // keep the wire-protocol package linked to this decision point
	return ensureMirasimAgentSystemPrompt(body)
}
