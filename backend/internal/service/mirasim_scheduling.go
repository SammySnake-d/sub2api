package service

// Mirasim scheduling semantics: status-code handling + the four-layer quota
// window model.
//
// A mirasim account is NOT a new platform. It is platform=anthropic plus the
// credential marker credentials.provider=mirasim (the same marker
// repository.isMirasimAccount keys the signing decorator off). Every function in
// this file is therefore gated on IsMirasimAccount, and the two shared files it
// hooks into (model_rate_limit.go, ratelimit_service.go) gain exactly one
// mirasim-guarded branch each. No behaviour of any other channel changes.
//
// WHY A SEPARATE WINDOW MODEL: the upstream enforces four independent quota
// windows per account, not one:
//
//	5h         global 5-hour rolling window
//	7d         global 7-day window
//	7d_claude  7-day window for the claude family (opus / sonnet / haiku)
//	7d_fable   7-day window for the fable family
//
// EVIDENCE GRADE OF THAT LIST — READ THIS BEFORE TRUSTING IT. The four-window
// SHAPE is what the mirasim operator console shows. The four STRINGS above are
// the tokens this file expects to find in the response headers, and two of them
// are guesses: see the mirasimWindowTokenEvidence table below, which is the one
// place that grading is recorded. Nothing in this repository has ever observed a
// mirasim 429 carrying "7d_claude" or "7d_fable".
//
// An opus request consumes 5h ∧ 7d ∧ 7d_claude; a fable request consumes
// 5h ∧ 7d ∧ 7d_fable. Exhausting 7d_claude must NOT stop fable traffic, and vice
// versa — which is exactly the shape sub2api's per-(account, scope) model rate
// limits already have (see anthropicFableRateLimitKey, the production precedent
// for a family-level scope). So the two global windows land on the account-level
// scalar (accounts.rate_limit_reset_at) and the two family windows land on
// scopes in accounts.extra->'model_rate_limits'. The AND across all of them is
// free: IsSchedulableForModelWithContext already requires IsSchedulable() AND
// every scope key returned by modelRateLimitKeysForRequest to be clear.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
)

// IsMirasimAccount reports whether an account is served by the mirasim upstream.
//
// It is the service-layer twin of repository.isMirasimAccount and MUST stay
// identical to it: platform=anthropic AND credentials.provider=mirasim. There is
// deliberately no PlatformMirasim constant — introducing one would drag in every
// platform whitelist in the codebase (isAllowedSchedulingThresholdPlatform and
// friends) for no gain.
func IsMirasimAccount(a *Account) bool {
	if a == nil || a.Platform != PlatformAnthropic {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.GetCredential(mirasim.CredProvider)), mirasim.ProviderMirasim)
}

// ---------------------------------------------------------------------------
// Quota windows
// ---------------------------------------------------------------------------

// The four independent quota windows the mirasim upstream enforces per account.
//
// Each constant is BOTH the internal window id AND the literal <window> token
// substituted into the Anthropic unified-ratelimit header family:
//
//	anthropic-ratelimit-unified-<window>-status
//	anthropic-ratelimit-unified-<window>-reset
//	anthropic-ratelimit-unified-<window>-utilization
//	anthropic-ratelimit-unified-<window>-surpassed-threshold
//
// That double duty is why the exact spelling matters and why every token is
// graded in mirasimWindowTokenEvidence directly below. Do not add a token here
// without adding its grade there.
const (
	// MirasimWindow5h — OBSERVED. The same "5h" token the shared anthropic path
	// has been reading in production for every anthropic account
	// (ratelimit_service.go selectAnthropicExhaustedWindow).
	MirasimWindow5h = "5h"
	// MirasimWindow7d — OBSERVED, same source as 5h.
	MirasimWindow7d = "7d"
	// MirasimWindow7dClaude — NOT OBSERVED. INFERRED, and the nearest real
	// evidence points somewhere else: see mirasimWindowTokenEvidence.
	MirasimWindow7dClaude = "7d_claude"
	// MirasimWindow7dFable — NOT OBSERVED. INFERRED, and the nearest real
	// evidence ("7d_oi") actively contradicts it: see mirasimWindowTokenEvidence.
	MirasimWindow7dFable = "7d_fable"
)

// MirasimWindowTokenGrade records HOW a window token is known.
type MirasimWindowTokenGrade string

const (
	// MirasimWindowTokenObserved: this exact string has been read off real
	// upstream responses and is already load-bearing in production.
	MirasimWindowTokenObserved MirasimWindowTokenGrade = "observed"
	// MirasimWindowTokenInferred: NOBODY HAS SEEN THIS STRING. It was derived by
	// analogy ("mirasim speaks the Anthropic protocol, the unified-ratelimit
	// header family is window-parametric, so the family window must be
	// <family>"), which is an argument about header GRAMMAR, not evidence about
	// this particular VOCABULARY.
	MirasimWindowTokenInferred MirasimWindowTokenGrade = "inferred_not_observed"
)

// mirasimWindowTokenEvidence is the SINGLE place the evidence grade of each
// window token lives. Nothing else in this file may assert a token is real.
//
// WHAT BREAKS IF AN INFERRED TOKEN IS WRONG — the whole failure chain, because
// it is silent at every step:
//
//	upstream sends anthropic-ratelimit-unified-<real>-status: rejected
//	  → isAnthropicWindowRejected(headers, "7d_fable") reads a header that does
//	    not exist and returns false
//	  → selectMirasimExhaustedWindows returns nothing
//	  → persistMirasimWindowLimits returns false
//	  → handleMirasimUpstreamError falls through to apply429FallbackRateLimit,
//	    a SECONDS-scale cooldown (defaultRateLimit429CooldownSeconds), against a
//	    window that is actually exhausted for DAYS
//	  → the scheduler re-offers the same exhausted account seconds later, burns
//	    another upstream 429, and repeats — for the rest of the window.
//
// The account is never marked, the operator sees no cooldown, and the only
// visible symptom is a 429 rate that never goes down. That is precisely why
// reportMirasimUnknownWindows exists: an unrecognized token must announce
// itself, because this failure mode cannot be inferred from its own symptoms.
var mirasimWindowTokenEvidence = map[string]MirasimWindowTokenGrade{
	MirasimWindow5h:       MirasimWindowTokenObserved,
	MirasimWindow7d:       MirasimWindowTokenObserved,
	MirasimWindow7dClaude: MirasimWindowTokenInferred,
	MirasimWindow7dFable:  MirasimWindowTokenInferred,
}

// mirasimWindowTokenCounterEvidence names, per inferred token, the strongest
// real-world evidence that the guess is WRONG.
//
//	7d_fable ← the anthropic family 7-day window is called "7d_oi" on the wire.
//	           That is not a hypothesis: this repository already parses it
//	           (ratelimit_service.go selectAnthropicFableWindowLimit,
//	           anthropicFableWindowReason) and account_usage_service.go labels
//	           UsageInfo.SevenDayFable as "7天Fable窗口（响应头 7d_oi）".
//	7d_claude ← the claude-family 7-day window is reported as "seven_day_sonnet"
//	            in the anthropic usage payload (account_usage_service.go
//	            UsageInfo.SevenDaySonnet), i.e. upstream names this window after
//	            the MODEL, not after the vendor. "7d_sonnet" is a likelier token
//	            than "7d_claude"; neither has been observed for mirasim.
var mirasimWindowTokenCounterEvidence = map[string]string{
	MirasimWindow7dFable:  "anthropic emits the fable 7d window as 7d_oi (see selectAnthropicFableWindowLimit)",
	MirasimWindow7dClaude: "anthropic names the claude-family 7d window seven_day_sonnet in its usage payload",
}

// mirasimAlternativeWindowTokens maps a token we might SEE on the wire to what
// it most likely is, so an alert carries the diagnosis instead of only the
// surprise. These are candidates, NOT tokens this file acts on: silently
// adopting "7d_oi" as the fable window would replace one unverified guess with
// another, and the whole point of the alert is that the answer comes from an
// observation rather than from a second round of reasoning.
var mirasimAlternativeWindowTokens = map[string]string{
	"7d_oi":     "anthropic's fable 7d window; if mirasim relays it, MirasimWindow7dFable (" + MirasimWindow7dFable + ") is the wrong token",
	"7d_sonnet": "claude-family 7d window named after the model; if mirasim relays it, MirasimWindow7dClaude (" + MirasimWindow7dClaude + ") is the wrong token",
	"7d_opus":   "claude-family 7d window named after the model; if mirasim relays it, MirasimWindow7dClaude (" + MirasimWindow7dClaude + ") is the wrong token",
}

// MirasimWindowTokenIsVerified reports whether the exact header token has been
// observed on a real response, as opposed to inferred from protocol grammar.
//
// An unknown window is reported as unverified: a token nobody graded is, by
// construction, not evidence.
func MirasimWindowTokenIsVerified(window string) bool {
	return mirasimWindowTokenEvidence[window] == MirasimWindowTokenObserved
}

// mirasimQuotaWindows is the evaluation order for a 429 response. The global
// windows come first so that, within a single response that rejects on more than
// one window, the account-level scalar settles on the longest cooldown (see
// shouldPersistAnthropicWindowLimit, which never shortens an existing one).
var mirasimQuotaWindows = []string{
	MirasimWindow5h,
	MirasimWindow7d,
	MirasimWindow7dClaude,
	MirasimWindow7dFable,
}

// Per-(account, scope) rate-limit keys for the two family windows. They are
// plain strings in accounts.extra->'model_rate_limits'; the repository imposes
// no schema on the scope beyond "non-empty" (account_repo.go SetModelRateLimit),
// which is what makes a family-level scope possible at all.
const (
	mirasimClaude7dRateLimitKey = "mirasim:7d_claude"
	mirasimFable7dRateLimitKey  = "mirasim:7d_fable"
)

// Model families. The family — not the individual model name — is the unit of
// quota, so every claude-family model shares ONE scope key (and therefore one
// quota state) instead of each alias accruing its own.
const (
	mirasimFamilyClaude = "claude"
	mirasimFamilyFable  = "fable"
)

// mirasimModelFamily classifies a model into the family whose 7-day window it
// consumes. Matching is substring-based for the same reason isAnthropicFableModel
// is: variants carry suffixes ("claude-fable-5[1m]", "claude-opus-4-6-thinking")
// and every variant draws on the same upstream window.
func mirasimModelFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	// Fable is checked first: a fable model name may also carry the "claude"
	// vendor prefix ("claude-fable-5"), and fable is the more specific claim.
	if strings.Contains(m, "fable") {
		return mirasimFamilyFable
	}
	if strings.Contains(m, "opus") ||
		strings.Contains(m, "sonnet") ||
		strings.Contains(m, "haiku") ||
		strings.Contains(m, "claude") {
		return mirasimFamilyClaude
	}
	return ""
}

// mirasimFamilyScope returns the model-rate-limit scope key for a model's
// family, or "" when the model belongs to no known family (such a request still
// consumes the two global windows, which are account-level).
func mirasimFamilyScope(model string) string {
	switch mirasimModelFamily(model) {
	case mirasimFamilyFable:
		return mirasimFable7dRateLimitKey
	case mirasimFamilyClaude:
		return mirasimClaude7dRateLimitKey
	}
	return ""
}

// MirasimWindowsForModel lists every window a request for model draws on. The
// account is schedulable for that model only when ALL of them are clear.
func MirasimWindowsForModel(model string) []string {
	windows := []string{MirasimWindow5h, MirasimWindow7d}
	switch mirasimModelFamily(model) {
	case mirasimFamilyFable:
		return append(windows, MirasimWindow7dFable)
	case mirasimFamilyClaude:
		return append(windows, MirasimWindow7dClaude)
	}
	return windows
}

// mirasimWindowScope maps a window id to where its cooldown is stored.
// ok=false means the window id is not one of the four.
// scope=="" means the window lands on the account-level scalar rather than a
// per-(account, scope) entry.
func mirasimWindowScope(window string) (scope string, ok bool) {
	switch window {
	case MirasimWindow5h, MirasimWindow7d:
		return "", true
	case MirasimWindow7dClaude:
		return mirasimClaude7dRateLimitKey, true
	case MirasimWindow7dFable:
		return mirasimFable7dRateLimitKey, true
	}
	return "", false
}

func mirasimWindowReason(window string) string {
	return "mirasim_" + window + "_window_exhausted"
}

// mirasimModelRateLimitKeys is the read-side hook, called from
// modelRateLimitKeysForRequest inside case PlatformAnthropic. It returns the
// family scope this request must also clear. Returning nil for a non-mirasim
// account is what keeps every other anthropic account byte-identical.
func mirasimModelRateLimitKeys(a *Account, modelKey string) []string {
	if !IsMirasimAccount(a) {
		return nil
	}
	if scope := mirasimFamilyScope(modelKey); scope != "" {
		return []string{scope}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Status-code semantics
// ---------------------------------------------------------------------------

// statusMirasimClientClosedRequest mirrors handler.statusClientClosedRequest.
// It is not an IANA status; it appears when the downstream client hung up, and
// it says nothing at all about the upstream account.
const statusMirasimClientClosedRequest = 499

// MirasimStatusAction is what a mirasim upstream status means for ACCOUNT state.
type MirasimStatusAction string

const (
	// MirasimActionShared means "mirasim has nothing special to say" — the
	// shared sub2api handling in HandleUpstreamError runs unchanged.
	MirasimActionShared MirasimStatusAction = ""
	// MirasimActionWindowCooldown: one or more quota windows are exhausted.
	MirasimActionWindowCooldown MirasimStatusAction = "window_cooldown"
	// MirasimActionRefreshCredential: the access token / device ticket is stale.
	// Recoverable — the account must NOT be disabled.
	MirasimActionRefreshCredential MirasimStatusAction = "refresh_credential"
	// MirasimActionRequestScoped: the failure belongs to THIS request (its
	// headers, its timing, its client), not to the account. Nothing may be
	// persisted, or the next request inherits a verdict that was never about it.
	MirasimActionRequestScoped MirasimStatusAction = "request_scoped"
	// MirasimActionAccountFatal: an operator-visible account state (banned, no
	// balance). Handled by the existing shared path, which disables the account.
	MirasimActionAccountFatal MirasimStatusAction = "account_fatal"
)

// mirasimAccountFatal400Markers are the three upstream 400 messages sub2api
// already treats as account-fatal (HandleUpstreamError case 400). They are
// enumerated here so the mirasim classifier agrees with the shared path instead
// of quietly diverging from it.
var mirasimAccountFatal400Markers = []string{
	"organization has been disabled",
	"credit balance",
	"identity verification is required",
}

// MirasimClassifyStatus maps an upstream status to its account-state meaning.
//
//	429 → window cooldown (which window is decided from the response headers)
//	401 → credential refresh; never a disable, the token is simply stale
//	403 → account banned. Deliberately routed to the shared path so the existing
//	      operator decision ("403 是号被禁，就应该禁用") stays exactly as it is.
//	402 → no balance. Also the shared path, which stops scheduling the account —
//	      that is what makes the NEXT request get intercepted locally instead of
//	      being spent on the upstream.
//	400 → account-fatal only for the three known messages; every other 400 is a
//	      property of this request's headers/body, not of the account.
//	503 → the model has no capacity right now. The account is healthy.
//	499 → the downstream client went away. Says nothing about anything upstream.
func MirasimClassifyStatus(statusCode int, responseBody []byte) MirasimStatusAction {
	switch statusCode {
	case http.StatusTooManyRequests:
		return MirasimActionWindowCooldown
	case http.StatusUnauthorized:
		return MirasimActionRefreshCredential
	case http.StatusForbidden, http.StatusPaymentRequired:
		return MirasimActionAccountFatal
	case http.StatusBadRequest:
		if mirasimBadRequestIsAccountFatal(responseBody) {
			return MirasimActionAccountFatal
		}
		return MirasimActionRequestScoped
	case http.StatusServiceUnavailable, statusMirasimClientClosedRequest:
		return MirasimActionRequestScoped
	}
	return MirasimActionShared
}

func mirasimBadRequestIsAccountFatal(responseBody []byte) bool {
	msg := strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(responseBody)))
	if msg == "" {
		return false
	}
	for _, marker := range mirasimAccountFatal400Markers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// mirasimVolatileHeaders are excluded from the request fingerprint because they
// change on every single request even when the request is semantically identical
// (a fresh nonce, timestamp and signature per call, and a device ticket that
// rotates on its own schedule). Including them would make every key unique and
// the fingerprint useless.
var mirasimVolatileHeaders = map[string]struct{}{
	"X-Mirasim-Ts":    {},
	"X-Mirasim-Nonce": {},
	"X-Mirasim-Sig":   {},
	"X-Mirasim-Enc":   {},
	"Authorization":   {},
	"X-Api-Key":       {},
}

// MirasimRequestFingerprint is a stable digest of the headers that can change
// how the upstream validates a request (everything except the per-call signature
// material and the bearer).
func MirasimRequestFingerprint(h http.Header) string {
	names := make([]string, 0, len(h))
	for name := range h {
		canonical := http.CanonicalHeaderKey(name)
		if _, volatile := mirasimVolatileHeaders[canonical]; volatile {
			continue
		}
		names = append(names, canonical)
	}
	sort.Strings(names)

	digest := sha256.New()
	for _, name := range names {
		values := append([]string(nil), h.Values(name)...)
		sort.Strings(values)
		digest.Write([]byte(name))
		digest.Write([]byte{0})
		for _, v := range values {
			digest.Write([]byte(v))
			digest.Write([]byte{0})
		}
		digest.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil))[:22]
}

// MirasimRequestMemoKey is the key any per-request memo of a mirasim outcome
// MUST be stored under.
//
// The header fingerprint is not optional. A sibling gateway keyed its 400 memo
// on (session, body-hash) only; one request carrying an unsupported beta header
// then poisoned every subsequent request with the same body for the whole memo
// window — 352 of 722 observed 400s in one window were local replays, and fixing
// the root cause did not stop customers from seeing 400s, because the replays
// outlived it. Same body + different headers MUST be a different key.
func MirasimRequestMemoKey(sessionID string, body []byte, h http.Header) string {
	bodyDigest := sha256.Sum256(body)
	return strings.Join([]string{
		strings.TrimSpace(sessionID),
		base64.RawURLEncoding.EncodeToString(bodyDigest[:])[:22],
		MirasimRequestFingerprint(h),
	}, "|")
}

// ---------------------------------------------------------------------------
// Write side
// ---------------------------------------------------------------------------

type mirasimWindowLimit struct {
	window  string
	scope   string // "" → account-level scalar
	resetAt time.Time
}

// selectMirasimExhaustedWindows reads the per-window rate-limit headers and
// returns every window this 429 actually rejected on.
//
// The header family is Anthropic's unified-ratelimit set, which is
// window-parametric: anthropic-ratelimit-unified-<window>-status /
// -surpassed-threshold / -utilization / -reset. mirasim speaks the Anthropic
// protocol, so the audited helpers (isAnthropicWindowRejected,
// isAnthropicWindowExceeded, parseAnthropicWindowReset) are reused verbatim with
// the mirasim window tokens substituted; a window with no parsable reset is
// skipped rather than guessed at.
//
// THE GRAMMAR IS EVIDENCE, THE VOCABULARY IS NOT. Reusing the helpers is safe
// because the header SHAPE is verified. Which <window> tokens appear in that
// shape is a separate claim, and for two of the four it is an unverified one —
// see mirasimWindowTokenEvidence. This function can therefore return an empty
// slice for a 429 that named a window perfectly clearly, just under a token we
// do not look for. Callers must pair it with reportMirasimUnknownWindows so
// that case is loud instead of silent.
func selectMirasimExhaustedWindows(headers http.Header, now time.Time) []mirasimWindowLimit {
	if headers == nil {
		return nil
	}
	var limits []mirasimWindowLimit
	for _, window := range mirasimQuotaWindows {
		if !isAnthropicWindowRejected(headers, window) && !isAnthropicWindowExceeded(headers, window) {
			continue
		}
		resetAt, ok := parseAnthropicWindowReset(headers, window, now)
		if !ok {
			continue
		}
		scope, known := mirasimWindowScope(window)
		if !known {
			continue
		}
		limits = append(limits, mirasimWindowLimit{window: window, scope: scope, resetAt: resetAt})
	}
	return limits
}

// ---------------------------------------------------------------------------
// Unrecognized window tokens: making a wrong guess fail loudly
// ---------------------------------------------------------------------------

// mirasimUnifiedRateLimitHeaderPrefix is the fixed part of the header family.
const mirasimUnifiedRateLimitHeaderPrefix = "anthropic-ratelimit-unified-"

// mirasimUnifiedRateLimitFields are the per-window suffixes. Longest first, so
// "-surpassed-threshold" is stripped before the shorter fields get a chance.
//
// A header whose suffix is NOT one of these is ignored entirely rather than
// guessed at: the point of this scanner is a sharp signal, so it only speaks
// about headers it is sure are window-parametric.
var mirasimUnifiedRateLimitFields = []string{
	"surpassed-threshold",
	"utilization",
	"remaining",
	"status",
	"reset",
	"limit",
}

// mirasimWindowTokenFromHeaderName extracts the <window> token out of one
// unified-ratelimit header name.
//
// ok=false for anything that is not a per-window header — including the
// aggregate, window-less "anthropic-ratelimit-unified-reset", which would
// otherwise be read as a window called "reset".
func mirasimWindowTokenFromHeaderName(name string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(name))
	if !strings.HasPrefix(lower, mirasimUnifiedRateLimitHeaderPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(lower, mirasimUnifiedRateLimitHeaderPrefix)
	for _, field := range mirasimUnifiedRateLimitFields {
		if rest == field {
			return "", false
		}
		if token := strings.TrimSuffix(rest, "-"+field); token != rest && token != "" {
			return token, true
		}
	}
	return "", false
}

// mirasimUnknownWindow is one quota window the upstream named and this file does
// not know about.
type mirasimUnknownWindow struct {
	// token is the literal <window> string as it arrived.
	token string
	// exhausted is true when the response says this unknown window is the (or a)
	// reason for the rejection — the case where dropping it costs a real
	// multi-hour cooldown.
	exhausted bool
}

// selectMirasimUnknownWindows returns every unified-ratelimit window token in
// headers that mirasimWindowTokenEvidence does not grade, sorted for a stable
// log line.
//
// This is the detector for "our token names were wrong". The four tokens we act
// on are looked up positively (headers.Get on a name we chose), so a wrong guess
// produces exactly zero evidence of itself. Scanning the other direction — over
// the names the upstream actually sent — is the only way the mistake can show
// up at all.
func selectMirasimUnknownWindows(headers http.Header) []mirasimUnknownWindow {
	if headers == nil {
		return nil
	}
	seen := make(map[string]struct{})
	tokens := make([]string, 0, len(headers))
	for name := range headers {
		token, ok := mirasimWindowTokenFromHeaderName(name)
		if !ok {
			continue
		}
		if _, graded := mirasimWindowTokenEvidence[token]; graded {
			continue
		}
		if _, dup := seen[token]; dup {
			continue
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)

	unknown := make([]mirasimUnknownWindow, 0, len(tokens))
	for _, token := range tokens {
		unknown = append(unknown, mirasimUnknownWindow{
			token:     token,
			exhausted: isAnthropicWindowRejected(headers, token) || isAnthropicWindowExceeded(headers, token),
		})
	}
	return unknown
}

// reportMirasimUnknownWindows emits one structured record per unrecognized
// window token and returns what it reported.
//
// It runs on EVERY mirasim 429, including the ones a known window handled: a
// response can reject on 5h (which we cool correctly) while also reporting the
// family window under a token we never look for, and that second fact is the one
// worth knowing.
//
// Severity is deliberately split. An unknown window that is merely present is a
// vocabulary discovery (Info). An unknown window the response says is EXHAUSTED
// is an active defect (Warn): a real multi-day cooldown was just dropped on the
// floor and the account is about to be rescheduled into the same 429.
func reportMirasimUnknownWindows(accountID int64, statusCode int, headers http.Header) []mirasimUnknownWindow {
	unknown := selectMirasimUnknownWindows(headers)
	for _, window := range unknown {
		attrs := []any{
			"account_id", accountID,
			"status_code", statusCode,
			"window_token", window.token,
			"exhausted", window.exhausted,
			"expected_tokens", mirasimQuotaWindows,
		}
		if hint, ok := mirasimAlternativeWindowTokens[window.token]; ok {
			attrs = append(attrs, "likely_meaning", hint)
		}
		if window.exhausted {
			// The cooldown that should have been written is being lost right now.
			// The unverified expectations ride along, because the actionable
			// question at that moment is "which of OUR tokens should this replace".
			attrs = append(attrs, "unverified_expectations", mirasimUnverifiedWindowExpectations())
			slog.Warn("mirasim_unrecognized_quota_window_exhausted", attrs...)
			continue
		}
		slog.Info("mirasim_unrecognized_quota_window", attrs...)
	}
	return unknown
}

// mirasimUnverifiedWindowExpectations lists the tokens this file looks for that
// nobody has ever observed, each with the evidence that it is wrong.
func mirasimUnverifiedWindowExpectations() []string {
	out := make([]string, 0, len(mirasimQuotaWindows))
	for _, window := range mirasimQuotaWindows {
		if MirasimWindowTokenIsVerified(window) {
			continue
		}
		if counter, ok := mirasimWindowTokenCounterEvidence[window]; ok {
			out = append(out, window+": "+counter)
			continue
		}
		out = append(out, window+": inferred from protocol grammar, never observed")
	}
	return out
}

// persistMirasimWindowLimits writes one cooldown per exhausted window. Returns
// true when at least one window was recognised and handled.
//
// Global windows go to the account-level scalar via SetRateLimited; family
// windows go to their own scope via SetModelRateLimit, so an exhausted
// 7d_claude leaves the account fully schedulable for fable and vice versa.
func (s *RateLimitService) persistMirasimWindowLimits(ctx context.Context, account *Account, headers http.Header) bool {
	if s == nil || s.accountRepo == nil || account == nil {
		return false
	}
	now := time.Now()
	limits := selectMirasimExhaustedWindows(headers, now)
	if len(limits) == 0 {
		return false
	}
	handled := false
	for _, limit := range limits {
		reason := mirasimWindowReason(limit.window)
		if limit.scope == "" {
			handled = s.persistMirasimAccountWindow(ctx, account, limit, reason, now) || handled
			continue
		}
		if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, limit.scope, limit.resetAt, reason); err != nil {
			slog.Warn("mirasim_window_model_rate_limit_set_failed",
				"account_id", account.ID,
				"window", limit.window,
				"scope", limit.scope,
				"reset_at", limit.resetAt,
				"error", err)
			handled = true
			continue
		}
		// Keep the in-memory account consistent with what was just written, so a
		// scheduling decision taken on this same snapshot sees the cooldown.
		setAccountModelRateLimitSnapshot(account, limit.scope, limit.resetAt, reason, now)
		slog.Info("mirasim_window_model_rate_limited",
			"account_id", account.ID,
			"window", limit.window,
			"scope", limit.scope,
			"reset_at", limit.resetAt,
			"reset_in", time.Until(limit.resetAt).Truncate(time.Second))
		handled = true
	}
	return handled
}

func (s *RateLimitService) persistMirasimAccountWindow(ctx context.Context, account *Account, limit mirasimWindowLimit, reason string, now time.Time) bool {
	candidate := &anthropicWindowLimit{window: limit.window, resetAt: limit.resetAt, reason: reason}
	if !shouldPersistAnthropicWindowLimit(account, candidate, now) {
		// An existing, longer cooldown wins. Still "handled": the window was
		// recognised, so the caller must not fall through to the generic path.
		slog.Info("mirasim_window_rate_limit_kept",
			"account_id", account.ID,
			"window", limit.window,
			"reset_at", limit.resetAt,
			"existing_reset_at", account.RateLimitResetAt)
		return true
	}
	s.notifyAccountSchedulingBlocked(account, limit.resetAt, reason)
	if err := s.accountRepo.SetRateLimited(ctx, account.ID, limit.resetAt); err != nil {
		slog.Warn("mirasim_window_rate_limit_set_failed",
			"account_id", account.ID,
			"window", limit.window,
			"reset_at", limit.resetAt,
			"error", err)
		return true
	}
	resetAt := limit.resetAt
	account.RateLimitResetAt = &resetAt
	slog.Info("mirasim_window_rate_limited",
		"account_id", account.ID,
		"window", limit.window,
		"reset_at", limit.resetAt,
		"reset_in", time.Until(limit.resetAt).Truncate(time.Second))
	return true
}

// handleMirasimUpstreamError is the single mirasim-guarded branch inside
// HandleUpstreamError.
//
// handled=false hands the response back to the shared sub2api path completely
// untouched — which is what 403 (banned → disable) and 402 (no balance → stop
// scheduling) deliberately do, and what every non-mirasim account always does.
//
// shouldDisable keeps HandleUpstreamError's meaning: "this account must stop
// being scheduled". A stale credential and a capacity blip are neither.
func (s *RateLimitService) handleMirasimUpstreamError(
	ctx context.Context,
	account *Account,
	statusCode int,
	headers http.Header,
	responseBody []byte,
) (handled bool, shouldDisable bool) {
	if s == nil || account == nil || !IsMirasimAccount(account) {
		return false, false
	}

	switch MirasimClassifyStatus(statusCode, responseBody) {
	case MirasimActionWindowCooldown:
		// Runs before the persist, and regardless of whether the persist
		// succeeds: a 429 can be correctly cooled on 5h while ALSO naming a
		// family window under a token we never look for, and that second fact is
		// the only way an inferred token in mirasimWindowTokenEvidence can ever
		// be falsified.
		unknown := reportMirasimUnknownWindows(account.ID, statusCode, headers)
		if s.persistMirasimWindowLimits(ctx, account, headers) {
			return true, false
		}
		// No window header we recognise. sub2api has no region / shared-quota
		// 429 dimension, so this lands on the configurable seconds-scale
		// fallback rather than a multi-hour window guess.
		//
		// The two reasons are kept apart because they call for opposite actions:
		// a 429 carrying no window headers at all is an upstream shape we do not
		// model, while a 429 carrying window headers under tokens we do not
		// recognise means OUR TOKEN LIST IS WRONG and a real cooldown was just
		// downgraded to seconds.
		reason := "mirasim_no_window_headers"
		if len(unknown) > 0 {
			reason = "mirasim_unrecognized_window_headers"
		}
		s.apply429FallbackRateLimit(ctx, account, reason)
		return true, false

	case MirasimActionRefreshCredential:
		// The upstream rejected the access token / device ticket. That is a
		// credential freshness problem, not an account problem: mirasim accounts
		// carry a refresh token and the signing layer rotates it. Disabling here
		// (which the shared non-OAuth 401 path would do, since a mirasim account
		// is type=apikey) would kill a fully healthy account on a token that was
		// one HTTP call away from being renewed.
		if s.tokenCacheInvalidator != nil {
			if err := s.tokenCacheInvalidator.InvalidateToken(ctx, account); err != nil {
				slog.Warn("mirasim_401_invalidate_token_cache_failed", "account_id", account.ID, "error", err)
			}
		}
		slog.Info("mirasim_401_credential_refresh",
			"account_id", account.ID,
			"has_refresh_token", strings.TrimSpace(account.GetCredential(mirasim.CredRefreshToken)) != "")
		return true, false

	case MirasimActionRequestScoped:
		// Nothing is persisted. Not a cooldown, not a temp-unschedulable, not a
		// model rate limit — because the next request may differ from this one in
		// exactly the way that caused the failure.
		slog.Info("mirasim_request_scoped_upstream_error",
			"account_id", account.ID,
			"status_code", statusCode,
			"request_fingerprint", MirasimRequestFingerprint(headers))
		return true, false
	}

	return false, false
}
