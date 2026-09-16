package mirasim

// Tests for the /v1/limits quota probe.
//
// EVIDENCE GRADE OF THE FIXTURE: the response body used here is modelled on
// ma-relay internal/relay/quota.go probeQuotaOne, which reads this endpoint in
// production for these same accounts. The four window NAMES in the fixture are a
// different matter: 5h and 7d are observed, 7d_claude and 7d_fable are the
// unverified tokens this repository expects (see service.mirasimWindowTokenEvidence).
// These tests therefore prove sub2api handles the documented SHAPE correctly —
// including the case where an expected window is simply absent — not that those
// names are what the relay emits.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// limitsFixture renders a /v1/limits body in ma-relay's shape.
func limitsFixture(t *testing.T, suspended bool, windows ...LimitsWindow) string {
	t.Helper()
	raw, err := json.Marshal(LimitsInfo{Suspended: suspended, Windows: windows})
	if err != nil {
		t.Fatalf("marshal limits fixture: %v", err)
	}
	return string(raw)
}

// limitsProbeServer answers /v1/limits with the supplied status and body, and
// records the request it received. /v1/device/session is answered with a ticket
// so Prepare takes its normal path.
func limitsProbeServer(t *testing.T, status int, body string) (*httptest.Server, func() *http.Request) {
	t.Helper()
	var captured *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/device/session":
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "tkt_limits", "expiresIn": 600})
		case LimitsPath:
			captured = r.Clone(context.Background())
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() *http.Request {
		if captured == nil {
			t.Fatalf("the probe never issued a %s request", LimitsPath)
		}
		return captured
	}
}

func limitsProbeIdentity(t *testing.T, base string) Identity {
	t.Helper()
	return Identity{
		DeviceSeed:  testSeed,
		AccessToken: testJWT(t, "usr_limits", time.Now().Add(45*time.Minute)),
		ExpiresAt:   time.Now().Add(45 * time.Minute),
		AuthBase:    base,
		RelayBase:   base,
		SessionID:   "session_fixture",
	}
}

// TestApplyContextHeadersMarksOnlyTheLimitsProbe pins the branch FetchLimits
// depends on, with its differential negative.
//
// The marker matters because an UNSIGNED, unmarked /v1/limits does not fail — it
// succeeds with the relay's shared placeholder budget. A silently-dropped marker
// would therefore produce confident wrong numbers, so the branch needs a test
// that also proves it does not fire on other paths (which would put a probe
// marker on real traffic).
func TestApplyContextHeadersMarksOnlyTheLimitsProbe(t *testing.T) {
	probe := http.Header{}
	ApplyContextHeaders(probe, ContextInput{Path: LimitsPath, SessionID: "s", Locale: "en-US"})
	if got := probe.Get("x-mirasim-probe"); got != "usage" {
		t.Fatalf("x-mirasim-probe = %q, want %q on %s", got, "usage", LimitsPath)
	}
	if got := probe.Get("accept-encoding"); got != "identity" {
		t.Fatalf("accept-encoding = %q, want identity on %s", got, LimitsPath)
	}

	messages := http.Header{}
	ApplyContextHeaders(messages, ContextInput{Path: "/v1/messages", SessionID: "s", Locale: "en-US"})
	if got := messages.Get("x-mirasim-probe"); got != "" {
		t.Fatalf("/v1/messages carried x-mirasim-probe = %q; the usage marker must not ride on real traffic", got)
	}
	if got := messages.Get("accept-encoding"); got != "" {
		t.Fatalf("/v1/messages carried accept-encoding = %q, want it untouched", got)
	}
}

// TestFetchLimitsSignsSealsAndParsesFourWindows is the positive case: the
// request goes out signed, sealed and marked as a usage probe, and all four
// windows come back with their absolute counters intact.
func TestFetchLimitsSignsSealsAndParsesFourWindows(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).Unix()
	srv, captured := limitsProbeServer(t, http.StatusOK, limitsFixture(t, false,
		LimitsWindow{Name: "5h", Used: 120, Budget: 400, ResetAt: reset},
		LimitsWindow{Name: "7d", Used: 5000, Budget: 20000, ResetAt: reset},
		LimitsWindow{Name: "7d_claude", Used: 3000, Budget: 12000, ResetAt: reset},
		LimitsWindow{Name: "7d_fable", Used: 10, Budget: 900, ResetAt: reset},
	))

	reg := NewRegistry()
	client := doerFunc(func(r *http.Request) (*http.Response, error) { return srv.Client().Do(r) })
	info, err := reg.FetchLimits(context.Background(), 77, limitsProbeIdentity(t, srv.URL), client, nil, nil)
	if err != nil {
		t.Fatalf("FetchLimits error = %v", err)
	}
	if info == nil || len(info.Windows) != 4 {
		t.Fatalf("FetchLimits returned %+v, want four windows", info)
	}
	if info.Windows[2].Name != "7d_claude" || info.Windows[2].Used != 3000 || info.Windows[2].Budget != 12000 {
		t.Fatalf("third window = %+v, want the claude family window with its absolute counters", info.Windows[2])
	}

	req := captured()
	if req.Method != http.MethodGet {
		t.Fatalf("probe used %s, want GET (a GET with no body is what makes this cost zero tokens)", req.Method)
	}
	// accept-encoding is the one part of ApplyContextHeaders' /v1/limits branch
	// that is NOT swept into the seal, so it is the observable proof the branch
	// ran — and therefore that x-mirasim-probe was set before sealing.
	if got := req.Header.Get("accept-encoding"); got != "identity" {
		t.Fatalf("accept-encoding = %q, want identity: the /v1/limits branch of ApplyContextHeaders did not run, "+
			"so x-mirasim-probe was never set and the relay would answer with the shared placeholder budget", got)
	}
	if req.Header.Get("x-mirasim-enc") == "" {
		t.Fatal("the request went out unsealed; an unsigned /v1/limits is answered with a placeholder budget, not an error")
	}
	if req.Header.Get("x-mirasim-client") == "" {
		t.Fatal("x-mirasim-client must stay plaintext so the relay can read the client version without unsealing")
	}
	// Everything else in the namespace must be INSIDE the seal.
	for _, name := range []string{"x-mirasim-probe", "x-mirasim-session", "x-mirasim-agent", "x-mirasim-sig", "x-mirasim-device"} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("%s travelled in the clear (%q); SealHeaders must fold it into x-mirasim-enc", name, got)
		}
	}
	// The bearer must be the ticket Prepare minted — the same string the
	// signature covers.
	if got := req.Header.Get("authorization"); got != "Bearer tkt_limits" {
		t.Fatalf("authorization = %q, want the minted device ticket", got)
	}
}

// TestFetchLimitsRejectsNon2xxWithoutDecoding: a refusal must surface as a
// status, not as an empty reading. The body is deliberately valid limits JSON so
// the test fails if the code decodes first and checks the status afterwards.
func TestFetchLimitsRejectsNon2xxWithoutDecoding(t *testing.T) {
	srv, _ := limitsProbeServer(t, http.StatusServiceUnavailable, limitsFixture(t, false,
		LimitsWindow{Name: "7d", Used: 1, Budget: 2, ResetAt: time.Now().Add(time.Hour).Unix()},
	))

	reg := NewRegistry()
	client := doerFunc(func(r *http.Request) (*http.Response, error) { return srv.Client().Do(r) })
	info, err := reg.FetchLimits(context.Background(), 78, limitsProbeIdentity(t, srv.URL), client, nil, nil)
	if err == nil {
		t.Fatalf("FetchLimits returned %+v for a 503; a rejection must never look like a reading", info)
	}
	if info != nil {
		t.Fatalf("FetchLimits returned both an error and %+v", info)
	}
	if got := LimitsStatus(err); got != http.StatusServiceUnavailable {
		t.Fatalf("LimitsStatus = %d, want 503", got)
	}
	if IsLimitsDecodeFailure(err) {
		t.Fatal("a 503 was classified as a decode failure; the two call for different operator actions")
	}
}

// TestFetchLimitsUnreadableBodyIsADecodeFailure: a 2xx carrying an HTML
// interstitial (a proxy login page, an edge error) keeps its status but is
// reported as a decode failure, so the caller records "200 with an unreadable
// body" rather than "the account has no windows".
func TestFetchLimitsUnreadableBodyIsADecodeFailure(t *testing.T) {
	srv, _ := limitsProbeServer(t, http.StatusOK, "<html><body>proxy auth required</body></html>")

	reg := NewRegistry()
	client := doerFunc(func(r *http.Request) (*http.Response, error) { return srv.Client().Do(r) })
	info, err := reg.FetchLimits(context.Background(), 79, limitsProbeIdentity(t, srv.URL), client, nil, nil)
	if err == nil {
		t.Fatalf("FetchLimits accepted an HTML body and returned %+v", info)
	}
	if !IsLimitsDecodeFailure(err) {
		t.Fatalf("error %v is not a decode failure; the caller cannot distinguish it from a transport error", err)
	}
	if got := LimitsStatus(err); got != http.StatusOK {
		t.Fatalf("LimitsStatus = %d, want 200: the status is what says the relay answered at all", got)
	}
}

// TestFetchLimitsUnknownFieldNamesYieldNoWindows documents the silent trap this
// package deliberately does NOT defend against, so the guarantee the caller must
// provide is written down and enforced somewhere.
//
// encoding/json ignores unknown field names without error, so a renamed payload
// decodes into an all-zero LimitsInfo: no error, no windows. Returning an error
// here would collapse it into the transport bucket and lose the HTTP status,
// which is exactly what tells an operator the relay answered. The rejection
// therefore belongs to the caller — service.MirasimQuotaProbeService records it
// as "empty_windows" with http_status 200.
func TestFetchLimitsUnknownFieldNamesYieldNoWindows(t *testing.T) {
	srv, _ := limitsProbeServer(t, http.StatusOK, `{"quotas":[{"label":"7d","consumed":5,"cap":10}]}`)

	reg := NewRegistry()
	client := doerFunc(func(r *http.Request) (*http.Response, error) { return srv.Client().Do(r) })
	info, err := reg.FetchLimits(context.Background(), 80, limitsProbeIdentity(t, srv.URL), client, nil, nil)
	if err != nil {
		t.Fatalf("FetchLimits error = %v, want a successful decode into an empty struct", err)
	}
	if info == nil || len(info.Windows) != 0 {
		t.Fatalf("FetchLimits returned %+v, want zero windows from an unrecognised payload", info)
	}
}

// TestFetchLimitsUsesTheAccountsOwnRelayBase: the probe must read the relay the
// account is bound to. A probe sent to the default base would be a perfectly
// successful reading of somebody else's numbers.
func TestFetchLimitsUsesTheAccountsOwnRelayBase(t *testing.T) {
	srv, captured := limitsProbeServer(t, http.StatusOK, limitsFixture(t, false,
		LimitsWindow{Name: "7d", Used: 1, Budget: 2, ResetAt: time.Now().Add(time.Hour).Unix()},
	))

	reg := NewRegistry()
	client := doerFunc(func(r *http.Request) (*http.Response, error) { return srv.Client().Do(r) })
	if _, err := reg.FetchLimits(context.Background(), 81, limitsProbeIdentity(t, srv.URL), client, nil, nil); err != nil {
		t.Fatalf("FetchLimits error = %v", err)
	}
	if host := captured().Host; !strings.Contains(srv.URL, host) {
		t.Fatalf("probe hit host %q, want the account's own relay %q", host, srv.URL)
	}
	if got := reg.relayBaseFor(81); got != strings.TrimRight(srv.URL, "/") {
		t.Fatalf("relayBaseFor = %q, want the account's bound relay %q", got, srv.URL)
	}
	// An account that never bound a base falls back to the shipped default, in
	// one place, rather than to the empty string.
	if got := reg.relayBaseFor(999); got != DefaultRelayBase {
		t.Fatalf("relayBaseFor(unbound) = %q, want %q", got, DefaultRelayBase)
	}
}
