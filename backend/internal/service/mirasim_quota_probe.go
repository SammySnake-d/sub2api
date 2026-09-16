package service

// Mirasim quota-window probe.
//
// WHAT IT ANSWERS: "how much of each of this account's four quota windows is
// already spent, and when does each reset" — proactively, before the scheduler
// spends a request finding out.
//
// WHY PROACTIVE AT ALL. sub2api already learns window exhaustion reactively,
// from the unified-ratelimit headers on a 429 (mirasim_scheduling.go). That path
// is correct but it is paid for: every discovery costs one upstream rejection,
// and on a 143-account pool a spent 7-day window is rediscovered on every
// scheduling attempt until something writes a cooldown. GET /v1/limits answers
// the same question for zero tokens — it is a GET with no body, so it consumes
// requests, not quota.
//
// WHAT IT IS NOT ALLOWED TO DO. Three invariants, each of which is the reason a
// specific failure cannot happen here:
//
//  1. A FAILED PROBE NEVER PRODUCES A HEALTHY-LOOKING SNAPSHOT. Transport
//     failure, non-2xx, unreadable body, or a 2xx that decoded into zero windows
//     all land in persistQuotaProbeFailure with status=failed, an http_status and
//     a stable reason label. "We could not read it" and "we read it and it says
//     zero" must never be the same stored value, because the second one is a
//     scheduling instruction and the first one is not.
//
//  2. A WINDOW THE UPSTREAM DID NOT MENTION DOES NOT APPEAR. No zero-filling, no
//     placeholder rows. ma-relay's passive header merge deliberately appends an
//     empty window shell (RecordQuotaHeaders) and that is exactly the behaviour
//     NOT copied here: a shell with budget 0 renders as "explored, found
//     nothing" everywhere downstream.
//
//  3. A COOLDOWN IS ONLY EVER WRITTEN THROUGH THE EXISTING WRITE PATH.
//     persistMirasimWindowLimitSet — the same function the 429 path uses, with
//     the same shouldPersistAnthropicWindowLimit precedence that refuses to
//     shorten a live cooldown. This file selects windows; it does not invent a
//     second way to store them.
//
// SHAPE: modelled on MirasimPlanProbeService — leader-locked periodic runner,
// bounded batch per cycle, per-account next_probe_at, singleflight per account,
// plus manual single/batch entry points. The cadence is NOT inherited from it;
// see the constants below for why.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	// CADENCE — deliberately NOT the plan probe's.
	//
	// A plan changes on the order of weeks, so that probe runs every 6h and
	// sweeps 20 accounts per 5-minute cycle. Quota moves on the order of
	// minutes, and the whole point of this probe is to notice exhaustion BEFORE
	// the scheduler does. Inheriting 20-per-5-minutes would make a 143-account
	// pool take 8 cycles ≈ 40 minutes to sweep once, so the effective refresh
	// interval would be max(interval, 40min) and a spent window could stay
	// invisible for 40 minutes no matter what interval was configured. The pair
	// below sweeps 143 accounts in ~4 cycles ≈ 8 minutes, which keeps the
	// configured interval the binding constraint rather than the batch size.
	mirasimQuotaProbeDefaultIntervalMinutes = 20
	mirasimQuotaProbeMinIntervalMinutes     = 5
	mirasimQuotaProbeCycleInterval          = 2 * time.Minute
	mirasimQuotaProbeMaxPerCycle            = 40
	// CONCURRENCY — bounded by egress, not by CPU. Every probe leaves through
	// its own account's residential proxy, so N concurrent probes light up N
	// distinct proxy circuits at once. ma-relay's background quota loop settled
	// on 2 for exactly this reason and reserves 8 for the operator-triggered
	// batch; 4 sits between them and still drains a 40-account cycle in seconds.
	mirasimQuotaProbeConcurrency = 4
	// Backoff ceiling for a repeatedly failing account. Long enough that a dead
	// account stops occupying cycle slots, short enough that a recovered one is
	// rediscovered the same day.
	mirasimQuotaProbeMaxDelay      = 6 * time.Hour
	mirasimQuotaProbeLeaderLockKey = "mirasim:quota:probe:leader"
	mirasimQuotaProbeLeaderLockTTL = 2 * time.Minute
)

// MirasimQuotaProbeMaxBatchSize limits one manual batch and one runner cycle.
const MirasimQuotaProbeMaxBatchSize = mirasimQuotaProbeMaxPerCycle

const (
	MirasimQuotaProbeStatusOK     = "ok"
	MirasimQuotaProbeStatusFailed = "failed"
)

// Where a quota reading came from. The distinction is load-bearing downstream:
// a "limits" reading carries absolute used/budget, a "headers" reading carries
// utilization only, and the two must never be blended into a row that looks like
// it has both.
const (
	MirasimQuotaSourceLimits  = "limits"
	MirasimQuotaSourceHeaders = "headers"
)

var (
	ErrMirasimQuotaProbeUnavailable = infraerrors.ServiceUnavailable(
		"MIRASIM_QUOTA_PROBE_UNAVAILABLE", "mirasim quota probe is unavailable",
	)
	ErrMirasimQuotaProbeAccountInvalid = infraerrors.BadRequest(
		"MIRASIM_QUOTA_PROBE_ACCOUNT_INVALID", "account is not a mirasim account",
	)
	ErrMirasimQuotaProbeIdentityChanged = infraerrors.Conflict(
		"MIRASIM_QUOTA_PROBE_IDENTITY_CHANGED", "account proxy changed during mirasim quota probe; retry the probe",
	)
)

// ---------------------------------------------------------------------------
// Stored shapes
// ---------------------------------------------------------------------------

// MirasimQuotaWindow is one window reading.
//
// EVERY MEASURED FIELD IS A POINTER, AND THAT IS THE POINT. nil means "this
// probe did not measure it"; a non-nil 0 means "the upstream said zero". Those
// two are opposite scheduling instructions — an unmeasured budget must never
// gate anything, while a measured used==budget is a hard stop — and a plain
// float64 cannot tell them apart. The frontend has the same problem in reverse:
// a nil utilization must render as "unknown", whereas 0.0 renders as a full
// green bar, i.e. "plenty left".
type MirasimQuotaWindow struct {
	// Name is the upstream's own window token, stored VERBATIM. It is not
	// normalised into the four tokens this repository expects, because an
	// unexpected token is the only evidence that those expectations are wrong
	// (see mirasimWindowTokenEvidence).
	Name string `json:"name"`
	// Used and Budget are absolute counts in the upstream's unit.
	Used   *float64 `json:"used,omitempty"`
	Budget *float64 `json:"budget,omitempty"`
	// Utilization is the 0..1 fraction. Present ONLY when Budget > 0: dividing
	// by a zero or absent budget would manufacture a number out of nothing.
	Utilization *float64 `json:"utilization,omitempty"`
	// ResetAt is when the window clears. Absent when the upstream sent none.
	ResetAt *time.Time `json:"reset_at,omitempty"`
}

// MirasimQuotaProbeSnapshot is what the probe persists under
// accounts.extra->'mirasim_quota_probe'. Structurally a sibling of
// MirasimPlanSnapshot: same status vocabulary, same attempt/next/failure
// bookkeeping, same "one key, written whole" rule.
type MirasimQuotaProbeSnapshot struct {
	Status string `json:"status"`
	// Source is always "limits" for a successful probe. It is recorded rather
	// than assumed so a reader never has to infer provenance from field
	// presence.
	Source string `json:"source,omitempty"`
	// Suspended is the upstream halting the whole account, which is independent
	// of any window being spent.
	Suspended bool `json:"suspended,omitempty"`
	// Windows holds ONLY the windows the upstream actually returned.
	Windows []MirasimQuotaWindow `json:"windows,omitempty"`
	// UnknownWindows are returned window names that mirasimWindowTokenEvidence
	// does not grade. Persisted, not just logged: a log line is gone by the time
	// an operator asks "which accounts saw this".
	UnknownWindows []string `json:"unknown_windows,omitempty"`
	// AppliedWindows are the windows whose exhaustion this probe actually wrote
	// into a cooldown. Empty on a healthy account, and empty on a failed probe.
	AppliedWindows []string `json:"applied_windows,omitempty"`

	// ObservedAt is when the numbers above were read. On a failed probe it is
	// carried over from the last successful one, so a reader can always tell how
	// stale the surviving reading is.
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
	LastAttemptAt time.Time  `json:"last_attempt_at"`
	NextProbeAt   time.Time  `json:"next_probe_at"`
	FailureCount  int        `json:"failure_count,omitempty"`
	HTTPStatus    int        `json:"http_status,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// MirasimQuotaProbeResult is returned by the manual probe entry points.
type MirasimQuotaProbeResult struct {
	AccountID int64                      `json:"account_id"`
	Snapshot  *MirasimQuotaProbeSnapshot `json:"snapshot,omitempty"`
	Error     string                     `json:"error,omitempty"`
}

// DecodeMirasimQuotaProbeSnapshot reads the snapshot out of extra. Returns nil
// for absent or malformed data rather than a zero snapshot, so a caller can
// never mistake "never probed" for "probed and found nothing".
func DecodeMirasimQuotaProbeSnapshot(extra map[string]any) *MirasimQuotaProbeSnapshot {
	if extra == nil {
		return nil
	}
	value, ok := extra[mirasim.ExtraQuotaProbe]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var snapshot MirasimQuotaProbeSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Status == "" {
		return nil
	}
	if snapshot.Status != MirasimQuotaProbeStatusOK && snapshot.Status != MirasimQuotaProbeStatusFailed {
		return nil
	}
	return &snapshot
}

// MirasimQuotaProbeEnabled is an opt-OUT: absent means enabled.
func MirasimQuotaProbeEnabled(account *Account) bool {
	if account == nil || account.Extra == nil {
		return true
	}
	enabled, ok := account.Extra[mirasim.ExtraQuotaProbeEnabled].(bool)
	if !ok {
		return true
	}
	return enabled
}

// ---------------------------------------------------------------------------
// From a reading to a scheduling decision
// ---------------------------------------------------------------------------

// mirasimQuotaProactiveWindows are the windows a /v1/limits snapshot is allowed
// to pre-emptively cool down.
//
// 5h IS DELIBERATELY ABSENT, and that is not an oversight — ma-relay makes the
// same exclusion (pool.go quotaExhaustedLocked: "Only the 7d windows gate; the
// transient 5h window is left to reactive 429 handling"). The 5h window is a
// ROLLING one: its `used` decays continuously as old requests age out, while
// `reset_at` is when it would clear if nothing else were sent. Writing a
// cooldown until reset_at on a momentarily-full 5h window would park a healthy
// account for hours it did not owe. A genuinely spent 5h window costs one 429,
// which the reactive path already handles with headers that are current by
// construction.
//
// The 7-day windows do not have that problem at this magnitude: they decay
// ~30x slower, so a snapshot taken minutes ago is still true, and the cost of
// NOT gating them is days of guaranteed 429s.
var mirasimQuotaProactiveWindows = map[string]bool{
	MirasimWindow7d:       true,
	MirasimWindow7dClaude: true,
	MirasimWindow7dFable:  true,
}

// mirasimQuotaMaxCooldown bounds how far into the future a probe-driven cooldown
// may reach. Mirrors parseAnthropicWindowReset's sanity window for the non-5h
// windows: a reset_at beyond this is a clock skew or a unit error, not a real
// eight-day quota window, and acting on it would park the account for longer
// than any real window can last.
const mirasimQuotaMaxCooldown = 8 * 24 * time.Hour

// mirasimQuotaWindowExhausted reports whether a reading says this window is
// spent RIGHT NOW.
//
// Every "unknown" answer is false. A window with no budget was not measured; a
// window with no used count was not measured; neither may gate scheduling,
// because the whole failure this probe exists to prevent is an account being
// parked on a number nobody actually read.
func mirasimQuotaWindowExhausted(window MirasimQuotaWindow, now time.Time) bool {
	spent := false
	switch {
	case window.Budget != nil && *window.Budget > 0 && window.Used != nil:
		spent = *window.Used >= *window.Budget
	case window.Utilization != nil:
		// A utilization-only reading (no absolute budget) can still be a hard
		// stop. 1.0 means the window is full by the upstream's own arithmetic.
		spent = *window.Utilization >= 1.0
	}
	if !spent {
		return false
	}
	if window.ResetAt == nil {
		// Spent, with no stated end. Exhausted as a fact — but see
		// selectMirasimExhaustedWindowsFromQuota, which refuses to turn it into a
		// cooldown because a cooldown needs an expiry.
		return true
	}
	return now.Before(*window.ResetAt)
}

// selectMirasimExhaustedWindowsFromQuota turns a quota reading into the cooldown
// set to write. It is the /v1/limits twin of selectMirasimExhaustedWindows,
// which does the same job from 429 headers, and both feed the one writer
// (persistMirasimWindowLimitSet).
//
// A window is dropped — silently, on purpose — when it is not one of the four
// graded tokens, when it is 5h (see mirasimQuotaProactiveWindows), when it is
// not exhausted, or when its reset_at is missing / in the past / implausibly far
// out. "Dropped" here means "no cooldown written", never "recorded as healthy":
// the reading itself is persisted verbatim either way, and an ungraded token is
// additionally reported by reportMirasimUnknownQuotaWindows.
//
// Iteration follows mirasimQuotaWindows rather than the response order so that,
// when more than one global window is spent, the account-level scalar settles on
// the longest cooldown — the same ordering guarantee the header path relies on.
func selectMirasimExhaustedWindowsFromQuota(windows []MirasimQuotaWindow, now time.Time) []mirasimWindowLimit {
	if len(windows) == 0 {
		return nil
	}
	byName := make(map[string]MirasimQuotaWindow, len(windows))
	for _, window := range windows {
		byName[window.Name] = window
	}
	var limits []mirasimWindowLimit
	for _, name := range mirasimQuotaWindows {
		if !mirasimQuotaProactiveWindows[name] {
			continue
		}
		window, ok := byName[name]
		if !ok || !mirasimQuotaWindowExhausted(window, now) {
			continue
		}
		if window.ResetAt == nil {
			slog.Warn("mirasim_quota_window_exhausted_without_reset",
				"window", name,
				"detail", "upstream reported the window spent but sent no reset_at; no cooldown can be written")
			continue
		}
		resetAt := *window.ResetAt
		if !resetAt.After(now) || resetAt.After(now.Add(mirasimQuotaMaxCooldown)) {
			// Loud, because the far-future case has a very specific cause: a
			// reset_at in MILLISECONDS read as seconds lands tens of thousands of
			// years out (this is why parseAnthropicResetTimestamp normalises
			// ts > 1e11). Acting on it would park the account permanently; the
			// bound turns that into a dropped cooldown, and this line is what
			// stops the drop from being invisible.
			slog.Warn("mirasim_quota_window_reset_out_of_range",
				"window", name,
				"reset_at", resetAt,
				"max_cooldown", mirasimQuotaMaxCooldown,
				"detail", "window reports exhausted but its reset_at is in the past or implausibly far out; no cooldown written")
			continue
		}
		scope, known := mirasimWindowScope(name)
		if !known {
			continue
		}
		limits = append(limits, mirasimWindowLimit{window: name, scope: scope, resetAt: resetAt})
	}
	return limits
}

// selectMirasimUnknownQuotaWindowNames returns the window names in a reading
// that mirasimWindowTokenEvidence does not grade, sorted and de-duplicated.
//
// This is the /v1/limits counterpart of selectMirasimUnknownWindows, and it is
// the stronger instrument of the two: the header scanner can only see tokens on
// a response that happened to be a 429, whereas every single successful quota
// probe enumerates the account's whole window set. Two of the four tokens this
// repository acts on (7d_claude, 7d_fable) have never been observed, and the
// counter-evidence says both are probably wrong. This list is how that gets
// settled with an observation instead of another round of reasoning.
func selectMirasimUnknownQuotaWindowNames(windows []MirasimQuotaWindow) []string {
	seen := make(map[string]struct{}, len(windows))
	var unknown []string
	for _, window := range windows {
		name := strings.TrimSpace(window.Name)
		if name == "" {
			continue
		}
		if _, graded := mirasimWindowTokenEvidence[name]; graded {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		unknown = append(unknown, name)
	}
	sort.Strings(unknown)
	return unknown
}

// reportMirasimQuotaWindowEvidence emits the two records a quota reading can
// produce about the token vocabulary itself.
//
// Unknown token → Warn. It is not a curiosity: if the upstream calls the fable
// window something we never look for, selectMirasimExhaustedWindowsFromQuota
// drops a real multi-day exhaustion and the scheduler keeps offering the
// account until the 429s stop, which they will not.
//
// Inferred token actually seen → Info, once per probe. This is the ONLY way
// MirasimWindowTokenInferred is ever discharged. It deliberately does not
// promote the grade on its own: the grade is a source-controlled claim about
// evidence, and a log line is not a commit.
func reportMirasimQuotaWindowEvidence(accountID int64, windows []MirasimQuotaWindow, unknown []string) {
	for _, name := range unknown {
		attrs := []any{
			"account_id", accountID,
			"window_token", name,
			"expected_tokens", mirasimQuotaWindows,
			"unverified_expectations", mirasimUnverifiedWindowExpectations(),
		}
		if hint, ok := mirasimAlternativeWindowTokens[name]; ok {
			attrs = append(attrs, "likely_meaning", hint)
		}
		slog.Warn("mirasim_quota_unrecognized_window", attrs...)
	}
	for _, window := range windows {
		grade, graded := mirasimWindowTokenEvidence[window.Name]
		if !graded || grade != MirasimWindowTokenInferred {
			continue
		}
		slog.Info("mirasim_quota_inferred_window_token_observed",
			"account_id", accountID,
			"window_token", window.Name,
			"detail", "a token graded inferred_not_observed just arrived on a real /v1/limits response; "+
				"if this repeats, promote it in mirasimWindowTokenEvidence")
	}
}

// ---------------------------------------------------------------------------
// The probe service
// ---------------------------------------------------------------------------

// MirasimQuotaProbeService periodically reads each mirasim account's quota
// windows and turns exhaustion into scheduling state.
type MirasimQuotaProbeService struct {
	accountRepo  AccountRepository
	httpUpstream HTTPUpstream
	// rateLimitService owns the cooldown write path. Optional: without it the
	// probe still records readings, it just cannot act on them — which is the
	// correct degradation, since the alternative would be a second writer.
	rateLimitService *RateLimitService

	registry     *mirasim.Registry
	parentCtx    context.Context
	parentCancel context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.Mutex
	started      bool
	stopped      bool
	cycleMu      sync.Mutex
	probeGroup   singleflight.Group
	probeSlots   chan struct{}
	now          func() time.Time
	lockCache    LeaderLockCache
	db           *sql.DB
	instanceID   string
	intervalMin  int
}

// NewMirasimQuotaProbeService builds the probe. planProbe is used ONLY for its
// credential registry (see MirasimPlanProbeService.SharedRegistry): the two
// probes hit the same accounts minutes apart, and separate registries would mean
// separate device tickets fighting over the same account identity.
func NewMirasimQuotaProbeService(
	accountRepo AccountRepository,
	httpUpstream HTTPUpstream,
	rateLimitService *RateLimitService,
	planProbe *MirasimPlanProbeService,
) *MirasimQuotaProbeService {
	ctx, cancel := context.WithCancel(context.Background())
	return &MirasimQuotaProbeService{
		accountRepo:      accountRepo,
		httpUpstream:     httpUpstream,
		rateLimitService: rateLimitService,
		registry:         planProbe.SharedRegistry(),
		parentCtx:        ctx,
		parentCancel:     cancel,
		probeSlots:       make(chan struct{}, mirasimQuotaProbeConcurrency),
		now:              time.Now,
		instanceID:       uuid.NewString(),
		intervalMin:      mirasimQuotaProbeDefaultIntervalMinutes,
	}
}

func (s *MirasimQuotaProbeService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// ProvideMirasimQuotaProbeService starts the process-wide periodic runner.
func ProvideMirasimQuotaProbeService(
	accountRepo AccountRepository,
	httpUpstream HTTPUpstream,
	rateLimitService *RateLimitService,
	planProbe *MirasimPlanProbeService,
	lockCache LeaderLockCache,
	db *sql.DB,
) *MirasimQuotaProbeService {
	svc := NewMirasimQuotaProbeService(accountRepo, httpUpstream, rateLimitService, planProbe)
	svc.SetLeaderLock(lockCache, db)
	svc.Start()
	return svc
}

func (s *MirasimQuotaProbeService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runLoop()
}

func (s *MirasimQuotaProbeService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.parentCancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *MirasimQuotaProbeService) runLoop() {
	defer s.wg.Done()
	_ = s.RunDue(s.parentCtx)
	ticker := time.NewTicker(mirasimQuotaProbeCycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			if err := s.RunDue(s.parentCtx); err != nil {
				logger.LegacyPrintf("service.mirasim_quota_probe", "run_due_failed: err=%v", err)
			}
		}
	}
}

// RunDue executes at most one bounded batch of due accounts.
//
// NO GLOBAL ENABLE SETTING, deliberately. The plan probe has one because its
// answer (a subscription tier) is an operator convenience that can be switched
// off without consequence. This probe's answer feeds the scheduler, so a global
// off switch would silently return the pool to discovering exhaustion one 429 at
// a time — a degradation nothing in the system reports. The two controls that do
// exist are honest about their blast radius: not wiring the service at all, and
// the per-account opt-out (mirasim.ExtraQuotaProbeEnabled).
func (s *MirasimQuotaProbeService) RunDue(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	// One instance per deployment does the sweep. Without this, N replicas each
	// probe all 143 accounts every cycle — N times the egress on the same
	// residential proxies, which is the traffic pattern most likely to get an
	// account flagged.
	release, acquired, lockErr := s.tryAcquireLeaderLock(ctx, mirasimQuotaProbeLeaderLockKey)
	if lockErr != nil {
		return fmt.Errorf("acquire mirasim quota probe leader lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer release()

	now := s.currentTime()
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformAnthropic)
	if err != nil {
		return fmt.Errorf("list anthropic accounts for mirasim quota probe: %w", err)
	}
	due := make([]Account, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		if !IsMirasimAccount(&account) || !account.IsActive() || !MirasimQuotaProbeEnabled(&account) {
			continue
		}
		snapshot := DecodeMirasimQuotaProbeSnapshot(account.Extra)
		if snapshot != nil && !snapshot.NextProbeAt.IsZero() && now.Before(snapshot.NextProbeAt) {
			continue
		}
		due = append(due, account)
	}
	sort.SliceStable(due, func(i, j int) bool {
		left := DecodeMirasimQuotaProbeSnapshot(due[i].Extra)
		right := DecodeMirasimQuotaProbeSnapshot(due[j].Extra)
		leftUnset := left == nil || left.NextProbeAt.IsZero()
		rightUnset := right == nil || right.NextProbeAt.IsZero()
		if leftUnset && rightUnset {
			return due[i].ID < due[j].ID
		}
		if leftUnset {
			return true
		}
		if rightUnset {
			return false
		}
		return left.NextProbeAt.Before(right.NextProbeAt)
	})
	if len(due) > mirasimQuotaProbeMaxPerCycle {
		due = due[:mirasimQuotaProbeMaxPerCycle]
	}

	var group errgroup.Group
	for i := range due {
		accountID := due[i].ID
		group.Go(func() error {
			if _, probeErr := s.probeAccountWithMode(ctx, accountID, true); probeErr != nil {
				logger.LegacyPrintf("service.mirasim_quota_probe", "probe_due_failed: account_id=%d err=%v", accountID, probeErr)
			}
			return nil
		})
	}
	return group.Wait()
}

// SetAccountEnabled flips the per-account opt-out.
func (s *MirasimQuotaProbeService) SetAccountEnabled(ctx context.Context, accountID int64, enabled bool) error {
	if s == nil || s.accountRepo == nil {
		return ErrMirasimQuotaProbeUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if !IsMirasimAccount(account) {
		return ErrMirasimQuotaProbeAccountInvalid
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{mirasim.ExtraQuotaProbeEnabled: enabled})
}

// ProbeAccount performs one manual probe, ignoring both switches.
func (s *MirasimQuotaProbeService) ProbeAccount(ctx context.Context, accountID int64) (*MirasimQuotaProbeSnapshot, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrMirasimQuotaProbeUnavailable
	}
	return s.probeAccountWithMode(ctx, accountID, false)
}

// ProbeAccounts performs a bounded manual batch with the runner's concurrency.
func (s *MirasimQuotaProbeService) ProbeAccounts(ctx context.Context, accountIDs []int64) []MirasimQuotaProbeResult {
	if len(accountIDs) > mirasimQuotaProbeMaxPerCycle {
		accountIDs = accountIDs[:mirasimQuotaProbeMaxPerCycle]
	}
	results := make([]MirasimQuotaProbeResult, len(accountIDs))
	if s == nil || s.accountRepo == nil {
		for i, accountID := range accountIDs {
			results[i] = MirasimQuotaProbeResult{AccountID: accountID, Error: ErrMirasimQuotaProbeUnavailable.Error()}
		}
		return results
	}
	var group errgroup.Group
	for i, accountID := range accountIDs {
		i, accountID := i, accountID
		results[i].AccountID = accountID
		group.Go(func() error {
			snapshot, err := s.probeAccountWithMode(ctx, accountID, false)
			if err != nil {
				results[i].Error = safeMirasimQuotaProbeError(err)
				return nil
			}
			results[i].Snapshot = snapshot
			return nil
		})
	}
	_ = group.Wait()
	return results
}

func (s *MirasimQuotaProbeService) probeAccountWithMode(ctx context.Context, accountID int64, requireEnabled bool) (*MirasimQuotaProbeSnapshot, error) {
	key := strconv.FormatInt(accountID, 10)
	value, err, _ := s.probeGroup.Do(key, func() (any, error) {
		select {
		case s.probeSlots <- struct{}{}:
			defer func() { <-s.probeSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		account, loadErr := s.accountRepo.GetByID(ctx, accountID)
		if loadErr != nil {
			return nil, loadErr
		}
		if !IsMirasimAccount(account) {
			return nil, ErrMirasimQuotaProbeAccountInvalid
		}
		if requireEnabled {
			if !account.IsActive() || !MirasimQuotaProbeEnabled(account) {
				return nil, nil
			}
			if snapshot := DecodeMirasimQuotaProbeSnapshot(account.Extra); snapshot != nil &&
				!snapshot.NextProbeAt.IsZero() && s.currentTime().Before(snapshot.NextProbeAt) {
				return nil, nil
			}
		}
		return s.probeLoadedAccount(ctx, account)
	})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	snapshot, ok := value.(*MirasimQuotaProbeSnapshot)
	if !ok {
		return nil, fmt.Errorf("invalid mirasim quota probe result")
	}
	return snapshot, nil
}

func (s *MirasimQuotaProbeService) probeLoadedAccount(ctx context.Context, account *Account) (*MirasimQuotaProbeSnapshot, error) {
	now := s.currentTime().UTC()
	if s.httpUpstream == nil {
		return s.persistQuotaProbeFailure(ctx, account, now, 0, "transport_unavailable")
	}

	// The probe MUST leave through the account's own proxy. A mirasim account is
	// pinned to one egress IP and the upstream binds the device to it; a probe
	// that went out direct would show the account operating from two IPs at once.
	proxyURL := ""
	if account.ProxyID != nil {
		if account.Proxy == nil {
			return s.persistQuotaProbeFailure(ctx, account, now, 0, "proxy_unavailable")
		}
		if account.Proxy.ID != *account.ProxyID {
			return nil, ErrMirasimQuotaProbeIdentityChanged
		}
		proxyURL = account.Proxy.URL()
	}

	identity := mirasim.Identity{
		DeviceSeed:   account.GetCredential(mirasim.CredDeviceSeed),
		AccessToken:  account.GetCredential(mirasim.CredAccessToken),
		RefreshToken: account.GetCredential(mirasim.CredRefreshToken),
		AuthBase:     account.GetCredential(mirasim.CredAuthBase),
		RelayBase:    strings.TrimSpace(account.GetCredential("base_url")),
		SessionID:    mirasimExtraString(account, mirasim.ExtraSessionID),
	}
	if expires := account.GetCredentialAsTime(mirasim.CredExpiresAt); expires != nil {
		identity.ExpiresAt = *expires
	}
	if strings.TrimSpace(identity.DeviceSeed) == "" {
		// Without a seed there is no signature, and an unsigned /v1/limits is
		// answered with a shared placeholder budget. Failing here is the only way
		// that placeholder never reaches the scheduler.
		return s.persistQuotaProbeFailure(ctx, account, now, 0, "missing_device_seed")
	}

	doer := &mirasimQuotaProbeDoer{
		upstream:    s.httpUpstream,
		proxyURL:    proxyURL,
		accountID:   account.ID,
		concurrency: account.Concurrency,
	}
	limits, err := s.registry.FetchLimits(ctx, account.ID, identity, doer,
		s.persistCredentials(account), s.persistExtra(account))
	if err != nil {
		return s.persistQuotaProbeFailure(ctx, account, now,
			mirasim.LimitsStatus(err), mirasimQuotaProbeFailureReason(err))
	}
	windows := mirasimQuotaWindowsFromLimits(limits)
	if len(windows) == 0 && (limits == nil || !limits.Suspended) {
		// A 200 that yielded no window is not a quota reading. It is what a
		// renamed field set decodes to (encoding/json ignores unknown names
		// silently), and recording it as OK would publish "this account has no
		// quota state" — which every downstream reader would take as "nothing is
		// exhausted".
		return s.persistQuotaProbeFailure(ctx, account, now, http.StatusOK, "empty_windows")
	}

	unknown := selectMirasimUnknownQuotaWindowNames(windows)
	reportMirasimQuotaWindowEvidence(account.ID, windows, unknown)

	observedAt := now
	snapshot := &MirasimQuotaProbeSnapshot{
		Status:         MirasimQuotaProbeStatusOK,
		Source:         MirasimQuotaSourceLimits,
		Suspended:      limits.Suspended,
		Windows:        windows,
		UnknownWindows: unknown,
		AppliedWindows: s.applyQuotaToScheduling(ctx, account, windows, now),
		ObservedAt:     &observedAt,
		LastAttemptAt:  now,
		NextProbeAt:    now.Add(mirasimQuotaNextProbeDelay(s.interval(), 0)),
		HTTPStatus:     http.StatusOK,
	}
	if err := s.writeSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// applyQuotaToScheduling writes the cooldowns this reading implies and returns
// the windows it actually wrote.
//
// Cooldowns are applied BEFORE the snapshot is persisted, deliberately: if the
// extra write then fails, the account is already protected and the next cycle
// simply re-probes. The other order would leave a recorded "exhausted" reading
// with nothing stopping the scheduler from using it.
func (s *MirasimQuotaProbeService) applyQuotaToScheduling(
	ctx context.Context,
	account *Account,
	windows []MirasimQuotaWindow,
	now time.Time,
) []string {
	limits := selectMirasimExhaustedWindowsFromQuota(windows, now)
	if len(limits) == 0 {
		return nil
	}
	if s.rateLimitService == nil {
		// Recorded, not silently swallowed: an operator seeing exhaustion in the
		// snapshot but no cooldown needs to know the writer was absent.
		slog.Warn("mirasim_quota_cooldown_not_applied",
			"account_id", account.ID,
			"windows", mirasimWindowLimitNames(limits),
			"detail", "quota probe has no rate limit service; exhaustion was recorded but not enforced")
		return nil
	}
	if !s.rateLimitService.persistMirasimWindowLimitSet(ctx, account, limits, now) {
		return nil
	}
	return mirasimWindowLimitNames(limits)
}

func mirasimWindowLimitNames(limits []mirasimWindowLimit) []string {
	names := make([]string, 0, len(limits))
	for _, limit := range limits {
		names = append(names, limit.window)
	}
	return names
}

// mirasimQuotaWindowsFromLimits converts the wire shape into the stored shape.
//
// TWO RULES, BOTH ABOUT NOT INVENTING DATA:
//   - a window the upstream did not send does not appear (no zero-filling);
//   - utilization is computed ONLY when budget > 0, so "budget unknown" can
//     never render as "0% used".
func mirasimQuotaWindowsFromLimits(limits *mirasim.LimitsInfo) []MirasimQuotaWindow {
	if limits == nil || len(limits.Windows) == 0 {
		return nil
	}
	windows := make([]MirasimQuotaWindow, 0, len(limits.Windows))
	for _, raw := range limits.Windows {
		name := strings.TrimSpace(raw.Name)
		if name == "" {
			// A nameless window cannot be attributed to any quota, so it cannot
			// gate anything and cannot be displayed. Dropping it is the only
			// honest option; keeping it would pad the snapshot with a row that
			// means nothing.
			continue
		}
		used := raw.Used
		budget := raw.Budget
		window := MirasimQuotaWindow{Name: name, Used: &used, Budget: &budget}
		if budget > 0 {
			utilization := used / budget
			window.Utilization = &utilization
		}
		window.ResetAt = mirasimQuotaResetAt(raw.ResetAt, name)
		windows = append(windows, window)
	}
	return windows
}

// mirasimQuotaResetPlausibleRange bounds what can be a reset time at all.
// Anything outside it is a unit or encoding error, not a quota window.
const mirasimQuotaResetPlausibleRange = 365 * 24 * time.Hour

// mirasimQuotaResetAt turns the upstream's reset_at into a time, or nil when it
// cannot be one.
//
// TWO DISTINCT HAZARDS, AND ONLY ONE OF THEM IS ABOUT SCHEDULING:
//
//  1. MILLISECONDS. The value arrives as a Unix second, but a 13-digit
//     millisecond timestamp read as seconds lands in the year ~57680. This
//     repository already normalises exactly that (parseAnthropicResetTimestamp:
//     `if ts > 1e11 { ts = ts / 1000 }`) for the anthropic reset HEADER, and
//     mirasim speaks the same protocol, so the same rule applies here rather
//     than a second, divergent one.
//
//  2. SERIALISABILITY. This is not a nicety: encoding/json REFUSES to marshal a
//     time.Time outside year [0,9999] ("year outside of range"). A single
//     out-of-range reset_at would therefore make the whole snapshot
//     unmarshalable, UpdateExtra would fail, and the probe would record NOTHING
//     — not even the failure — for an otherwise perfectly readable response,
//     re-probing the same account forever with an opaque error. nil is a state
//     every consumer already handles ("spent but no stated end"), so an
//     unusable value degrades to nil instead of poisoning the write.
//
// A reset time in the PAST is deliberately kept: it is a real, informative
// reading (the window has since cleared) and the exhaustion check already
// refuses to act on it.
func mirasimQuotaResetAt(raw int64, window string) *time.Time {
	if raw <= 0 {
		return nil
	}
	seconds := raw
	if seconds > 1e11 {
		seconds /= 1000
	}
	resetAt := time.Unix(seconds, 0).UTC()
	now := time.Now()
	if seconds > 1e11 ||
		resetAt.Before(now.Add(-mirasimQuotaResetPlausibleRange)) ||
		resetAt.After(now.Add(mirasimQuotaResetPlausibleRange)) {
		slog.Warn("mirasim_quota_window_reset_unusable",
			"window", window,
			"raw_reset_at", raw,
			"detail", "reset_at is not a plausible Unix timestamp even after millisecond normalisation; "+
				"recorded as absent so the rest of the reading still persists")
		return nil
	}
	return &resetAt
}

// persistQuotaProbeFailure records a failed attempt WITHOUT ever producing a
// healthy-looking reading.
//
// The last good reading is carried forward — an operator relying on it should
// not lose it because one probe timed out — but Status stays "failed" and
// ObservedAt keeps pointing at when those numbers were actually read, so every
// reader can tell how stale they are. Nothing here writes a cooldown: a failed
// probe measured nothing, and "no data" is not "exhausted".
func (s *MirasimQuotaProbeService) persistQuotaProbeFailure(
	ctx context.Context,
	account *Account,
	now time.Time,
	statusCode int,
	reason string,
) (*MirasimQuotaProbeSnapshot, error) {
	previous := DecodeMirasimQuotaProbeSnapshot(account.Extra)
	failureCount := 1
	if previous != nil {
		failureCount = previous.FailureCount + 1
	}
	snapshot := &MirasimQuotaProbeSnapshot{
		Status:        MirasimQuotaProbeStatusFailed,
		LastAttemptAt: now,
		NextProbeAt:   now.Add(mirasimQuotaNextProbeDelay(s.interval(), failureCount)),
		FailureCount:  failureCount,
		HTTPStatus:    statusCode,
		LastError:     reason,
	}
	if previous != nil {
		snapshot.Source = previous.Source
		snapshot.Suspended = previous.Suspended
		snapshot.Windows = previous.Windows
		snapshot.UnknownWindows = previous.UnknownWindows
		snapshot.AppliedWindows = previous.AppliedWindows
		snapshot.ObservedAt = previous.ObservedAt
	}
	if err := s.writeSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// writeSnapshot persists ONE key, for the same reason the plan probe does: the
// two probes share accounts.extra and neither may clobber the other's evidence.
func (s *MirasimQuotaProbeService) writeSnapshot(ctx context.Context, account *Account, snapshot *MirasimQuotaProbeSnapshot) error {
	if s.accountRepo == nil {
		return ErrMirasimQuotaProbeUnavailable
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{mirasim.ExtraQuotaProbe: snapshot}); err != nil {
		return err
	}
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	account.Extra[mirasim.ExtraQuotaProbe] = snapshot
	return nil
}

// persistCredentials writes a token rotation performed during a probe back to
// the account, so a refresh triggered here is not repeated by the next request.
func (s *MirasimQuotaProbeService) persistCredentials(account *Account) mirasim.Persister {
	return func(ctx context.Context, accountID int64, updates map[string]any) error {
		merged := make(map[string]any, len(account.Credentials)+len(updates))
		for key, value := range account.Credentials {
			merged[key] = value
		}
		for key, value := range updates {
			merged[key] = value
		}
		return persistAccountCredentials(ctx, s.accountRepo, account, merged)
	}
}

// persistExtra durably stores the account's stable session id the first time one
// is minted. Without it the whole pool rotates session ids in lockstep on every
// restart — a correlator no set of independently installed clients would produce.
func (s *MirasimQuotaProbeService) persistExtra(account *Account) mirasim.Persister {
	return func(ctx context.Context, accountID int64, updates map[string]any) error {
		if s.accountRepo == nil {
			return ErrMirasimQuotaProbeUnavailable
		}
		if err := s.accountRepo.UpdateExtra(ctx, accountID, updates); err != nil {
			return err
		}
		if account != nil && account.ID == accountID {
			if account.Extra == nil {
				account.Extra = map[string]any{}
			}
			for key, value := range updates {
				account.Extra[key] = value
			}
		}
		return nil
	}
}

func (s *MirasimQuotaProbeService) interval() int {
	if s == nil || s.intervalMin <= 0 {
		return mirasimQuotaProbeDefaultIntervalMinutes
	}
	return s.intervalMin
}

func (s *MirasimQuotaProbeService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *MirasimQuotaProbeService) tryAcquireLeaderLock(ctx context.Context, key string) (func(), bool, error) {
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s.lockCache != nil {
		acquired, err := s.lockCache.TryAcquireLeaderLock(lockCtx, key, s.instanceID, mirasimQuotaProbeLeaderLockTTL)
		if err != nil {
			return nil, false, err
		}
		if !acquired {
			return nil, false, nil
		}
		return func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer releaseCancel()
			_ = s.lockCache.ReleaseLeaderLock(releaseCtx, key, s.instanceID)
		}, true, nil
	}
	if s.db != nil {
		return tryAcquireDBAdvisoryLockWithError(lockCtx, s.db, hashAdvisoryLockID(key))
	}
	return func() {}, true, nil
}

// mirasimQuotaNextProbeDelay schedules the next attempt.
//
// IT HAS ITS OWN BACKOFF ON PURPOSE. The shared nextProbeDelay takes a
// Retry-After duration as its second argument, NOT a failure count — so the plan
// probe, which passes 0, retries a permanently broken account at full cadence
// forever, and it clamps against the BILLING probe's floor rather than its own.
// Copying that here would let one dead account consume a cycle slot every 20
// minutes indefinitely while a live account waits behind it.
//
// failureCount == 0 is the success path: the configured interval with jitter.
// Each consecutive failure doubles it, capped at mirasimQuotaProbeMaxDelay, so a
// recovered account is still rediscovered within hours.
func mirasimQuotaNextProbeDelay(intervalMinutes int, failureCount int) time.Duration {
	interval := time.Duration(intervalMinutes) * time.Minute
	if interval < mirasimQuotaProbeMinIntervalMinutes*time.Minute {
		interval = mirasimQuotaProbeMinIntervalMinutes * time.Minute
	}
	if failureCount > 0 {
		shift := failureCount - 1
		if shift > 8 {
			shift = 8
		}
		backoff := interval << uint(shift)
		if backoff <= 0 || backoff > mirasimQuotaProbeMaxDelay {
			backoff = mirasimQuotaProbeMaxDelay
		}
		interval = backoff
	}
	if interval > mirasimQuotaProbeMaxDelay {
		interval = mirasimQuotaProbeMaxDelay
	}
	// Jitter keeps a pool that was imported in one batch from probing in
	// lockstep forever after.
	jitterRange := interval / 5
	if jitterRange > 5*time.Minute {
		jitterRange = 5 * time.Minute
	}
	if jitterRange > 0 {
		interval += time.Duration(mathrand.Int64N(int64(jitterRange)*2+1)) - jitterRange
	}
	if interval < time.Minute {
		interval = time.Minute
	}
	return interval
}

// mirasimQuotaProbeDoer is the account-scoped egress for the quota probe.
//
//   - proxyURL is the account's proxy, so the request leaves from the IP the
//     upstream already associates with this device;
//   - the TLS profile is Mirasim.app's, so the handshake matches the data plane;
//   - the signing decorator is switched OFF because mirasim.FetchLimits signs the
//     request itself. Letting both sign would produce two signatures over
//     different header sets, and the second seal would encrypt the first one's
//     output — a request the relay cannot verify at all.
type mirasimQuotaProbeDoer struct {
	upstream    HTTPUpstream
	proxyURL    string
	accountID   int64
	concurrency int
}

func (d *mirasimQuotaProbeDoer) Do(req *http.Request) (*http.Response, error) {
	ctx := WithMirasimSigningDisabled(WithHTTPUpstreamRedirectsDisabled(req.Context()))
	*req = *req.WithContext(ctx)
	return d.upstream.DoWithTLS(req, d.proxyURL, d.accountID, d.concurrency, tlsfingerprint.MirasimProfile())
}

// mirasimQuotaProbeFailureReason maps an error to a stable, non-secret label.
// Never the error string: a transport error can quote a proxy URL, credentials
// included.
func mirasimQuotaProbeFailureReason(err error) string {
	if err == nil {
		return ""
	}
	if mirasim.IsLimitsDecodeFailure(err) {
		return "invalid_body"
	}
	if status := mirasim.LimitsStatus(err); status > 0 {
		switch {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return "credential_rejected"
		case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
			return "unsupported"
		case status == http.StatusTooManyRequests:
			return "rate_limited"
		case status >= 500:
			return "upstream_error"
		}
		return "http_error"
	}
	if mirasim.IsHardAuthRejection(err) {
		return "refresh_rejected"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return "request_failed"
}

func safeMirasimQuotaProbeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrMirasimQuotaProbeAccountInvalid) {
		return ErrMirasimQuotaProbeAccountInvalid.Error()
	}
	if errors.Is(err, ErrMirasimQuotaProbeUnavailable) {
		return ErrMirasimQuotaProbeUnavailable.Error()
	}
	if errors.Is(err, ErrMirasimQuotaProbeIdentityChanged) {
		return ErrMirasimQuotaProbeIdentityChanged.Error()
	}
	return "probe_failed"
}
