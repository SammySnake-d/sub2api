package admin

// Import-side coverage for mirasim subscription plan fields.
//
// The source of truth for the shape tested here is ma-relay's credentials.json,
// which carries plan / plan_expires_at / redeemed / threshold / next_plan next to
// the device seed and tokens. Before this, the importer parsed none of them.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// mirasimPlanImportEntryJSON is one ma-relay credentials.json record, including
// the plan block. plan_expires_at deliberately carries the microsecond precision
// the real export uses, and expires_at (the ACCESS TOKEN's death) is a different
// instant entirely — the pair is what makes a conflation of the two visible.
func mirasimPlanImportEntryJSON(t *testing.T, name, seed, plan string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"name":                name,
		"mirasim_device_seed": seed,
		"access_token":        "test-access-token-" + name,
		"refresh_token":       "test-refresh-token-" + name,
		"expires_at":          "2030-01-02T03:04:05Z",
		"plan":                plan,
		"plan_expires_at":     "2027-09-10T19:20:40.460323Z",
		"redeemed":            14,
		"threshold":           10,
		"next_plan":           "max",
	})
	if err != nil {
		t.Fatalf("marshal mirasim plan import entry: %v", err)
	}
	return string(payload)
}

// TestImportMirasimAccountsCarriesPlanFieldsIntoExtra proves the importer no
// longer drops the subscription block, and that it keeps the subscription expiry
// separate from the access token expiry.
func TestImportMirasimAccountsCarriesPlanFieldsIntoExtra(t *testing.T) {
	// [[cov:PL:import-carries-plan]]
	seed := mirasimImportTestSeed(t, "test-seed-plan01")
	svc := newCodexImportMemoryAdminService(nil)
	handler := mirasimImportTestHandler(svc)
	req := mirasimImportTestRequest()
	req.Content = mirasimPlanImportEntryJSON(t, "alpha", seed, "max")

	entries, err := parseMirasimImportEntries(req)
	if err != nil {
		t.Fatalf("parseMirasimImportEntries error = %v", err)
	}
	result, err := handler.importMirasimAccounts(context.Background(), req, entries)
	if err != nil {
		t.Fatalf("importMirasimAccounts error = %v", err)
	}
	if result.Created != 1 || result.Failed != 0 {
		t.Fatalf("MirasimAccountImportResult = created %d / failed %d, want 1/0", result.Created, result.Failed)
	}
	if len(svc.createdAccounts) != 1 {
		t.Fatalf("service.CreateAccount called %d times, want 1", len(svc.createdAccounts))
	}
	created := svc.createdAccounts[0]

	if got := created.Extra[mirasim.ExtraPlanClaimed]; got != "max" {
		t.Fatalf("extra[%s] = %v, want the source plan %q", mirasim.ExtraPlanClaimed, got, "max")
	}
	// Byte-identical to the source: an operator diffing sub2api against ma-relay
	// must not see a value that was silently re-rendered.
	if got := created.Extra[mirasim.ExtraPlanExpiresAt]; got != "2027-09-10T19:20:40.460323Z" {
		t.Fatalf("extra[%s] = %v, want the source subscription expiry 2027-09-10T19:20:40.460323Z", mirasim.ExtraPlanExpiresAt, got)
	}
	if got := created.Extra[mirasim.ExtraPlanRedeemed]; got != int64(14) {
		t.Fatalf("extra[%s] = %#v, want 14", mirasim.ExtraPlanRedeemed, got)
	}
	if got := created.Extra[mirasim.ExtraPlanThreshold]; got != int64(10) {
		t.Fatalf("extra[%s] = %#v, want 10", mirasim.ExtraPlanThreshold, got)
	}
	if got := created.Extra[mirasim.ExtraPlanNext]; got != "max" {
		t.Fatalf("extra[%s] = %v, want %q", mirasim.ExtraPlanNext, got, "max")
	}

	// The two expiries must not have been conflated: the token expiry stays in
	// credentials under mirasim.CredExpiresAt and keeps its own, different value.
	if got := created.Credentials[mirasim.CredExpiresAt]; got != "2030-01-02T03:04:05Z" {
		t.Fatalf("credentials[%s] = %v, want the ACCESS TOKEN expiry 2030-01-02T03:04:05Z", mirasim.CredExpiresAt, got)
	}
	if created.Extra[mirasim.ExtraPlanExpiresAt] == created.Credentials[mirasim.CredExpiresAt] {
		t.Fatalf("subscription expiry and access token expiry collapsed to the same value")
	}

	// The importer writes a CLAIM, and nothing here may look like a verified
	// reading: the probe snapshot key must still be absent.
	if _, probed := created.Extra[mirasim.ExtraPlanProbe]; probed {
		t.Fatalf("import wrote extra[%s]; only the plan probe may write that key", mirasim.ExtraPlanProbe)
	}

	// And the resolved view says so.
	item := service.ResolveMirasimPlan(&service.Account{
		Platform:    service.PlatformAnthropic,
		Credentials: created.Credentials,
		Extra:       created.Extra,
	})
	if item.Plan != "max" || item.PlanSource != service.MirasimPlanSourceClaimed {
		t.Fatalf("ResolveMirasimPlan = plan %q source %q, want max/%s", item.Plan, item.PlanSource, service.MirasimPlanSourceClaimed)
	}
}

// TestImportMirasimAccountsBackfillsPlanFieldsOnExistingAccounts is the backfill
// path for the accounts imported before plan fields were parsed: re-running the
// same source file with update_existing must add the plan block to the stored
// extra without disturbing what is already there (the session id in particular,
// which is minted per account and must survive).
func TestImportMirasimAccountsBackfillsPlanFieldsOnExistingAccounts(t *testing.T) {
	seed := mirasimImportTestSeed(t, "test-seed-plan02")
	existing := service.Account{
		ID:       7,
		Name:     "alpha",
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			mirasim.CredProvider:    mirasim.ProviderMirasim,
			mirasim.CredDeviceSeed:  seed,
			mirasim.CredAccessToken: "test-access-token-alpha",
		},
		// An account as it looked before this change: no plan keys at all.
		Extra: map[string]any{
			"anthropic_passthrough": true,
			mirasim.ExtraSessionID:  "session_minted_earlier",
		},
	}
	svc := newCodexImportMemoryAdminService([]service.Account{existing})
	handler := mirasimImportTestHandler(svc)
	req := mirasimImportTestRequest()
	req.UpdateExisting = boolPtr(true)
	req.Content = mirasimPlanImportEntryJSON(t, "alpha", seed, "plus")

	entries, err := parseMirasimImportEntries(req)
	if err != nil {
		t.Fatalf("parseMirasimImportEntries error = %v", err)
	}
	result, err := handler.importMirasimAccounts(context.Background(), req, entries)
	if err != nil {
		t.Fatalf("importMirasimAccounts error = %v", err)
	}
	if result.Updated != 1 || result.Created != 0 || result.Failed != 0 {
		t.Fatalf("MirasimAccountImportResult = updated %d / created %d / failed %d, want 1/0/0",
			result.Updated, result.Created, result.Failed)
	}
	if len(svc.updatedAccounts) != 1 {
		t.Fatalf("service.UpdateAccount called %d times, want 1", len(svc.updatedAccounts))
	}
	updated := svc.updatedAccounts[0].input
	if updated.Extra[mirasim.ExtraPlanClaimed] != "plus" {
		t.Fatalf("backfilled extra[%s] = %v, want plus", mirasim.ExtraPlanClaimed, updated.Extra[mirasim.ExtraPlanClaimed])
	}
	if updated.Extra[mirasim.ExtraPlanExpiresAt] != "2027-09-10T19:20:40.460323Z" {
		t.Fatalf("backfilled extra[%s] = %v, want the source subscription expiry", mirasim.ExtraPlanExpiresAt, updated.Extra[mirasim.ExtraPlanExpiresAt])
	}
	if updated.Extra[mirasim.ExtraSessionID] != "session_minted_earlier" {
		t.Fatalf("backfill overwrote extra[%s] = %v, want the previously minted session id",
			mirasim.ExtraSessionID, updated.Extra[mirasim.ExtraSessionID])
	}
}
