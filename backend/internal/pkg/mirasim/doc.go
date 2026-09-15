// Package mirasim implements the device-signature protocol of the mirasim relay
// (https://relay.mirasim.ai), an Anthropic-protocol upstream that authenticates
// every request with a per-request ed25519 device signature instead of a static
// API key.
//
// # What the upstream requires
//
// Five headers travel with each call. Only x-mirasim-client is cleartext; the
// other four (device, ts, nonce, sig) plus every x-mirasim-* context header
// (session, agent, call, locale, account, probe) are collected into a JSON
// object, sealed with X25519 + HKDF-SHA256 + ChaCha20-Poly1305 to a fixed relay
// public key, and sent as a single x-mirasim-enc blob. The signature covers
//
//	METHOD \0 path \0 ts \0 nonce \0 deviceId \0 clientVersion \0 credential
//
// plus the request body and plus a "meta" string derived from the context
// headers. The PATH ONLY — the query string is not signed, so "?beta=true"
// passes through untouched.
//
// The signing itself runs inside the upstream client's own crypto-core WASM
// module (subpackage ccore, an unmodified copy including the .wasm binary,
// hosted with the pure-Go wazero runtime). Running the original module is what
// guarantees byte-identical signatures rather than a hand re-derivation of the
// construction; see differential_test.go for the gate that proves it.
//
// # How it plugs into sub2api
//
// mirasim speaks the Anthropic protocol, so it is NOT a new platform and NOT a
// new forwarder. It reuses platform=anthropic end to end, and the signature is
// injected by a decorator over service.HTTPUpstream
// (internal/repository/mirasim_upstream.go) — the one seam that receives both a
// fully-formed *http.Request (headers AND body) and the account id, which is
// exactly the input set signing needs. Non-mirasim accounts are delegated
// untouched.
//
// # Account configuration
//
// A mirasim account is an ordinary Anthropic API-key passthrough account:
//
//	platform = "anthropic"
//	type     = "apikey"
//	extra.anthropic_passthrough = true    // byte-for-byte body passthrough;
//	                                      // required, see "cache prefix" below
//	credentials = {
//	  "provider":            "mirasim",   // the ONLY switch the decorator reads
//	  "mirasim_device_seed": "<base64 raw-std 32-byte ed25519 seed>",
//	  "access_token":        "<jwt>",
//	  "refresh_token":       "<jwt>",
//	  "expires_at":          "<RFC3339>",
//	  "mirasim_auth_base":   "https://auth.mirasim.ai",   // optional
//	  "base_url":            "https://relay.mirasim.ai",
//	  "api_key":             "mirasim-signed"             // placeholder; the
//	                                                      // decorator replaces
//	                                                      // the auth header
//	}
//	extra = {
//	  "mirasim_session_id": "session_<hex>"   // minted on first use
//	}
//
// Everything lives in the existing accounts.credentials JSONB column rather than
// in new columns. That column is already the per-account secret and knob bag
// (api_key, access_token/refresh_token/expires_at for OAuth accounts, pool_mode
// and custom_error_codes for behaviour), and it already has the repository write
// path (AccountRepository.UpdateCredentials) that token rotation needs. The only
// genuinely new field is the device seed, which no existing column models — one
// field does not justify a migration.
//
// The device seed MUST be persistent. The upstream binds the derived device id
// to the account at /v1/device/session, so regenerating the seed presents a new
// device on every restart. It lives in credentials, not extra, because the admin
// DTO layer redacts credentials and does not redact extra.
//
// The session id must be durable for a different reason: without it every
// account mints a fresh x-mirasim-session at process start, so the entire pool
// rotates session ids in lockstep on every restart — a correlation signal no set
// of independently installed clients produces. It lives in extra because it is
// not a secret and because every credentials write on an apikey account drops
// extra.upstream_billing_probe (see account_repo.go UpdateCredentials).
//
// Tokens, seeds and tickets are secrets: nothing in this package logs them, and
// nothing should.
//
// # Credential lifecycle
//
// Access tokens live ~50 minutes. Registry.Prepare refreshes within 5 minutes of
// expiry against the account's auth_base, persists the rotation (the refresh
// token rotates too), then mints a short-lived device ticket from the relay and
// signs with THAT. The whole sequence runs under one per-account mutex, so N
// concurrent requests produce one refresh and one mint. A failed mint is not an
// error: the access token is itself a valid credential.
//
// # Things the upstream enforces that are easy to get wrong
//
//   - The body must look like real Claude Code. Without the canonical first
//     system block ("You are Claude Code, Anthropic's official CLI for Claude.")
//     the relay answers 400 "the request was rejected as invalid"; with a
//     near-miss block it answers 403 "the request was rejected". Both were
//     reproduced with ma-relay's own signing code, so neither is a signature
//     fault. A genuine Claude Code client sends this; a bare API client does not.
//   - A rejected signature is 401 (error.code "device_signature"), NOT 403. That
//     matters because sub2api treats 403 as "account banned → disable": a
//     signing regression will therefore surface as an auth error rather than
//     silently disabling a fleet of accounts.
//   - anthropic-beta is coupled to the body, so this package does not touch it.
//     The caller's set is forwarded exactly as sub2api's passthrough whitelist
//     produced it.
//   - Cache prefix: the decorator never rewrites the body. Anything that
//     round-trips the body through map[string]any would reorder keys and destroy
//     the upstream prompt-cache prefix, which is the most expensive failure mode
//     in this system.
//   - Redirects must not be followed: the signature covers the path, so any hop
//     arrives unsigned for its own path. The decorator marks every mirasim
//     request with service.WithHTTPUpstreamRedirectsDisabled.
//   - TLS: mirasim requests go out with tlsfingerprint.MirasimProfile()
//     (Mirasim.app's Electron ClientHello, JA3
//     71dc8c533dd919ae9f4963224a4ba8fd), overriding whatever profile the caller
//     resolved — sub2api's default is the Claude Code CLI fingerprint, which
//     does not belong under a mirasim device signature.
package mirasim
