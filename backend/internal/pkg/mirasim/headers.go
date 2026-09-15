package mirasim

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// agentForUpstreamPath maps an upstream path to the x-mirasim-agent value the
// real client pairs with it: /v1/responses is the codex (OpenAI Responses) lane,
// /v1/chat/completions is the kimi lane, and everything else (/v1/messages,
// count_tokens, limits) is the claude lane. The agent travels inside the seal
// and the signed meta, so it MUST match the path the signature covers.
//
// Ported from ma-relay internal/relay/proxy.go.
func agentForUpstreamPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/responses"), strings.HasPrefix(path, "/responses"):
		return "codex"
	case strings.HasPrefix(path, "/v1/chat/completions"), strings.HasPrefix(path, "/chat/completions"):
		return "kimi"
	default:
		return "claude"
	}
}

// perAccountLocales is the realistic locale set the per-account locale is drawn
// from, weighted toward en-US. Hardcoding a single locale for every account is a
// fingerprint correlator; a stable, well-spread locale per account is not.
var perAccountLocales = []struct {
	tag    string
	weight uint32
}{
	{"en-US", 40},
	{"en-GB", 14},
	{"en-CA", 10},
	{"de-DE", 9},
	{"nl-NL", 8},
	{"ja-JP", 10},
	{"fr-FR", 9},
}

var perAccountLocaleTotal = func() uint32 {
	var t uint32
	for _, l := range perAccountLocales {
		t += l.weight
	}
	return t
}()

// LocaleForAccount derives a STABLE locale for an account id: the same id always
// yields the same locale (across restarts and across every request), while the
// population spreads across perAccountLocales weighted toward en-US.
// Deterministic (sha256 of the id), never random per request.
//
// Ported verbatim from ma-relay internal/relay/proxy.go localeForAccount.
func LocaleForAccount(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "en-US"
	}
	sum := sha256.Sum256([]byte("mirasim-locale\x00" + id))
	n := binary.BigEndian.Uint32(sum[:4]) % perAccountLocaleTotal
	var acc uint32
	for _, l := range perAccountLocales {
		acc += l.weight
		if n < acc {
			return l.tag
		}
	}
	return "en-US"
}

func randomPrefixedID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return prefix + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return prefix + hex.EncodeToString(b)
}

// ContextInput carries everything the context-header assembly needs that is not
// derivable from the request itself.
type ContextInput struct {
	// Path is the upstream URL path (no query). It selects the agent family and
	// is the path the signature covers.
	Path string
	// SessionID is the x-mirasim-session value. Callers should prefer the
	// client's own session id and fall back to the account's stable one.
	SessionID string
	// AccountSub is the upstream account identity (the access token's JWT
	// "sub"), sent as x-mirasim-account telemetry. Empty is allowed.
	AccountSub string
	// Locale is the stable per-account locale (see LocaleForAccount).
	Locale string
}

// ApplyContextHeaders ADDS the plaintext x-mirasim-* context headers the
// protocol requires, in ma-relay's order. SignAndSeal must run AFTER this so the
// seal covers the final header set.
//
// This function only ever writes headers in the x-mirasim-* namespace (plus
// accept-encoding on the /v1/limits probe, which the real client also sets).
// It deliberately does NOT touch user-agent, x-stainless-*, anthropic-beta or
// any other caller header — see fingerprint.go for why client-identity
// normalisation is out of scope for this batch.
//
// Ported from ma-relay internal/relay/proxy.go attemptPath.
func ApplyContextHeaders(h http.Header, in ContextInput) {
	h.Set("x-mirasim-session", in.SessionID)
	// x-mirasim-agent is per-family and MUST match the upstream path the
	// signature and seal cover.
	h.Set("x-mirasim-agent", agentForUpstreamPath(in.Path))
	h.Set("x-mirasim-call", randomPrefixedID("call_"))
	h.Set("x-mirasim-locale", in.Locale)
	if in.AccountSub != "" {
		h.Set("x-mirasim-account", in.AccountSub)
	}
	if in.Path == "/v1/limits" {
		h.Set("accept-encoding", "identity")
		h.Set("x-mirasim-probe", "usage")
	}
}
