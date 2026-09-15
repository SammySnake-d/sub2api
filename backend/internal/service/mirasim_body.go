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

// mirasimAgentSystemLine is the exact text mirasim expects to lead the system
// block. It is a wire constant, not a prompt we are free to reword.
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
		if strings.TrimSpace(s) == "" || s == mirasimAgentSystemLine {
			out, err := sjson.SetRawBytes(body, "system", []byte("["+mirasimAgentSystemBlockRaw+"]"))
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
		if len(elems) > 0 && elems[0].Get("text").String() == mirasimAgentSystemLine {
			// Already canonical. Returning the input untouched is the whole
			// point: re-serialising here would churn the prefix on every retry.
			return body
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

// shapeMirasimRequestBody applies every mirasim-specific body requirement.
// Non-mirasim accounts are returned untouched.
func shapeMirasimRequestBody(account *Account, body []byte) []byte {
	if !IsMirasimAccount(account) {
		return body
	}
	_ = mirasim.ClientVersion // keep the wire-protocol package linked to this decision point
	return ensureMirasimAgentSystemPrompt(body)
}
