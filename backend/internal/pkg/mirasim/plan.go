package mirasim

// Subscription plan (tier) state for a mirasim account.
//
// TWO INDEPENDENT SOURCES, DELIBERATELY KEPT APART:
//
//  1. CLAIMED — what the import source said. ma-relay's credentials.json carries
//     plan / plan_expires_at / redeemed / threshold / next_plan per account, and
//     the importer copies them into accounts.extra under the Extra* keys below.
//     This is an import-time label: frequently over-stated as "max".
//
//  2. AUTHORITATIVE — what the server actually grants, read from
//     GET <auth base>/auth/referral. current_plan is the tier in force now.
//
//     EVIDENCE GRADE: OBSERVED (2026-09-16). The request shape and response field
//     names were ported from ma-relay (internal/relay/referral.go), and have
//     since been confirmed against the live endpoint from this repository: the
//     production deployment carries real probe results in accounts.extra, e.g.
//
//	{"plan":"plus","next_plan":"max","threshold":10,"http_status":200,
//	 "plan_expires_at":"2026-10-08T07:02:27.551784Z","status":"ok"}
//
//     A non-zero plan / non-zero plan_expires_at could not have been produced by
//     a mis-ported field set — both failure modes below end in a recorded
//     failure, never in a plausible-looking value. See ReferralInfo.
//
// The two disagree in production and the disagreement is the interesting signal,
// not noise: an account labelled max whose current_plan is still plus has met
// the invitation threshold but the upgrade has not settled. ma-relay kept a
// dedicated PlanNote field for exactly these. So the probe NEVER writes over the
// claimed keys — it writes its own snapshot (ExtraPlanProbe) and the reader
// resolves precedence explicitly. See PlanDiffNote.
//
// NOT TO BE CONFUSED WITH TOKEN EXPIRY: credentials.expires_at is when the
// access token dies (minutes/hours). ExtraPlanExpiresAt is when the paid
// subscription ends (months/years). They are unrelated.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Plan fields live in accounts.extra, not accounts.credentials: none of them is
// a secret, the admin DTO does not redact extra (so the operator console can
// filter on them), and every write to credentials on an apikey account drops
// extra.upstream_billing_probe.
const (
	// ExtraPlanClaimed is the plan label the import source declared
	// ("plus" | "max"). Claimed, not verified.
	ExtraPlanClaimed = "mirasim_plan"
	// ExtraPlanExpiresAt is the claimed SUBSCRIPTION expiry (RFC3339), e.g.
	// "2027-09-10T19:20:40.460323Z". Not the access token's expires_at.
	ExtraPlanExpiresAt = "mirasim_plan_expires_at"
	// ExtraPlanRedeemed is the claimed count of redeemed invitations.
	ExtraPlanRedeemed = "mirasim_plan_redeemed"
	// ExtraPlanThreshold is the claimed invitation count required to upgrade.
	ExtraPlanThreshold = "mirasim_plan_threshold"
	// ExtraPlanNext is the claimed tier the account upgrades into.
	ExtraPlanNext = "mirasim_plan_next"
	// ExtraPlanProbe holds the authoritative /auth/referral snapshot written by
	// the plan probe. It is the ONLY key the probe writes.
	ExtraPlanProbe = "mirasim_plan_probe"
	// ExtraPlanProbeEnabled is the per-account opt-OUT (absent means enabled):
	// plan discovery is the reason a mirasim account is imported at all, so the
	// default is on and an operator switches individual accounts off.
	ExtraPlanProbeEnabled = "mirasim_plan_probe_enabled"
)

// referralTimeout bounds one /auth/referral call. Matches ma-relay's.
const referralTimeout = 15 * time.Second

// ReferralInfo is the account's referral / upgrade state from
// GET <auth base>/auth/referral.
//
// This is the AUTHORITATIVE plan signal. current_plan reflects the tier the
// server actually grants, independent of:
//   - the imported `plan` label (an import-time guess, often over-stated), and
//   - the /v1/limits budget (a shared placeholder ~11667 for accounts whose real
//     budget the server has not synced to this egress).
//
// A "标 max 实为 plus" account shows current_plan=plus, reached=true,
// next_plan=max: invitations met the threshold but the plus→max upgrade has not
// settled yet.
//
// ---------------------------------------------------------------------------
// EVIDENCE GRADE: PORTED, NOT OBSERVED.
// ---------------------------------------------------------------------------
//
// Every json tag below was copied from ma-relay internal/relay/referral.go
// (lines 25-35). As of 2026-09-16 the ported shape has been confirmed against the
// live auth server FROM THIS REPOSITORY: the production deployment's
// accounts.extra->'mirasim_plan_probe' holds real readings (http_status 200,
// current_plan "plus", next_plan "max", threshold 10, a real plan_expires_at).
//
// What is still second-hand is the shape of the fields we DON'T read. A response
// carrying extra fields, or a rename of a field we never look at, would be
// invisible here — the confirmation covers the subset below, not the endpoint.
//
// WHAT HAPPENS IF THE SHAPE IS WRONG — both failure modes end in a recorded
// failure, never in a wrong plan value:
//
//  1. The body is not JSON (an HTML error page, a proxy interstitial):
//     FetchReferral's json.Unmarshal fails and it returns an error. The caller
//     records a failed probe.
//
//  2. The body is JSON but uses different names ({"tier":"max"}): decoding
//     SUCCEEDS into an all-zero struct — this is encoding/json's documented
//     behaviour for unknown fields and it is silent. The zero value is the whole
//     reason CurrentPlan must be checked: a caller that trusted this struct
//     would write Plan="" over a real reading. service.probeLoadedAccount is
//     that check — it rejects an empty CurrentPlan as "empty_current_plan" with
//     http_status=200, which is the observable signature of exactly this case:
//     a 200 that our struct could not read.
//
// So the safety of case 2 lives in the CALLER, not here. Do not make that check
// optional, and do not add a default/fallback value to any field below: a
// defaulted field would turn "the shape is wrong" into a plausible-looking plan.
type ReferralInfo struct {
	Code           string `json:"code"`
	Redeemed       int    `json:"redeemed"`
	Threshold      int    `json:"threshold"`
	Remaining      int    `json:"remaining"`
	Reached        bool   `json:"reached"`
	MaxRedemptions int    `json:"max_redemptions"`
	CurrentPlan    string `json:"current_plan"`
	NextPlan       string `json:"next_plan"`
	PlanExpiresAt  string `json:"plan_expires_at"`
}

// PendingUpgrade reports whether invitations have met the threshold but the
// account is still on a lower plan than it has earned. These are the
// "达标未结算" accounts.
func (r *ReferralInfo) PendingUpgrade() bool {
	if r == nil {
		return false
	}
	return r.Reached && r.NextPlan != "" && r.NextPlan != r.CurrentPlan
}

// ReferralRejectedError signals that the auth server answered /auth/referral
// with a non-2xx status, as opposed to the request never arriving. Callers can
// record the status without inspecting the body.
type ReferralRejectedError struct{ Status int }

func (e *ReferralRejectedError) Error() string {
	return fmt.Sprintf("mirasim referral returned HTTP %d", e.Status)
}

// ReferralStatus extracts the upstream HTTP status from a FetchReferral error,
// or 0 when the failure was not an upstream rejection.
func ReferralStatus(err error) int {
	var rejected *ReferralRejectedError
	if errors.As(err, &rejected) {
		return rejected.Status
	}
	return 0
}

// PlanDiffNote describes a claimed-vs-authoritative discrepancy in one operator
// readable line, and returns "" when they agree.
//
// Ported from ma-relay internal/relay/pool.go planDiffNote, including its
// wording, so an operator reading sub2api sees the same note they already read
// in ma-relay for the same account. The wording is therefore as verified as
// ma-relay's own — which is to say the note is only as true as the ReferralInfo
// that fed it; see that type's evidence grade. It returns "" whenever ref is nil
// or CurrentPlan is empty, so an unreadable response produces no note rather
// than a confident-sounding one.
func PlanDiffNote(claimedPlan string, ref *ReferralInfo) string {
	claimedPlan = strings.TrimSpace(claimedPlan)
	if ref == nil || ref.CurrentPlan == "" || claimedPlan == "" || claimedPlan == ref.CurrentPlan {
		return ""
	}
	if claimedPlan == "max" && ref.CurrentPlan == "plus" {
		if ref.Threshold > 0 && ref.Redeemed >= ref.Threshold {
			return fmt.Sprintf("标注max实为plus(邀请%d/%d达标,待升级→%s)", ref.Redeemed, ref.Threshold, strings.ToUpper(ref.NextPlan))
		}
		return fmt.Sprintf("标注max实为plus(邀请%d/%d)", ref.Redeemed, ref.Threshold)
	}
	return fmt.Sprintf("标注%s实为%s", claimedPlan, ref.CurrentPlan)
}

// FetchReferral reads one account's authoritative plan state.
//
// RETURN CONTRACT — a non-nil *ReferralInfo means "the auth server answered 2xx
// and the body was valid JSON". It does NOT mean the body was a referral: a 2xx
// whose JSON uses field names our ported struct does not know decodes into an
// all-zero struct with no error (see ReferralInfo, evidence grade). Callers MUST
// reject an empty CurrentPlan instead of persisting what they got.
//
// The unverified field set is deliberately not defended against here. Returning
// an error for an all-zero decode would collapse "200 with an unreadable shape"
// into the same bucket as a transport failure, and the HTTP status is precisely
// what tells an operator which of the two happened.
//
// It reuses the SAME per-account credential machinery as an outbound data-plane
// request — identity sync, and a token refresh when the access token is within
// refreshBefore of expiry, persisted through the supplied Persister — so a plan
// probe never sends a stale token and never forks a second view of the account's
// token state.
//
// WHY THE REQUEST IS NOT DEVICE-SIGNED. Unlike /v1/messages and the /v1/limits
// usage probe (which the relay answers with a shared placeholder unless the
// request carries the x-mirasim-* signature), /auth/referral lives on the AUTH
// server, and its authentication is the plain access-token bearer. That is
// exactly what ma-relay's FetchReferral sends — authorization + content-type and
// nothing else — and it is the same shape as this package's own /auth/refresh
// call a few lines away in runtime.go. Signing here would replace the bearer
// with the relay-issued device ticket, which the auth server never issued and
// has no reason to accept. The caller's Doer is still the account's own egress,
// so the IP and TLS fingerprint are unchanged.
func (r *Registry) FetchReferral(
	ctx context.Context,
	accountID int64,
	ident Identity,
	client Doer,
	persistCredentials Persister,
) (*ReferralInfo, error) {
	if r == nil {
		return nil, errors.New("mirasim registry is nil")
	}
	if client == nil {
		return nil, errors.New("mirasim referral probe has no HTTP client")
	}

	c := r.get(accountID)
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.syncLocked(ident); err != nil {
		return nil, err
	}
	if err := c.ensureFreshLocked(ctx, client, persistCredentials); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(c.accessToken)
	if token == "" {
		return nil, errors.New("mirasim account has no access token to read its plan with")
	}

	base := strings.TrimRight(strings.TrimSpace(c.authBase), "/")
	if base == "" {
		base = DefaultAuthBase
	}
	reqCtx, cancel := context.WithTimeout(ctx, referralTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/auth/referral", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("content-type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mirasim referral transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxControlBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &ReferralRejectedError{Status: resp.StatusCode}
	}
	var info ReferralInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("decode mirasim referral: %w", err)
	}
	return &info, nil
}
