package service

// Tests for the mirasim subscription-plan probe.
//
// EVIDENCE GRADE OF THE FIXTURE: the /auth/referral response body used here is
// modelled on ma-relay's ReferralInfo (internal/relay/referral.go) — field names
// and semantics are ported from that implementation, which operates these same
// accounts. It has NOT been re-observed live from this repository. What these
// tests therefore prove is that sub2api handles that documented shape correctly,
// not that the shape is what the server emits today.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// mirasimPlanTestSeed is an obviously-fake but structurally valid 32-byte
// ed25519 device seed. No real credential appears in this file.
const mirasimPlanTestSeed = "dGVzdC1taXJhc2ltLXBsYW4tc2VlZC0wMDAwMDAwMDA"

type mirasimPlanProbeCall struct {
	method           string
	url              string
	proxyURL         string
	accountID        int64
	authorization    string
	tlsProfile       string
	signingDisabled  bool
	redirectDisabled bool
	mirasimHeaders   []string
}

// mirasimPlanProbeUpstream is a fake HTTPUpstream that records how each request
// was dispatched, which is what makes the egress assertions possible.
type mirasimPlanProbeUpstream struct {
	mu     sync.Mutex
	calls  []mirasimPlanProbeCall
	status int
	body   string
}

func (u *mirasimPlanProbeUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (u *mirasimPlanProbeUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	call := mirasimPlanProbeCall{
		method:           req.Method,
		url:              req.URL.String(),
		proxyURL:         proxyURL,
		accountID:        accountID,
		authorization:    req.Header.Get("authorization"),
		signingDisabled:  MirasimSigningDisabled(req.Context()),
		redirectDisabled: HTTPUpstreamRedirectsDisabled(req.Context()),
	}
	if profile != nil {
		call.tlsProfile = profile.Name
	}
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-mirasim-") {
			call.mirasimHeaders = append(call.mirasimHeaders, strings.ToLower(name))
		}
	}
	u.mu.Lock()
	u.calls = append(u.calls, call)
	u.mu.Unlock()

	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(u.body)),
	}, nil
}

func (u *mirasimPlanProbeUpstream) lastCall(t *testing.T) mirasimPlanProbeCall {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.calls) == 0 {
		t.Fatalf("the probe issued no upstream request at all")
	}
	return u.calls[len(u.calls)-1]
}

// mirasimPlanProbeRepo is the narrow slice of AccountRepository the probe uses.
// The interface is embedded so the unused methods stay nil rather than being
// stubbed out one by one; any accidental call panics loudly instead of silently
// returning a zero value.
type mirasimPlanProbeRepo struct {
	AccountRepository
	mu             sync.Mutex
	account        *Account
	extraWrites    []map[string]any
	updateExtraErr error
}

func (r *mirasimPlanProbeRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account == nil || r.account.ID != id {
		return nil, ErrAccountNotFound
	}
	return r.account, nil
}

func (r *mirasimPlanProbeRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateExtraErr != nil {
		return r.updateExtraErr
	}
	r.extraWrites = append(r.extraWrites, updates)
	if r.account != nil && r.account.ID == id {
		if r.account.Extra == nil {
			r.account.Extra = map[string]any{}
		}
		for key, value := range updates {
			r.account.Extra[key] = value
		}
	}
	return nil
}

// newMirasimPlanProbeAccount builds a mirasim account bound to its own proxy.
// claimedPlan == "" models an account imported before plan fields existed.
func newMirasimPlanProbeAccount(claimedPlan string) *Account {
	extra := map[string]any{"anthropic_passthrough": true}
	if claimedPlan != "" {
		extra[mirasim.ExtraPlanClaimed] = claimedPlan
	}
	return &Account{
		ID:          42,
		Name:        "mirasim-alpha",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 3,
		Credentials: map[string]any{
			mirasim.CredProvider:    mirasim.ProviderMirasim,
			mirasim.CredDeviceSeed:  mirasimPlanTestSeed,
			mirasim.CredAccessToken: "test-access-token-alpha",
			mirasim.CredAuthBase:    "https://auth.mirasim.test",
			"base_url":              "https://relay.mirasim.test",
			"api_key":               "mirasim-signed",
		},
		Extra:   extra,
		ProxyID: mirasimPlanTestProxyID(9),
		Proxy: &Proxy{
			ID:       9,
			Name:     "mirasim-egress-9",
			Protocol: "socks5h",
			Host:     "proxy.example.test",
			Port:     1080,
			Username: "mirasim9",
			Password: "test-proxy-secret",
			Status:   StatusActive,
		},
	}
}

func mirasimPlanTestProxyID(v int64) *int64 { return &v }

// mirasimReferralBody renders a /auth/referral response in ma-relay's shape.
func mirasimReferralBody(t *testing.T, info mirasim.ReferralInfo) string {
	t.Helper()
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal referral fixture: %v", err)
	}
	return string(raw)
}

// storedMirasimPlanSnapshot reads back what the probe persisted, through the
// same decoder production uses — a snapshot that cannot be decoded is worth
// nothing, so the test must not bypass that step.
func storedMirasimPlanSnapshot(t *testing.T, account *Account) *MirasimPlanSnapshot {
	t.Helper()
	// Round-trip through JSON first: accounts.extra is JSONB, so what comes back
	// from the database is never the Go struct that went in.
	raw, err := json.Marshal(account.Extra)
	if err != nil {
		t.Fatalf("marshal stored extra: %v", err)
	}
	var reloaded map[string]any
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal stored extra: %v", err)
	}
	snapshot := DecodeMirasimPlanSnapshot(reloaded)
	if snapshot == nil {
		t.Fatalf("DecodeMirasimPlanSnapshot found no snapshot under extra[%s]; keys = %v",
			mirasim.ExtraPlanProbe, reloaded)
	}
	return snapshot
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestMirasimPlanProbeUpdatesPlanFromAuthoritativeReferral: an account whose
// stored state says nothing about its tier gets the tier the server actually
// grants, plus the subscription expiry, and the resolved view reports the value
// as authoritative rather than claimed.
func TestMirasimPlanProbeUpdatesPlanFromAuthoritativeReferral(t *testing.T) {
	// [[cov:PL:probe-updates-plan]]
	account := newMirasimPlanProbeAccount("")
	repo := &mirasimPlanProbeRepo{account: account}
	upstream := &mirasimPlanProbeUpstream{
		body: mirasimReferralBody(t, mirasim.ReferralInfo{
			Code:          "TESTCODE",
			Redeemed:      3,
			Threshold:     10,
			Remaining:     7,
			CurrentPlan:   "max",
			NextPlan:      "max",
			PlanExpiresAt: "2027-09-10T19:20:40.460323Z",
		}),
	}
	svc := NewMirasimPlanProbeService(repo, upstream, nil)

	snapshot, err := svc.ProbeAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("ProbeAccount error = %v", err)
	}
	if snapshot == nil {
		t.Fatalf("ProbeAccount returned no snapshot")
	}
	if snapshot.Status != MirasimPlanProbeStatusOK {
		t.Fatalf("snapshot.Status = %q, want %q (last_error=%q http=%d)",
			snapshot.Status, MirasimPlanProbeStatusOK, snapshot.LastError, snapshot.HTTPStatus)
	}

	// The request actually went to /auth/referral on the account's auth base —
	// not to the relay, which answers a different question.
	call := upstream.lastCall(t)
	if call.method != http.MethodGet || !strings.HasSuffix(call.url, "/auth/referral") {
		t.Fatalf("probe issued %s %s, want GET .../auth/referral", call.method, call.url)
	}
	if !strings.HasPrefix(call.url, account.GetCredential(mirasim.CredAuthBase)) {
		t.Fatalf("probe hit %s, want the account's auth base %s", call.url, account.GetCredential(mirasim.CredAuthBase))
	}

	stored := storedMirasimPlanSnapshot(t, account)
	if stored.Plan != "max" {
		t.Fatalf("stored snapshot.Plan = %q, want the authoritative current_plan %q", stored.Plan, "max")
	}
	if stored.PlanExpiresAt != "2027-09-10T19:20:40.460323Z" {
		t.Fatalf("stored snapshot.PlanExpiresAt = %q, want the referral's plan_expires_at", stored.PlanExpiresAt)
	}
	if stored.Redeemed != 3 || stored.Threshold != 10 {
		t.Fatalf("stored snapshot invitations = %d/%d, want 3/10", stored.Redeemed, stored.Threshold)
	}
	if stored.NextProbeAt.IsZero() || !stored.NextProbeAt.After(stored.LastAttemptAt) {
		t.Fatalf("stored snapshot next_probe_at = %v must be after last_attempt_at = %v", stored.NextProbeAt, stored.LastAttemptAt)
	}

	item := ResolveMirasimPlan(account)
	if item.Plan != "max" || item.PlanSource != MirasimPlanSourceAuthoritative {
		t.Fatalf("ResolveMirasimPlan = plan %q source %q, want max/%s", item.Plan, item.PlanSource, MirasimPlanSourceAuthoritative)
	}
	if item.PlanExpiresAt != "2027-09-10T19:20:40.460323Z" || item.PlanExpiresSource != MirasimPlanSourceAuthoritative {
		t.Fatalf("ResolveMirasimPlan expiry = %q source %q, want the probed value marked authoritative",
			item.PlanExpiresAt, item.PlanExpiresSource)
	}
}

// TestMirasimPlanProbeKeepsClaimedAndAuthoritativePlansApart is the "标 max 实为
// plus" case ma-relay documents: invitations met the threshold, the upgrade has
// not settled, and the import label still says max. Both values must survive and
// the disagreement must be marked — a silent overwrite in either direction
// destroys the only evidence the operator has that the account needs chasing.
func TestMirasimPlanProbeKeepsClaimedAndAuthoritativePlansApart(t *testing.T) {
	// [[cov:PL:claimed-vs-authoritative]]
	account := newMirasimPlanProbeAccount("max")
	repo := &mirasimPlanProbeRepo{account: account}
	referral := mirasim.ReferralInfo{
		Code:          "TESTCODE",
		Redeemed:      14,
		Threshold:     10,
		Reached:       true,
		CurrentPlan:   "plus",
		NextPlan:      "max",
		PlanExpiresAt: "2027-09-10T19:20:40.460323Z",
	}
	upstream := &mirasimPlanProbeUpstream{body: mirasimReferralBody(t, referral)}
	svc := NewMirasimPlanProbeService(repo, upstream, nil)

	if _, err := svc.ProbeAccount(context.Background(), account.ID); err != nil {
		t.Fatalf("ProbeAccount error = %v", err)
	}

	// 1. The claim is untouched. The probe owns exactly one extra key.
	if got := account.Extra[mirasim.ExtraPlanClaimed]; got != "max" {
		t.Fatalf("probe overwrote extra[%s] = %v, want the untouched claim %q", mirasim.ExtraPlanClaimed, got, "max")
	}
	for _, write := range repo.extraWrites {
		for key := range write {
			if key != mirasim.ExtraPlanProbe {
				t.Fatalf("probe wrote extra key %q; it may only write %q", key, mirasim.ExtraPlanProbe)
			}
		}
	}

	// 2. Both values are readable side by side, and the mismatch is marked.
	stored := storedMirasimPlanSnapshot(t, account)
	if stored.ClaimedPlan != "max" || stored.Plan != "plus" {
		t.Fatalf("stored snapshot = claimed %q / authoritative %q, want max/plus", stored.ClaimedPlan, stored.Plan)
	}
	if !stored.Mismatch {
		t.Fatalf("stored snapshot.Mismatch = false, want true for claimed max vs authoritative plus")
	}
	if !stored.PendingUpgrade {
		t.Fatalf("stored snapshot.PendingUpgrade = false, want true (redeemed %d >= threshold %d, next_plan %q)",
			referral.Redeemed, referral.Threshold, referral.NextPlan)
	}
	wantNote := mirasim.PlanDiffNote("max", &referral)
	if wantNote == "" {
		t.Fatalf("mirasim.PlanDiffNote produced no note for a claimed/authoritative disagreement")
	}
	if stored.Note != wantNote {
		t.Fatalf("stored snapshot.Note = %q, want mirasim.PlanDiffNote output %q", stored.Note, wantNote)
	}

	// 3. The resolved view prefers the verified tier but still surfaces the claim.
	item := ResolveMirasimPlan(account)
	if item.Plan != "plus" || item.PlanSource != MirasimPlanSourceAuthoritative {
		t.Fatalf("ResolveMirasimPlan = plan %q source %q, want plus/%s", item.Plan, item.PlanSource, MirasimPlanSourceAuthoritative)
	}
	if item.ClaimedPlan != "max" || !item.PlanMismatch || item.PlanNote != wantNote {
		t.Fatalf("ResolveMirasimPlan lost the claim: claimed %q mismatch %v note %q",
			item.ClaimedPlan, item.PlanMismatch, item.PlanNote)
	}

	// 4. Filtering on the verified tier must not match the stale claim.
	if got := BuildMirasimPlanItems([]Account{*account}, "max", false); len(got) != 0 {
		t.Fatalf("filtering on plan=max returned %d accounts, want 0: the account's verified tier is plus", len(got))
	}
	if got := BuildMirasimPlanItems([]Account{*account}, "plus", false); len(got) != 1 {
		t.Fatalf("filtering on plan=plus returned %d accounts, want 1", len(got))
	}
}

// TestMirasimPlanProbeLeavesThroughTheAccountProxy: a mirasim account is pinned
// to one egress IP and the upstream binds the device to it, so a probe that went
// out on any other path would show the account operating from two IPs at once.
func TestMirasimPlanProbeLeavesThroughTheAccountProxy(t *testing.T) {
	// [[cov:PL:probe-uses-account-proxy]]
	account := newMirasimPlanProbeAccount("plus")
	repo := &mirasimPlanProbeRepo{account: account}
	upstream := &mirasimPlanProbeUpstream{
		body: mirasimReferralBody(t, mirasim.ReferralInfo{CurrentPlan: "plus", NextPlan: "max", Threshold: 10}),
	}
	svc := NewMirasimPlanProbeService(repo, upstream, nil)

	if _, err := svc.ProbeAccount(context.Background(), account.ID); err != nil {
		t.Fatalf("ProbeAccount error = %v", err)
	}
	call := upstream.lastCall(t)

	wantProxy := account.Proxy.URL()
	if wantProxy == "" {
		t.Fatalf("test fixture built an account whose Proxy.URL() is empty; the assertion below would be vacuous")
	}
	if call.proxyURL != wantProxy {
		t.Fatalf("probe egress proxy = %q, want the account's own Proxy.URL() %q", call.proxyURL, wantProxy)
	}
	if call.accountID != account.ID {
		t.Fatalf("probe dispatched under account id %d, want %d (the connection pool is keyed on it)", call.accountID, account.ID)
	}

	// Same handshake as the data plane: the upstream classifies the client from
	// the TLS hello before it reads a header.
	wantProfile := tlsfingerprint.MirasimProfile()
	if wantProfile == nil {
		t.Fatalf("tlsfingerprint.MirasimProfile() returned nil; the assertion below would be vacuous")
	}
	if call.tlsProfile != wantProfile.Name {
		t.Fatalf("probe TLS profile = %q, want tlsfingerprint.MirasimProfile() %q", call.tlsProfile, wantProfile.Name)
	}

	// The auth server authenticates the plain access-token bearer, so the probe
	// must reach the signing decorator with signing explicitly disabled and must
	// carry the account's own token — not a relay-issued device ticket.
	if !call.signingDisabled {
		t.Fatalf("probe request context did not carry the mirasim signing opt-out; the decorator would replace the bearer with a device ticket")
	}
	if len(call.mirasimHeaders) != 0 {
		t.Fatalf("probe carried device-signature headers %v; /auth/referral is authenticated by the bearer alone", call.mirasimHeaders)
	}
	if want := "Bearer " + account.GetCredential(mirasim.CredAccessToken); call.authorization != want {
		t.Fatalf("probe authorization = %q, want the account's access token bearer %q", call.authorization, want)
	}
	if !call.redirectDisabled {
		t.Fatalf("probe followed redirects; a credential-bearing probe must not")
	}
}

// TestMirasimPlanProbeFailureKeepsTheLastGoodReading: an upstream rejection must
// not blank out a plan an operator is relying on, and must not be promoted over
// the claim either.
func TestMirasimPlanProbeFailureKeepsTheLastGoodReading(t *testing.T) {
	account := newMirasimPlanProbeAccount("max")
	repo := &mirasimPlanProbeRepo{account: account}
	upstream := &mirasimPlanProbeUpstream{
		body: mirasimReferralBody(t, mirasim.ReferralInfo{CurrentPlan: "plus", NextPlan: "max", Threshold: 10, Redeemed: 14, Reached: true}),
	}
	svc := NewMirasimPlanProbeService(repo, upstream, nil)
	if _, err := svc.ProbeAccount(context.Background(), account.ID); err != nil {
		t.Fatalf("first ProbeAccount error = %v", err)
	}

	upstream.status = http.StatusUnauthorized
	upstream.body = `{"error":"unauthorized"}`
	snapshot, err := svc.ProbeAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("second ProbeAccount error = %v", err)
	}
	if snapshot.Status != MirasimPlanProbeStatusFailed {
		t.Fatalf("snapshot.Status = %q, want %q", snapshot.Status, MirasimPlanProbeStatusFailed)
	}
	if snapshot.HTTPStatus != http.StatusUnauthorized || snapshot.LastError != "credential_rejected" {
		t.Fatalf("snapshot = http %d / %q, want 401 / credential_rejected", snapshot.HTTPStatus, snapshot.LastError)
	}
	if snapshot.Plan != "plus" {
		t.Fatalf("failed probe dropped the last good reading: Plan = %q, want plus", snapshot.Plan)
	}
	// A failed probe is not evidence, so the resolved view falls back to the claim.
	item := ResolveMirasimPlan(account)
	if item.Plan != "max" || item.PlanSource != MirasimPlanSourceClaimed {
		t.Fatalf("ResolveMirasimPlan after a failed probe = plan %q source %q, want the claim max/%s",
			item.Plan, item.PlanSource, MirasimPlanSourceClaimed)
	}
}

// TestMirasimPlanProbeRejectsNonMirasimAccounts keeps the probe from pointing an
// unrelated account's credentials at mirasim's auth server.
func TestMirasimPlanProbeRejectsNonMirasimAccounts(t *testing.T) {
	account := newMirasimPlanProbeAccount("max")
	delete(account.Credentials, mirasim.CredProvider)
	repo := &mirasimPlanProbeRepo{account: account}
	upstream := &mirasimPlanProbeUpstream{body: "{}"}
	svc := NewMirasimPlanProbeService(repo, upstream, nil)

	if _, err := svc.ProbeAccount(context.Background(), account.ID); err == nil {
		t.Fatalf("ProbeAccount accepted a non-mirasim account")
	}
	upstream.mu.Lock()
	calls := len(upstream.calls)
	upstream.mu.Unlock()
	if calls != 0 {
		t.Fatalf("probe issued %d upstream requests for a non-mirasim account, want 0", calls)
	}
}
