package mirasim

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

// NOTE ON CLIENT-IDENTITY NORMALISATION — deliberately NOT implemented here.
//
// ma-relay additionally overwrites the version-bearing identity headers
// (user-agent, the x-stainless-* set) with one canonical Claude Code snapshot,
// so a whole population of accounts presents a single coherent client version.
// That behaviour is intentionally NOT ported: the operator scoped this batch to
// "restrict inbound to real Claude Code clients + normalise the version", and
// version normalisation is being added separately as a narrow hook over
// user-agent / x-stainless-package-version / x-stainless-runtime-version /
// x-stainless-os / x-stainless-arch.
//
// It must NOT be obtained by enabling sub2api's existing mimic path: that path
// also rewrites the body (gateway_claude_oauth_body.go
// rewriteSystemForNonClaudeCodeWithPromptBlocks replaces `system` wholesale and
// relocates the caller's system prompt into messages[0]/[1]), which destroys the
// upstream prompt-cache prefix.
//
// Consequence, stated so it is not mistaken for an oversight: this package does
// not touch user-agent, x-stainless-*, x-app, anthropic-version or
// anthropic-beta at all. Whatever sub2api's allowedHeaders whitelist forwarded
// is what gets signed and sent.
//
// For reference when that hook is written, ma-relay's captured self-consistent
// snapshot (Claude Code 2.1.272, real request headers) is:
//
//	user-agent                  claude-cli/2.1.272 (external, sdk-cli)
//	x-stainless-package-version 0.112.1
//	x-stainless-runtime-version v26.3.0
//	x-stainless-os              MacOS
//	x-stainless-arch            arm64
//
// The values must be taken as ONE captured set: a combination that never
// shipped is more incriminating than a stale one.
