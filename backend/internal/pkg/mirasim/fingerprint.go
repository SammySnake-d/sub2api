package mirasim

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"net/http"
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
// ApplyCanonicalIdentityHeaders 把版本相关的身份头写进 h，覆盖调用方带来的任何值。
//
// **它必须是这套画像的唯一落点。** 在它存在之前，canonical 头只挂在两条**网关**路径上
// （gateway_upstream_request.go 与 gateway_anthropic_passthrough.go），于是任何不走网关的
// mirasim 请求都会拿到 service.defaultFingerprint 那份陈旧值。实测后果（2026-09-16 线上
// usage_logs）：同一批账号的请求里，客户流量是
//
//	claude-cli/2.1.272 (external, sdk-cli) + MacOS + 0.112.1 + v26.3.0
//
// 而 sub2api 自己发的账号健康检查是
//
//	claude-cli/2.1.272 (external, cli)     + Linux + 0.94.0  + v24.3.0
//
// —— 上游看到的是**一台 Mac 和一台 Linux 交替在用同一个账号**，SDK 版本还差了一大截。
// 这正是整套「一号一指纹、同号对上游表现为同设备」要防的事，而它是静默的：
// 健康检查照样 200，没有任何信号。
//
// 所以这个函数的调用点被刻意收到了传输层的签名收口（repository.mirasimUpstream.sign），
// 那是每一个 mirasim 请求的必经之路。**不要在别处再调它一次** —— 多一个调用点，
// 就多一条将来会被漏掉的路径，而漏掉的代价是静默的。
func ApplyCanonicalIdentityHeaders(h http.Header) {
	if h == nil {
		return
	}
	for k, v := range CanonicalIdentityHeaders() {
		h.Set(k, v)
	}
}

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
