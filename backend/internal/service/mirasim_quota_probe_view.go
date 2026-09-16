package service

// The read side of a mirasim account's quota state: one composed view assembled
// from every source that can answer "how much is left on this account", with the
// provenance attached.
//
// THERE ARE EXACTLY TWO SOURCES, AND THEY ARE NOT INTERCHANGEABLE:
//
//	"limits"  — a /v1/limits probe. Absolute used and budget per window, for all
//	            four windows, on an account that may be receiving no traffic at
//	            all. Costs an upstream request.
//	"headers" — the unified-ratelimit headers sub2api already samples off normal
//	            responses (samplePassiveUsageFromHeaders). Free, but utilization
//	            only, 5h and 7d only, and only while the account is in use.
//
// THE ONE THING THIS FILE REFUSES TO DO IS BLEND THEM. ma-relay's
// RecordQuotaHeaders merges header utilization into whatever windows a previous
// /v1/limits probe left behind, back-deriving used = utilization × budget from a
// budget read at some earlier time, and appends an EMPTY window shell for any
// window the headers mentioned but the probe never saw. Both moves manufacture
// data: the back-derived `used` is a product of two readings taken minutes or
// hours apart, and the empty shell carries budget 0, which every renderer reads
// as "we looked and there is nothing" rather than "we never looked".
//
// So: a probe reading is published whole, a header reading is published whole,
// and when only headers exist the absolute counters stay ABSENT (nil) rather
// than being reconstructed. Absent is a state the UI can render honestly.

import (
	"strings"
	"time"
)

// Passive-sample keys written by RateLimitService.samplePassiveUsageFromHeaders.
// Named here so the read side and the write side cannot drift apart silently.
const (
	mirasimPassive5hUtilizationKey = "session_window_utilization"
	mirasimPassive7dUtilizationKey = "passive_usage_7d_utilization"
	mirasimPassive7dResetKey       = "passive_usage_7d_reset"
	mirasimPassiveSampledAtKey     = "passive_usage_sampled_at"
)

// MirasimQuotaSnapshot is the composed, read-only view of one mirasim account's
// quota and subscription state.
//
// Source is the field every consumer must branch on before trusting anything
// else: "limits" means used/budget are real numbers, "headers" means only
// Utilization is populated, and "" means nothing has ever been read — which is
// NOT the same as an account with no quota left.
type MirasimQuotaSnapshot struct {
	Source    string `json:"source"`
	Suspended bool   `json:"suspended,omitempty"`
	// Windows contains only windows that were actually observed. Never padded to
	// the four-window model: a missing window means "not measured".
	Windows []MirasimQuotaWindow `json:"windows"`
	// ObservedAt is when the windows above were read — from the probe when the
	// source is "limits", from the last passive sample when it is "headers".
	ObservedAt *time.Time `json:"observed_at,omitempty"`

	// Probe bookkeeping. ProbeStatus == "failed" means the numbers above (if
	// any) are a SURVIVING earlier reading, not a fresh one.
	ProbeStatus    string     `json:"probe_status,omitempty"`
	ProbeError     string     `json:"probe_error,omitempty"`
	FailureCount   int        `json:"failure_count,omitempty"`
	NextProbeAt    *time.Time `json:"next_probe_at,omitempty"`
	UnknownWindows []string   `json:"unknown_windows,omitempty"`

	// Subscription tier, resolved by the ONE authority on claimed-vs-probed
	// precedence (ResolveMirasimPlan). Re-reading extra here would be a second
	// precedence rule free to disagree with the first.
	Plan          string `json:"plan,omitempty"`
	NextPlan      string `json:"next_plan,omitempty"`
	PlanExpiresAt string `json:"plan_expires_at,omitempty"`
	PlanSource    string `json:"plan_source,omitempty"`
	PlanNote      string `json:"plan_note,omitempty"`
}

// BuildMirasimQuotaSnapshot composes the view for one account, or nil when the
// account is not a mirasim account.
//
// nil rather than an empty struct: a non-mirasim account has no mirasim quota,
// and an empty struct would render as "a mirasim account we know nothing about".
func BuildMirasimQuotaSnapshot(account *Account) *MirasimQuotaSnapshot {
	if !IsMirasimAccount(account) {
		return nil
	}
	view := &MirasimQuotaSnapshot{Windows: []MirasimQuotaWindow{}}

	plan := ResolveMirasimPlan(account)
	view.Plan = plan.Plan
	view.NextPlan = plan.NextPlan
	view.PlanExpiresAt = plan.PlanExpiresAt
	view.PlanSource = plan.PlanSource
	view.PlanNote = plan.PlanNote

	if probe := DecodeMirasimQuotaProbeSnapshot(account.Extra); probe != nil {
		view.ProbeStatus = probe.Status
		view.FailureCount = probe.FailureCount
		view.UnknownWindows = probe.UnknownWindows
		if probe.Status == MirasimQuotaProbeStatusFailed {
			view.ProbeError = probe.LastError
		}
		if !probe.NextProbeAt.IsZero() {
			nextProbeAt := probe.NextProbeAt
			view.NextProbeAt = &nextProbeAt
		}
		// Suspension is set from ANY real reading, including one that carried no
		// windows at all: "the upstream has halted this account" is a fact about
		// the account, not about a window, and an account can be suspended
		// precisely when it has no window state to report. Gating it on the
		// window list would drop the single most important thing the probe
		// learned.
		if probe.ObservedAt != nil {
			view.Suspended = probe.Suspended
			view.ObservedAt = probe.ObservedAt
		}
		// ObservedAt is the gate for the WINDOWS, not Status: a failed probe
		// still carries the last real reading, and suppressing it would hide the
		// only quota information the operator has. ProbeStatus says how fresh it
		// is.
		if len(probe.Windows) > 0 && probe.ObservedAt != nil {
			view.Source = MirasimQuotaSourceLimits
			view.Windows = append(view.Windows, probe.Windows...)
		}
	}

	if len(view.Windows) == 0 {
		if windows, sampledAt := mirasimPassiveQuotaWindows(account); len(windows) > 0 {
			view.Source = MirasimQuotaSourceHeaders
			view.Windows = windows
			view.ObservedAt = sampledAt
		}
	}
	return view
}

// mirasimPassiveQuotaWindows reconstructs what the response headers told us, and
// nothing more.
//
// Used and Budget stay nil. The headers never carried them, and the only way to
// produce them would be to multiply this utilization by a budget read at some
// other time — a number that was never true at any single instant. Utilization
// alone is enough to draw a bar; a fabricated absolute is enough to make a
// billing decision, which is why the two must not be confused.
//
// Only 5h and 7d appear: those are the windows sub2api samples. The family
// windows are absent, which is exactly right — nothing observed them.
func mirasimPassiveQuotaWindows(account *Account) ([]MirasimQuotaWindow, *time.Time) {
	if account == nil || account.Extra == nil {
		return nil, nil
	}
	sampledAt := mirasimExtraTime(account, mirasimPassiveSampledAtKey)
	if sampledAt == nil {
		// Without a sample timestamp there is no way to say how old these
		// numbers are, and an undated utilization is not a reading.
		return nil, nil
	}

	windows := make([]MirasimQuotaWindow, 0, 2)
	if utilization, ok := mirasimExtraFloat(account, mirasimPassive5hUtilizationKey); ok {
		value := utilization
		windows = append(windows, MirasimQuotaWindow{Name: MirasimWindow5h, Utilization: &value})
	}
	if utilization, ok := mirasimExtraFloat(account, mirasimPassive7dUtilizationKey); ok {
		value := utilization
		window := MirasimQuotaWindow{Name: MirasimWindow7d, Utilization: &value}
		if reset, ok := mirasimExtraInt64(account, mirasimPassive7dResetKey); ok && reset > 0 {
			resetAt := time.Unix(reset, 0).UTC()
			window.ResetAt = &resetAt
		}
		windows = append(windows, window)
	}
	if len(windows) == 0 {
		return nil, nil
	}
	return windows, sampledAt
}

// mirasimExtraFloat tolerates every numeric shape JSONB round-trips through.
// The bool return is the whole point: a missing key and a stored 0.0 are
// different answers.
func mirasimExtraFloat(account *Account, key string) (float64, bool) {
	if account == nil || account.Extra == nil {
		return 0, false
	}
	switch typed := account.Extra[key].(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}

func mirasimExtraInt64(account *Account, key string) (int64, bool) {
	if account == nil || account.Extra == nil {
		return 0, false
	}
	switch typed := account.Extra[key].(type) {
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	}
	return 0, false
}

func mirasimExtraTime(account *Account, key string) *time.Time {
	raw := strings.TrimSpace(mirasimExtraString(account, key))
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}
