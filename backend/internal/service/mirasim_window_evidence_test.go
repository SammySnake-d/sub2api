//go:build unit

package service

// Evidence tests for the mirasim quota-window TOKENS.
//
// WHAT IS ACTUALLY AT STAKE HERE. The four-window model is implemented by
// looking up header names this package chose: headers.Get("anthropic-ratelimit-
// unified-" + window + "-status"). A positive lookup on a name we invented
// produces no evidence when the name is wrong — it just returns "". Two of the
// four tokens (7d_claude, 7d_fable) have never been observed on any response,
// anywhere, by anyone; mirasimWindowTokenEvidence grades them accordingly.
//
// So these tests do not try to prove the tokens are right — nothing in this
// repository can. They pin the two properties that make being wrong survivable:
//
//	1. when a token DOES match, the cooldown lands on the correct scope;
//	2. when the upstream names a window under some OTHER token, that fact is
//	   reported instead of being silently dropped.
//
// (2) is the one that matters. Without it a wrong token is invisible: no
// cooldown is written, the account is rescheduled seconds later, and the only
// symptom is a 429 rate that never falls.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
)

// ---------------------------------------------------------------------------
// Fakes (self-contained: this file must not depend on another file's fixtures)
// ---------------------------------------------------------------------------

type windowEvidenceCall struct {
	method  string
	scope   string
	resetAt time.Time
	reason  string
}

// windowEvidenceRepo records the account-state writes. The embedded interface is
// nil on purpose: a repository method these paths are not supposed to touch
// panics instead of quietly succeeding.
type windowEvidenceRepo struct {
	AccountRepository

	bound *Account
	calls []windowEvidenceCall
}

func (r *windowEvidenceRepo) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	r.calls = append(r.calls, windowEvidenceCall{method: "SetRateLimited", resetAt: resetAt})
	if r.bound != nil && r.bound.ID == id {
		reset := resetAt
		r.bound.RateLimitResetAt = &reset
	}
	return nil
}

func (r *windowEvidenceRepo) SetModelRateLimit(ctx context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	call := windowEvidenceCall{method: "SetModelRateLimit", scope: scope, resetAt: resetAt}
	if len(reason) > 0 {
		call.reason = reason[0]
	}
	r.calls = append(r.calls, call)
	return nil
}

func (r *windowEvidenceRepo) callsOf(method string) []windowEvidenceCall {
	var out []windowEvidenceCall
	for _, call := range r.calls {
		if call.method == method {
			out = append(out, call)
		}
	}
	return out
}

// windowEvidenceAccount is the exact shape production recognises as mirasim:
// platform=anthropic plus the provider marker. No real credential material.
func windowEvidenceAccount() *Account {
	return &Account{
		ID:          7701,
		Name:        "mirasim-window-evidence",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			mirasim.CredProvider: mirasim.ProviderMirasim,
		},
	}
}

func windowEvidenceService() (*RateLimitService, *windowEvidenceRepo, *Account) {
	account := windowEvidenceAccount()
	repo := &windowEvidenceRepo{bound: account}
	return &RateLimitService{accountRepo: repo}, repo, account
}

// windowEvidenceRejectedHeaders marks one window token as rejected with a
// parsable reset. The token is a plain parameter precisely because the question
// under test is which token strings we react to.
func windowEvidenceRejectedHeaders(token string, resetAt time.Time) http.Header {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-"+token+"-status", "rejected")
	h.Set("anthropic-ratelimit-unified-"+token+"-reset", strconv.FormatInt(resetAt.Unix(), 10))
	h.Set("anthropic-ratelimit-unified-"+token+"-utilization", "1.0")
	return h
}

// ---------------------------------------------------------------------------
// slog capture: "observable" has to mean something a test can read
// ---------------------------------------------------------------------------

type windowEvidenceLogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

type windowEvidenceLogSink struct {
	mu      sync.Mutex
	records []windowEvidenceLogRecord
}

func (s *windowEvidenceLogSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *windowEvidenceLogSink) Handle(_ context.Context, record slog.Record) error {
	captured := windowEvidenceLogRecord{level: record.Level, msg: record.Message, attrs: map[string]any{}}
	record.Attrs(func(attr slog.Attr) bool {
		captured.attrs[attr.Key] = attr.Value.Any()
		return true
	})
	s.mu.Lock()
	s.records = append(s.records, captured)
	s.mu.Unlock()
	return nil
}

func (s *windowEvidenceLogSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *windowEvidenceLogSink) WithGroup(string) slog.Handler      { return s }

func (s *windowEvidenceLogSink) find(msg string) *windowEvidenceLogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].msg == msg {
			return &s.records[i]
		}
	}
	return nil
}

func (s *windowEvidenceLogSink) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.records))
	for _, record := range s.records {
		out = append(out, record.msg)
	}
	return out
}

func captureWindowEvidenceLogs(t *testing.T) *windowEvidenceLogSink {
	t.Helper()
	sink := &windowEvidenceLogSink{}
	previous := slog.Default()
	slog.SetDefault(slog.New(sink))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestMirasimKnownWindowTokenWritesItsScope: for each token in the map, a 429
// carrying that token lands its cooldown where the model says it should — the
// two global windows on the account-level scalar, the two family windows on
// their own per-(account, scope) entry.
//
// This is the "if our guess happens to be right" branch. It says nothing about
// whether 7d_claude / 7d_fable are the real strings.
func TestMirasimKnownWindowTokenWritesItsScope(t *testing.T) {
	// [[cov:QW:known-window-tokens-map]]
	ctx := context.Background()
	cases := []struct {
		window    string
		wantScope string
	}{
		{MirasimWindow5h, ""},
		{MirasimWindow7d, ""},
		{MirasimWindow7dClaude, mirasimClaude7dRateLimitKey},
		{MirasimWindow7dFable, mirasimFable7dRateLimitKey},
	}
	for _, testCase := range cases {
		t.Run(testCase.window, func(t *testing.T) {
			now := time.Now()
			reset := now.Add(3 * time.Hour).Truncate(time.Second)
			headers := windowEvidenceRejectedHeaders(testCase.window, reset)

			// 1. The token is recognised and routed to the scope the window model
			//    assigns it.
			selected := selectMirasimExhaustedWindows(headers, now)
			require.Len(t, selected, 1, "token %q produced no window at all", testCase.window)
			require.Equal(t, testCase.window, selected[0].window)
			require.Equal(t, testCase.wantScope, selected[0].scope)
			require.Equal(t, reset.Unix(), selected[0].resetAt.Unix(),
				"the cooldown must be the upstream's own reset, not a fallback")

			// 2. A token we DO know must not also trip the unknown-window alarm,
			//    or the alarm is worthless.
			require.Empty(t, selectMirasimUnknownWindows(headers),
				"known token %q was reported as unrecognized", testCase.window)

			// 3. The real write path agrees with the selection.
			svc, repo, account := windowEvidenceService()
			require.True(t, svc.persistMirasimWindowLimits(ctx, account, headers))
			if testCase.wantScope == "" {
				accountLevel := repo.callsOf("SetRateLimited")
				require.Len(t, accountLevel, 1)
				require.Equal(t, reset.Unix(), accountLevel[0].resetAt.Unix())
				require.Empty(t, repo.callsOf("SetModelRateLimit"),
					"a global window must not write a family scope")
			} else {
				scoped := repo.callsOf("SetModelRateLimit")
				require.Len(t, scoped, 1)
				require.Equal(t, testCase.wantScope, scoped[0].scope)
				require.Equal(t, mirasimWindowReason(testCase.window), scoped[0].reason)
				require.Equal(t, reset.Unix(), scoped[0].resetAt.Unix())
				require.Empty(t, repo.callsOf("SetRateLimited"),
					"a family window must never touch the account-level scalar")
			}
		})
	}
}

// TestMirasimUnknownWindowTokenLeavesAnObservableSignal is the honesty test for
// the two inferred tokens.
//
// The fixture uses "7d_oi" because that is not a hypothetical: it is the token
// anthropic actually emits for the fable 7-day window, already parsed elsewhere
// in this repository (selectAnthropicFableWindowLimit) and documented on
// UsageInfo.SevenDayFable. If mirasim relays anthropic's headers unchanged —
// which is the same assumption that produced "7d_fable" in the first place —
// then this response, not the one above, is what production sees.
//
// What the test pins is that such a response is LOUD: the missed cooldown is
// reported, the degraded outcome is labelled, and the cost is visible.
func TestMirasimUnknownWindowTokenLeavesAnObservableSignal(t *testing.T) {
	// [[cov:QW:unknown-window-is-observable]]
	ctx := context.Background()
	const observedToken = "7d_oi"
	now := time.Now()
	reset := now.Add(72 * time.Hour).Truncate(time.Second)
	headers := windowEvidenceRejectedHeaders(observedToken, reset)

	// 1. The silent failure being guarded against, stated plainly: the four
	//    tokens this file looks for match nothing in this response.
	require.Empty(t, selectMirasimExhaustedWindows(headers, now),
		"selectMirasimExhaustedWindows found a window; the fixture no longer models an unrecognized token")
	require.False(t, MirasimWindowTokenIsVerified(MirasimWindow7dFable),
		"MirasimWindow7dFable is graded as observed, but no observation exists")

	// 2. The token is nevertheless detected, by scanning the names the upstream
	//    sent rather than the names we hoped for.
	unknown := selectMirasimUnknownWindows(headers)
	require.Len(t, unknown, 1)
	require.Equal(t, observedToken, unknown[0].token)
	require.True(t, unknown[0].exhausted,
		"the response says this window is rejected; losing that is the expensive case")

	// 3. The production 429 path emits it. Nothing about this is a debug aid:
	//    handleMirasimUpstreamError is the only mirasim branch in
	//    HandleUpstreamError.
	logs := captureWindowEvidenceLogs(t)
	svc, repo, account := windowEvidenceService()
	handled, shouldDisable := svc.handleMirasimUpstreamError(ctx, account, http.StatusTooManyRequests, headers, nil)
	require.True(t, handled)
	require.False(t, shouldDisable, "an unrecognized window is not a reason to disable an account")

	record := logs.find("mirasim_unrecognized_quota_window_exhausted")
	require.NotNilf(t, record, "no unrecognized-window record was emitted; records = %v", logs.messages())
	require.Equal(t, slog.LevelWarn, record.level,
		"a dropped multi-day cooldown must not be logged below Warn")
	require.Equal(t, observedToken, record.attrs["window_token"])
	require.Equal(t, true, record.attrs["exhausted"])
	require.Equal(t, account.ID, record.attrs["account_id"])
	require.Equal(t, mirasimQuotaWindows, record.attrs["expected_tokens"],
		"the alert must say which tokens we were looking for")
	require.Contains(t, fmt.Sprint(record.attrs["unverified_expectations"]), MirasimWindow7dFable,
		"the alert must name the unverified token this observation contradicts")
	require.Contains(t, fmt.Sprint(record.attrs["likely_meaning"]), MirasimWindow7dFable)

	// 4. The degraded outcome is labelled as such, so "we cooled the account"
	//    cannot be confused with "we cooled the right window".
	fallback := logs.find("rate_limit_429_fallback_used")
	require.NotNilf(t, fallback, "no 429 fallback was applied; records = %v", logs.messages())
	require.Equal(t, "mirasim_unrecognized_window_headers", fallback.attrs["reason"])

	// 5. And the cost is visible rather than asserted: the account got a
	//    seconds-scale cooldown in place of the three days the header named.
	accountLevel := repo.callsOf("SetRateLimited")
	require.Len(t, accountLevel, 1)
	require.True(t, accountLevel[0].resetAt.Before(now.Add(time.Hour)),
		"expected the seconds-scale fallback, got %v (the header asked for %v)", accountLevel[0].resetAt, reset)
	require.Empty(t, repo.callsOf("SetModelRateLimit"),
		"no family scope may be written from a token we do not understand")
}

// TestMirasimInferredWindowTokensStayMarkedUnverified is the ratchet on the
// grading itself: upgrading a token to "observed" has to be a deliberate edit
// here, not a side effect of someone tidying the const block.
func TestMirasimInferredWindowTokensStayMarkedUnverified(t *testing.T) {
	require.True(t, MirasimWindowTokenIsVerified(MirasimWindow5h))
	require.True(t, MirasimWindowTokenIsVerified(MirasimWindow7d))
	require.Falsef(t, MirasimWindowTokenIsVerified(MirasimWindow7dClaude),
		"%q was graded observed; if a real mirasim 429 carrying it has been captured, cite it here", MirasimWindow7dClaude)
	require.Falsef(t, MirasimWindowTokenIsVerified(MirasimWindow7dFable),
		"%q was graded observed; if a real mirasim 429 carrying it has been captured, cite it here", MirasimWindow7dFable)

	// Every window the scheduler acts on carries a grade: an ungraded token is a
	// claim with no provenance at all.
	for _, window := range mirasimQuotaWindows {
		_, graded := mirasimWindowTokenEvidence[window]
		require.Truef(t, graded, "window %q is acted on but has no evidence grade", window)
	}

	expectations := mirasimUnverifiedWindowExpectations()
	require.Len(t, expectations, 2)
	require.Contains(t, fmt.Sprint(expectations), "7d_oi",
		"the fable expectation must carry the contradicting observation")
}

// TestMirasimWindowTokenExtractionIgnoresTheAggregateHeader keeps the detector
// from inventing windows out of headers that have none. The aggregate
// anthropic-ratelimit-unified-reset is window-less, and reading it as a window
// called "reset" would fire the alarm on every ordinary 429.
func TestMirasimWindowTokenExtractionIgnoresTheAggregateHeader(t *testing.T) {
	cases := []struct {
		header    string
		wantToken string
		wantOK    bool
	}{
		{"anthropic-ratelimit-unified-reset", "", false},
		{"anthropic-ratelimit-unified-status", "", false},
		{"Anthropic-Ratelimit-Unified-5h-Status", "5h", true},
		{"anthropic-ratelimit-unified-7d_oi-surpassed-threshold", "7d_oi", true},
		{"anthropic-ratelimit-unified-7d_claude-utilization", "7d_claude", true},
		{"anthropic-ratelimit-requests-limit", "", false},
		{"content-type", "", false},
	}
	for _, testCase := range cases {
		token, ok := mirasimWindowTokenFromHeaderName(testCase.header)
		require.Equalf(t, testCase.wantOK, ok, "header %q", testCase.header)
		require.Equalf(t, testCase.wantToken, token, "header %q", testCase.header)
	}

	aggregate := http.Header{}
	aggregate.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	aggregate.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	require.Empty(t, selectMirasimUnknownWindows(aggregate))
}
