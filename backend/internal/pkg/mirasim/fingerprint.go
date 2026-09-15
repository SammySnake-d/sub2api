package mirasim

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

// ClientVersion is the mirasim client version stamped into x-mirasim-client AND
// into the sixth field of the canonical signing string, so it is covered by the
// signature.
//
// DO NOT BUMP THIS ON ITS OWN. The version number and the signing scheme are a
// matched pair: this port implements the scheme that ma-relay runs in production
// with 0.0.307. The current Mirasim.app release is 0.0.326 and uses a different
// scheme (mrs-sig-v2). Claiming 0.0.326 while signing the 0.0.307 way is a
// self-inconsistent combination and is EASIER to spot than an old version
// number, because the server can infer the expected signature shape from the
// version it was told. Moving to mrs-sig-v2 is a separate, independently
// canaried change.
const ClientVersion = "0.0.307"

// DefaultRelayBase is the mirasim relay endpoint. Accounts override it via their
// credentials base_url.
const DefaultRelayBase = "https://relay.mirasim.ai"

// NOTE ON CLIENT-IDENTITY NORMALISATION.
//
// sub2api decides whether to impersonate Claude Code with
//
//	shouldMimicClaudeCode := account.IsOAuth() && !isClaudeCode
//
// (gateway_forward.go). Read it twice: a request that IS a real Claude Code
// client takes the *false* branch and its own version-bearing headers are
// forwarded verbatim through the allowedHeaders whitelist. So restricting a
// group to "Claude Code clients only" switches version normalisation OFF
// exactly when it is needed.
//
// For an upstream that treats one account as one device that matters. Customer
// A on 2.1.100 and customer B on 2.9.0 hitting the same account make that
// device's client version jump — and, when B is the older one, go backwards.
// Real installs only move forwards.
//
// So mirasim requests get the version-bearing identity headers rewritten to one
// captured snapshot, below. This is a NARROW hook: user-agent plus the four
// version/platform x-stainless-* headers. It deliberately does NOT touch
// anthropic-beta (coupled to the body — rewriting it yields opaque 400s) nor
// x-claude-code-session-id (the sticky-session key).
//
// It is emphatically NOT obtained by enabling sub2api's existing mimic path:
// that path also rewrites the body — gateway_claude_oauth_body.go's
// rewriteSystemForNonClaudeCodeWithPromptBlocks replaces `system` wholesale and
// relocates the caller's system prompt into messages[0]/[1] — which destroys
// the upstream prompt-cache prefix. That exact defect cost a sibling gateway 8%
// of requests dropping from >90% cache hit to 0%.
const (
	// canonicalUserAgentSuffix is what a real Claude Code 2.1.272 sends. Note
	// "sdk-cli", not sub2api's built-in "cli".
	canonicalUserAgentSuffix = " (external, sdk-cli)"

	// The x-stainless values below were captured from ONE real Claude Code
	// 2.1.272 request. They must move as a set: a combination that never
	// shipped is more incriminating than a stale one, because a stale version
	// is indistinguishable from a user who has not upgraded, while an
	// impossible pairing can only have been assembled.
	canonicalStainlessPackageVersion = "0.112.1"
	canonicalStainlessRuntimeVersion = "v26.3.0"
	canonicalStainlessOS             = "MacOS"
	canonicalStainlessArch           = "arm64"
	canonicalStainlessLang           = "js"
	canonicalStainlessRuntime        = "node"
)

// CanonicalIdentityHeaders returns the version-bearing identity headers that a
// mirasim request must carry, as ONE captured set.
//
// It returns values rather than mutating a header map because sub2api writes
// headers through its own wire-casing helpers (setHeaderRaw/resolveWireCasing)
// to bypass Go's canonical MIME casing; writing directly here would create a
// second copy under a different spelling, and a duplicated identity header is
// itself a tell.
//
// Keys are in the casing sub2api's own claude.DefaultHeaders uses, so callers
// can pass them straight through resolveWireCasing.
func CanonicalIdentityHeaders() map[string]string {
	return map[string]string{
		// claude.CLIVersion() is the process-wide pin (env-overridable upwards
		// only, resolved once at init so a single request can never report two
		// different versions). Taken from there rather than hardcoded so this
		// stays in step with the rest of sub2api.
		"User-Agent":                  "claude-cli/" + claude.CLIVersion() + canonicalUserAgentSuffix,
		"X-Stainless-Package-Version": canonicalStainlessPackageVersion,
		"X-Stainless-Runtime-Version": canonicalStainlessRuntimeVersion,
		"X-Stainless-OS":              canonicalStainlessOS,
		"X-Stainless-Arch":            canonicalStainlessArch,
		"X-Stainless-Lang":            canonicalStainlessLang,
		"X-Stainless-Runtime":         canonicalStainlessRuntime,
	}
}
