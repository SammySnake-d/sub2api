package service

// Evidence test for the /auth/referral RESPONSE SHAPE.
//
// mirasim.ReferralInfo's field set was ported from ma-relay
// (internal/relay/referral.go); no request has ever been sent to the live auth
// server from this repository and no captured body is checked in anywhere. The
// shape is therefore second-hand, and the failure it can produce is the quiet
// kind: encoding/json decodes a JSON object whose field names we do not know
// into an ALL-ZERO struct, with no error. A caller that trusted that struct
// would write plan="" over a real reading and call it authoritative.
//
// This test does not try to prove the field names are right — it cannot. It
// pins that being wrong is inert: a response our struct cannot read produces a
// recorded failure, and every previously known value survives untouched.

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

// planEvidenceTestSeed is an obviously-fake but structurally valid device seed.
// No real credential appears in this file.
const planEvidenceTestSeed = "dGVzdC1taXJhc2ltLXBsYW4tZXZpZGVuY2Utc2VlZDA"

// planEvidenceUpstream answers every probe with one scripted body.
type planEvidenceUpstream struct {
	mu     sync.Mutex
	status int
	body   string
}

func (u *planEvidenceUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (u *planEvidenceUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	status := u.status
	body := u.body
	u.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (u *planEvidenceUpstream) serve(status int, body string) {
	u.mu.Lock()
	u.status = status
	u.body = body
	u.mu.Unlock()
}

// planEvidenceRepo records every extra write. The embedded interface is nil so
// any repository method the probe is not supposed to call panics loudly.
type planEvidenceRepo struct {
	AccountRepository

	mu          sync.Mutex
	account     *Account
	extraWrites []map[string]any
}

func (r *planEvidenceRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account == nil || r.account.ID != id {
		return nil, ErrAccountNotFound
	}
	return r.account, nil
}

func (r *planEvidenceRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func (r *planEvidenceRepo) writtenKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var keys []string
	for _, write := range r.extraWrites {
		for key := range write {
			keys = append(keys, key)
		}
	}
	return keys
}

func planEvidenceAccount(claimedPlan string) *Account {
	return &Account{
		ID:          8801,
		Name:        "mirasim-plan-evidence",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			mirasim.CredProvider:    mirasim.ProviderMirasim,
			mirasim.CredDeviceSeed:  planEvidenceTestSeed,
			mirasim.CredAccessToken: "test-access-token-plan-evidence",
			mirasim.CredAuthBase:    "https://auth.mirasim.test",
			"base_url":              "https://relay.mirasim.test",
		},
		Extra: map[string]any{mirasim.ExtraPlanClaimed: claimedPlan},
	}
}

// planEvidenceStoredSnapshot reads back what was persisted through the same
// decoder production uses, after a JSON round-trip — accounts.extra is JSONB, so
// what comes back from the database is never the Go struct that went in.
func planEvidenceStoredSnapshot(t *testing.T, account *Account) *MirasimPlanSnapshot {
	t.Helper()
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

// TestMirasimMalformedReferralNeverPollutesPlanState feeds the probe two
// responses our ported struct cannot read and checks that neither one writes a
// plan value or destroys an existing one.
//
// Case A — a 200 whose JSON uses DIFFERENT FIELD NAMES. This is the dangerous
// one, because it is not an error at any layer: json.Unmarshal succeeds, the
// struct is all zeros, and only the empty CurrentPlan distinguishes it from a
// genuine reading. It is exactly what production would see if ma-relay's field
// names are stale or if this account's auth server speaks a different version.
//
// Case B — a 200 that is not JSON at all (an error page, a proxy interstitial).
func TestMirasimMalformedReferralNeverPollutesPlanState(t *testing.T) {
	// [[cov:PL:malformed-referral-is-safe]]
	ctx := context.Background()
	account := planEvidenceAccount("max")
	repo := &planEvidenceRepo{account: account}
	upstream := &planEvidenceUpstream{}
	svc := NewMirasimPlanProbeService(repo, upstream, nil)

	// A good reading first, so there is something to pollute. Claimed says max,
	// the server says plus — the "标 max 实为 plus" state the probe exists to find.
	upstream.serve(http.StatusOK, `{"code":"TESTCODE","redeemed":14,"threshold":10,"reached":true,`+
		`"current_plan":"plus","next_plan":"max","plan_expires_at":"2027-09-10T19:20:40.460323Z"}`)
	good, err := svc.ProbeAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("baseline ProbeAccount error = %v", err)
	}
	if good.Status != MirasimPlanProbeStatusOK || good.Plan != "plus" {
		t.Fatalf("baseline snapshot = status %q plan %q, want %q/plus (last_error=%q)",
			good.Status, good.Plan, MirasimPlanProbeStatusOK, good.LastError)
	}

	// ---- Case A: valid JSON, unknown field names -------------------------
	//
	// Every value in this body is a poison pill: if any of them reaches the
	// stored snapshot or the resolved item, the struct silently accepted a shape
	// it does not understand.
	upstream.serve(http.StatusOK, `{"tier":"enterprise","subscription_expires_at":"2099-01-01T00:00:00Z",`+
		`"invitations":{"used":99,"required":1},"upgrade_pending":true}`)
	shapeMismatch, err := svc.ProbeAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("shape-mismatch ProbeAccount error = %v", err)
	}

	if shapeMismatch.Status != MirasimPlanProbeStatusFailed {
		t.Fatalf("a 200 our struct could not read was recorded as %q; want %q — an unreadable shape must never count as a reading",
			shapeMismatch.Status, MirasimPlanProbeStatusFailed)
	}
	// The observable signature of this exact case: a 200 that produced no plan.
	// http_status=200 with last_error=empty_current_plan is what tells an
	// operator "the server is fine, our struct is wrong" instead of "the network
	// is flaky".
	if shapeMismatch.HTTPStatus != http.StatusOK || shapeMismatch.LastError != "empty_current_plan" {
		t.Fatalf("shape-mismatch snapshot = http %d / %q, want 200 / empty_current_plan",
			shapeMismatch.HTTPStatus, shapeMismatch.LastError)
	}
	// Nothing from the unreadable body leaked in.
	if shapeMismatch.Plan != "plus" || shapeMismatch.ClaimedPlan != "max" {
		t.Fatalf("shape-mismatch snapshot overwrote the last good reading: plan %q claimed %q, want plus/max",
			shapeMismatch.Plan, shapeMismatch.ClaimedPlan)
	}
	if shapeMismatch.PlanExpiresAt != "2027-09-10T19:20:40.460323Z" {
		t.Fatalf("shape-mismatch snapshot.PlanExpiresAt = %q, want the last good value", shapeMismatch.PlanExpiresAt)
	}
	if shapeMismatch.Redeemed != 14 || shapeMismatch.Threshold != 10 {
		t.Fatalf("shape-mismatch snapshot invitations = %d/%d, want the last good 14/10",
			shapeMismatch.Redeemed, shapeMismatch.Threshold)
	}

	stored := planEvidenceStoredSnapshot(t, account)
	if blob, err := json.Marshal(stored); err != nil {
		t.Fatalf("marshal stored snapshot: %v", err)
	} else if strings.Contains(string(blob), "enterprise") || strings.Contains(string(blob), "2099") {
		t.Fatalf("a value from the unreadable body reached the stored snapshot: %s", blob)
	}

	// The importer's claim is untouched, and the probe still owns exactly one key.
	if got := account.Extra[mirasim.ExtraPlanClaimed]; got != "max" {
		t.Fatalf("extra[%s] = %v, want the untouched claim %q", mirasim.ExtraPlanClaimed, got, "max")
	}
	for _, key := range repo.writtenKeys() {
		if key != mirasim.ExtraPlanProbe {
			t.Fatalf("probe wrote extra key %q; it may only write %q", key, mirasim.ExtraPlanProbe)
		}
	}

	// A failed probe is not evidence, so the resolved view falls back to the
	// claim rather than promoting anything the malformed response implied.
	item := ResolveMirasimPlan(account)
	if item.Plan != "max" || item.PlanSource != MirasimPlanSourceClaimed {
		t.Fatalf("ResolveMirasimPlan = plan %q source %q, want the claim max/%s",
			item.Plan, item.PlanSource, MirasimPlanSourceClaimed)
	}
	if item.PlanExpiresAt == "2099-01-01T00:00:00Z" {
		t.Fatalf("ResolveMirasimPlan adopted the unreadable body's expiry %q", item.PlanExpiresAt)
	}
	if got := BuildMirasimPlanItems([]Account{*account}, "enterprise", false); len(got) != 0 {
		t.Fatalf("filtering on the unreadable body's tier returned %d accounts, want 0", len(got))
	}

	// ---- Case B: a 200 that is not JSON ----------------------------------
	upstream.serve(http.StatusOK, `<html><head><title>502 Bad Gateway</title></head><body>nginx</body></html>`)
	notJSON, err := svc.ProbeAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("non-JSON ProbeAccount error = %v", err)
	}
	if notJSON.Status != MirasimPlanProbeStatusFailed {
		t.Fatalf("a non-JSON body was recorded as %q, want %q", notJSON.Status, MirasimPlanProbeStatusFailed)
	}
	// KNOWN OBSERVABILITY GAP, pinned rather than papered over: the decode error
	// is not an upstream rejection, so mirasimPlanProbeFailureReason cannot tell
	// it apart from a transport failure and the HTTP status is lost. If this ever
	// needs to be distinguishable, that is a change to the reason mapping — not
	// something to fix by loosening this assertion.
	if notJSON.LastError != "request_failed" {
		t.Fatalf("non-JSON snapshot.LastError = %q, want request_failed", notJSON.LastError)
	}
	if notJSON.Plan != "plus" || notJSON.ClaimedPlan != "max" {
		t.Fatalf("non-JSON probe destroyed the last good reading: plan %q claimed %q", notJSON.Plan, notJSON.ClaimedPlan)
	}
	if got := account.Extra[mirasim.ExtraPlanClaimed]; got != "max" {
		t.Fatalf("extra[%s] = %v after a non-JSON response, want the untouched claim", mirasim.ExtraPlanClaimed, got)
	}
	afterItem := ResolveMirasimPlan(account)
	if afterItem.Plan != "max" || afterItem.PlanSource != MirasimPlanSourceClaimed {
		t.Fatalf("ResolveMirasimPlan after a non-JSON response = plan %q source %q, want max/%s",
			afterItem.Plan, afterItem.PlanSource, MirasimPlanSourceClaimed)
	}
}
