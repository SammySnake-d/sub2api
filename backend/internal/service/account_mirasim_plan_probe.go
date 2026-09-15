package service

// Mirasim subscription-plan probe.
//
// WHAT IT ANSWERS: "is this account plus or max, and when does the subscription
// end" — the two questions an operator needs in order to filter a 143-account
// mirasim pool. Neither is answerable from sub2api's own state: the plan label
// that arrives with an import is a claim, and accounts.expires_at is the access
// token's death, not the subscription's.
//
// WHERE THE TRUTH LIVES: GET <auth base>/auth/referral. Its current_plan is the
// tier the server actually grants right now. ma-relay, which has operated these
// same accounts, treats it as authoritative for exactly this reason.
//
// THE DISAGREEMENT IS THE PRODUCT: an account imported as "max" whose
// current_plan is "plus" met the invitation threshold but the upgrade has not
// settled. That is a fact an operator must act on, so this probe records BOTH
// values and marks the mismatch. It never writes over the claimed keys — the
// claimed fields (mirasim.ExtraPlanClaimed and friends) belong to the importer,
// the probe owns exactly one key (mirasim.ExtraPlanProbe), and
// ResolveMirasimPlan states the precedence in one place.
//
// SHAPE: modelled on UpstreamBillingProbeService — leader-locked periodic
// runner, bounded batch per cycle, per-account next_probe_at with jitter and
// exponential-ish backoff on failure, singleflight per account, plus manual
// single/batch entry points for the admin console.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	// A plan changes on the order of weeks (an upgrade settles, a subscription
	// lapses), so the default cadence is hours, not minutes. The floor is still
	// generous enough for an operator who is actively watching a pending upgrade.
	mirasimPlanProbeDefaultIntervalMinutes = 6 * 60
	mirasimPlanProbeMinIntervalMinutes     = 15
	mirasimPlanProbeMaxIntervalMinutes     = 24 * 60
	// The scan is a cheap platform listing plus an in-memory filter; five minutes
	// is fine-grained enough to honour a 15-minute floor without querying every
	// minute for a population that moves this slowly.
	mirasimPlanProbeCycleInterval = 5 * time.Minute
	mirasimPlanProbeMaxPerCycle   = 20
	mirasimPlanProbeConcurrency   = 4
	mirasimPlanProbeLeaderLockKey = "mirasim:plan:probe:leader"
	mirasimPlanProbeLeaderLockTTL = 2 * time.Minute
)

// MirasimPlanProbeMaxBatchSize limits one manual batch and one runner cycle.
const MirasimPlanProbeMaxBatchSize = mirasimPlanProbeMaxPerCycle

const (
	MirasimPlanProbeStatusOK     = "ok"
	MirasimPlanProbeStatusFailed = "failed"
)

// Where a resolved plan value came from. An operator filtering on "max" needs to
// know whether they are filtering on a verified tier or on an import-time label.
const (
	MirasimPlanSourceAuthoritative = "authoritative"
	MirasimPlanSourceClaimed       = "claimed"
	MirasimPlanSourceUnknown       = "unknown"
)

var (
	ErrMirasimPlanProbeUnavailable = infraerrors.ServiceUnavailable(
		"MIRASIM_PLAN_PROBE_UNAVAILABLE", "mirasim plan probe is unavailable",
	)
	ErrMirasimPlanProbeAccountInvalid = infraerrors.BadRequest(
		"MIRASIM_PLAN_PROBE_ACCOUNT_INVALID", "account is not a mirasim account",
	)
	ErrMirasimPlanProbeIdentityChanged = infraerrors.Conflict(
		"MIRASIM_PLAN_PROBE_IDENTITY_CHANGED", "account proxy changed during mirasim plan probe; retry the probe",
	)
)

// MirasimPlanProbeSettings controls the periodic runner.
type MirasimPlanProbeSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

// MirasimPlanSnapshot is the authoritative /auth/referral reading, persisted in
// accounts.extra under mirasim.ExtraPlanProbe.
//
// ClaimedPlan is copied in on every write. Storing it next to Plan is what makes
// Mismatch self-describing: a reader does not have to join two extra keys (which
// may have been edited between probes) to know what the probe compared.
type MirasimPlanSnapshot struct {
	Status string `json:"status"`
	// ClaimedPlan is what accounts.extra claimed at the moment of this probe.
	ClaimedPlan string `json:"claimed_plan,omitempty"`
	// Plan is the authoritative current_plan. Never merged into ClaimedPlan.
	Plan          string `json:"plan,omitempty"`
	NextPlan      string `json:"next_plan,omitempty"`
	PlanExpiresAt string `json:"plan_expires_at,omitempty"`
	Redeemed      int    `json:"redeemed,omitempty"`
	Threshold     int    `json:"threshold,omitempty"`
	Reached       bool   `json:"reached,omitempty"`
	// PendingUpgrade: invitations reached the threshold but the account is still
	// on the lower tier ("达标未结算").
	PendingUpgrade bool `json:"pending_upgrade,omitempty"`
	// Mismatch: the claim and the authoritative tier disagree. Note carries the
	// operator-readable form, worded as ma-relay words it.
	Mismatch bool   `json:"mismatch,omitempty"`
	Note     string `json:"note,omitempty"`

	ObservedAt    *time.Time `json:"observed_at,omitempty"`
	LastAttemptAt time.Time  `json:"last_attempt_at"`
	NextProbeAt   time.Time  `json:"next_probe_at"`
	FailureCount  int        `json:"failure_count,omitempty"`
	HTTPStatus    int        `json:"http_status,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// MirasimPlanProbeResult is returned by the manual probe endpoints.
type MirasimPlanProbeResult struct {
	AccountID int64                `json:"account_id"`
	Snapshot  *MirasimPlanSnapshot `json:"snapshot,omitempty"`
	Error     string               `json:"error,omitempty"`
}

// MirasimPlanItem is the admin-console projection of one account's plan state:
// the claim, the authoritative reading, and the resolved value to filter on.
type MirasimPlanItem struct {
	AccountID     int64  `json:"account_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	ProbeEnabled  bool   `json:"plan_probe_enabled"`
	ClaimedPlan   string `json:"claimed_plan,omitempty"`
	Plan          string `json:"plan,omitempty"`
	PlanSource    string `json:"plan_source"`
	PlanExpiresAt string `json:"plan_expires_at,omitempty"`
	// PlanExpiresSource mirrors PlanSource for the expiry, which can come from a
	// probe even when the plan label itself never disagreed.
	PlanExpiresSource string               `json:"plan_expires_source"`
	NextPlan          string               `json:"next_plan,omitempty"`
	Redeemed          int                  `json:"redeemed,omitempty"`
	Threshold         int                  `json:"threshold,omitempty"`
	PendingUpgrade    bool                 `json:"pending_upgrade,omitempty"`
	PlanMismatch      bool                 `json:"plan_mismatch,omitempty"`
	PlanNote          string               `json:"plan_note,omitempty"`
	Probe             *MirasimPlanSnapshot `json:"probe,omitempty"`
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func defaultMirasimPlanProbeSettings() *MirasimPlanProbeSettings {
	return &MirasimPlanProbeSettings{Enabled: true, IntervalMinutes: mirasimPlanProbeDefaultIntervalMinutes}
}

func normalizeMirasimPlanProbeSettings(settings *MirasimPlanProbeSettings) {
	if settings.IntervalMinutes < mirasimPlanProbeMinIntervalMinutes {
		settings.IntervalMinutes = mirasimPlanProbeMinIntervalMinutes
	}
	if settings.IntervalMinutes > mirasimPlanProbeMaxIntervalMinutes {
		settings.IntervalMinutes = mirasimPlanProbeMaxIntervalMinutes
	}
}

// GetMirasimPlanProbeSettings returns defaults when the setting is absent.
func (s *SettingService) GetMirasimPlanProbeSettings(ctx context.Context) (*MirasimPlanProbeSettings, error) {
	defaults := defaultMirasimPlanProbeSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyMirasimPlanProbeSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, fmt.Errorf("get mirasim plan probe settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return defaults, nil
	}
	settings := *defaults
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return nil, fmt.Errorf("parse mirasim plan probe settings: %w", err)
	}
	if settings.IntervalMinutes == 0 {
		settings.IntervalMinutes = defaults.IntervalMinutes
	}
	normalizeMirasimPlanProbeSettings(&settings)
	return &settings, nil
}

// SetMirasimPlanProbeSettings validates and persists the runner settings.
func (s *SettingService) SetMirasimPlanProbeSettings(ctx context.Context, settings *MirasimPlanProbeSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository is unavailable")
	}
	if settings == nil {
		return infraerrors.BadRequest("INVALID_MIRASIM_PLAN_PROBE_SETTINGS", "settings cannot be nil")
	}
	if settings.IntervalMinutes < mirasimPlanProbeMinIntervalMinutes || settings.IntervalMinutes > mirasimPlanProbeMaxIntervalMinutes {
		return infraerrors.BadRequest(
			"INVALID_MIRASIM_PLAN_PROBE_INTERVAL",
			fmt.Sprintf("interval_minutes must be between %d and %d", mirasimPlanProbeMinIntervalMinutes, mirasimPlanProbeMaxIntervalMinutes),
		)
	}
	normalizeMirasimPlanProbeSettings(settings)
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal mirasim plan probe settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyMirasimPlanProbeSettings, string(data))
}

// ---------------------------------------------------------------------------
// Reading what is stored
// ---------------------------------------------------------------------------

// DecodeMirasimPlanSnapshot reads the authoritative snapshot out of extra.
// Returns nil for absent or malformed data rather than a zero snapshot, so a
// caller can never mistake "never probed" for "probed and found nothing".
func DecodeMirasimPlanSnapshot(extra map[string]any) *MirasimPlanSnapshot {
	if extra == nil {
		return nil
	}
	value, ok := extra[mirasim.ExtraPlanProbe]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var snapshot MirasimPlanSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Status == "" {
		return nil
	}
	if snapshot.Status != MirasimPlanProbeStatusOK && snapshot.Status != MirasimPlanProbeStatusFailed {
		return nil
	}
	return &snapshot
}

// MirasimClaimedPlan reads the import-time plan label.
func MirasimClaimedPlan(account *Account) string {
	if account == nil || account.Extra == nil {
		return ""
	}
	value, _ := account.Extra[mirasim.ExtraPlanClaimed].(string)
	return strings.ToLower(strings.TrimSpace(value))
}

func mirasimExtraString(account *Account, key string) string {
	if account == nil || account.Extra == nil {
		return ""
	}
	value, _ := account.Extra[key].(string)
	return strings.TrimSpace(value)
}

// mirasimExtraInt tolerates every numeric shape JSONB round-trips through
// (float64 from encoding/json, json.Number, int64 from a direct write).
func mirasimExtraInt(account *Account, key string) int {
	if account == nil || account.Extra == nil {
		return 0
	}
	switch typed := account.Extra[key].(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return parsed
		}
	}
	return 0
}

// MirasimPlanProbeEnabled is an opt-OUT: absent means enabled. Discovering the
// tier is why a mirasim account exists in this system, so the useful default is
// on; an operator switches off the individual accounts they do not want probed.
func MirasimPlanProbeEnabled(account *Account) bool {
	if account == nil || account.Extra == nil {
		return true
	}
	enabled, ok := account.Extra[mirasim.ExtraPlanProbeEnabled].(bool)
	if !ok {
		return true
	}
	return enabled
}

// ResolveMirasimPlan is the SINGLE place claimed-vs-authoritative precedence is
// decided. Everything else reads the two sources and hands them here.
//
// Precedence: a successful probe wins, because it is the only verified reading.
// The claim survives untouched in ClaimedPlan and, when the two disagree, the
// item is marked and annotated instead of one value quietly replacing the other.
func ResolveMirasimPlan(account *Account) MirasimPlanItem {
	item := MirasimPlanItem{
		PlanSource:        MirasimPlanSourceUnknown,
		PlanExpiresSource: MirasimPlanSourceUnknown,
	}
	if account == nil {
		return item
	}
	item.AccountID = account.ID
	item.Name = account.Name
	item.Status = account.Status
	item.ProbeEnabled = MirasimPlanProbeEnabled(account)

	item.ClaimedPlan = MirasimClaimedPlan(account)
	if item.ClaimedPlan != "" {
		item.Plan = item.ClaimedPlan
		item.PlanSource = MirasimPlanSourceClaimed
	}
	if claimedExpires := mirasimExtraString(account, mirasim.ExtraPlanExpiresAt); claimedExpires != "" {
		item.PlanExpiresAt = claimedExpires
		item.PlanExpiresSource = MirasimPlanSourceClaimed
	}
	item.NextPlan = strings.ToLower(mirasimExtraString(account, mirasim.ExtraPlanNext))
	item.Redeemed = mirasimExtraInt(account, mirasim.ExtraPlanRedeemed)
	item.Threshold = mirasimExtraInt(account, mirasim.ExtraPlanThreshold)

	snapshot := DecodeMirasimPlanSnapshot(account.Extra)
	if snapshot == nil {
		return item
	}
	item.Probe = snapshot
	if snapshot.Status != MirasimPlanProbeStatusOK {
		// A failed probe still carries the last good reading it inherited, but it
		// must not promote anything: leave the claim in place and let PlanNote
		// (below) stay empty rather than asserting a stale mismatch.
		return item
	}
	if snapshot.Plan != "" {
		item.Plan = snapshot.Plan
		item.PlanSource = MirasimPlanSourceAuthoritative
	}
	if snapshot.PlanExpiresAt != "" {
		item.PlanExpiresAt = snapshot.PlanExpiresAt
		item.PlanExpiresSource = MirasimPlanSourceAuthoritative
	}
	if snapshot.NextPlan != "" {
		item.NextPlan = snapshot.NextPlan
	}
	if snapshot.Redeemed != 0 {
		item.Redeemed = snapshot.Redeemed
	}
	if snapshot.Threshold != 0 {
		item.Threshold = snapshot.Threshold
	}
	item.PendingUpgrade = snapshot.PendingUpgrade
	item.PlanMismatch = snapshot.Mismatch
	item.PlanNote = snapshot.Note
	return item
}

// BuildMirasimPlanItems projects accounts into the console payload, keeping only
// mirasim accounts. plan/mismatch filters are applied after resolution, so a
// filter on "max" means the resolved tier, not whichever source happened to
// mention max.
func BuildMirasimPlanItems(accounts []Account, planFilter string, mismatchOnly bool) []MirasimPlanItem {
	planFilter = strings.ToLower(strings.TrimSpace(planFilter))
	items := make([]MirasimPlanItem, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		if !IsMirasimAccount(&account) {
			continue
		}
		item := ResolveMirasimPlan(&account)
		if planFilter != "" && item.Plan != planFilter {
			continue
		}
		if mismatchOnly && !item.PlanMismatch {
			continue
		}
		items = append(items, item)
	}
	return items
}

// ---------------------------------------------------------------------------
// The probe service
// ---------------------------------------------------------------------------

// MirasimPlanProbeService periodically reads each mirasim account's
// authoritative plan.
type MirasimPlanProbeService struct {
	accountRepo    AccountRepository
	httpUpstream   HTTPUpstream
	settingService *SettingService

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
}

func NewMirasimPlanProbeService(
	accountRepo AccountRepository,
	httpUpstream HTTPUpstream,
	settingService *SettingService,
) *MirasimPlanProbeService {
	ctx, cancel := context.WithCancel(context.Background())
	return &MirasimPlanProbeService{
		accountRepo:    accountRepo,
		httpUpstream:   httpUpstream,
		settingService: settingService,
		// A registry of its own, not the signing decorator's. The two converge
		// through the database: each syncs the persisted identity before use and
		// only adopts a token strictly newer than the one it holds (see
		// mirasim.Credential.syncLocked), which is the same reconciliation that
		// already covers a second sub2api process refreshing a token.
		registry:     mirasim.NewRegistry(),
		parentCtx:    ctx,
		parentCancel: cancel,
		probeSlots:   make(chan struct{}, mirasimPlanProbeConcurrency),
		now:          time.Now,
		instanceID:   uuid.NewString(),
	}
}

func (s *MirasimPlanProbeService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// ProvideMirasimPlanProbeService starts the process-wide periodic runner.
func ProvideMirasimPlanProbeService(
	accountRepo AccountRepository,
	httpUpstream HTTPUpstream,
	settingService *SettingService,
	lockCache LeaderLockCache,
	db *sql.DB,
) *MirasimPlanProbeService {
	svc := NewMirasimPlanProbeService(accountRepo, httpUpstream, settingService)
	svc.SetLeaderLock(lockCache, db)
	svc.Start()
	return svc
}

func (s *MirasimPlanProbeService) Start() {
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

func (s *MirasimPlanProbeService) Stop() {
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

func (s *MirasimPlanProbeService) runLoop() {
	defer s.wg.Done()
	_ = s.RunDue(s.parentCtx)
	ticker := time.NewTicker(mirasimPlanProbeCycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			if err := s.RunDue(s.parentCtx); err != nil {
				logger.LegacyPrintf("service.mirasim_plan_probe", "run_due_failed: err=%v", err)
			}
		}
	}
}

// RunDue executes at most one bounded batch of due accounts.
func (s *MirasimPlanProbeService) RunDue(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	settings, err := s.getSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	release, acquired, lockErr := s.tryAcquireLeaderLock(ctx, mirasimPlanProbeLeaderLockKey)
	if lockErr != nil {
		return fmt.Errorf("acquire mirasim plan probe leader lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer release()

	now := s.currentTime()
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformAnthropic)
	if err != nil {
		return fmt.Errorf("list anthropic accounts for mirasim plan probe: %w", err)
	}
	due := make([]Account, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		if !IsMirasimAccount(&account) || !account.IsActive() || !MirasimPlanProbeEnabled(&account) {
			continue
		}
		snapshot := DecodeMirasimPlanSnapshot(account.Extra)
		if snapshot != nil && !snapshot.NextProbeAt.IsZero() && now.Before(snapshot.NextProbeAt) {
			continue
		}
		due = append(due, account)
	}
	sort.SliceStable(due, func(i, j int) bool {
		left := DecodeMirasimPlanSnapshot(due[i].Extra)
		right := DecodeMirasimPlanSnapshot(due[j].Extra)
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
	if len(due) > mirasimPlanProbeMaxPerCycle {
		due = due[:mirasimPlanProbeMaxPerCycle]
	}

	var group errgroup.Group
	for i := range due {
		accountID := due[i].ID
		group.Go(func() error {
			if _, probeErr := s.probeAccountWithMode(ctx, accountID, settings.IntervalMinutes, true); probeErr != nil {
				logger.LegacyPrintf("service.mirasim_plan_probe", "probe_due_failed: account_id=%d err=%v", accountID, probeErr)
			}
			return nil
		})
	}
	return group.Wait()
}

func (s *MirasimPlanProbeService) getSettings(ctx context.Context) (*MirasimPlanProbeSettings, error) {
	if s.settingService == nil {
		return defaultMirasimPlanProbeSettings(), nil
	}
	return s.settingService.GetMirasimPlanProbeSettings(ctx)
}

func (s *MirasimPlanProbeService) GetSettings(ctx context.Context) (*MirasimPlanProbeSettings, error) {
	if s == nil {
		return nil, ErrMirasimPlanProbeUnavailable
	}
	return s.getSettings(ctx)
}

func (s *MirasimPlanProbeService) UpdateSettings(ctx context.Context, settings *MirasimPlanProbeSettings) error {
	if s == nil || s.settingService == nil {
		return ErrMirasimPlanProbeUnavailable
	}
	return s.settingService.SetMirasimPlanProbeSettings(ctx, settings)
}

// SetAccountEnabled flips the per-account opt-out.
func (s *MirasimPlanProbeService) SetAccountEnabled(ctx context.Context, accountID int64, enabled bool) error {
	if s == nil || s.accountRepo == nil {
		return ErrMirasimPlanProbeUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if !IsMirasimAccount(account) {
		return ErrMirasimPlanProbeAccountInvalid
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{mirasim.ExtraPlanProbeEnabled: enabled})
}

// ProbeAccount performs one manual probe, ignoring both switches.
func (s *MirasimPlanProbeService) ProbeAccount(ctx context.Context, accountID int64) (*MirasimPlanSnapshot, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrMirasimPlanProbeUnavailable
	}
	settings, err := s.getSettings(ctx)
	if err != nil {
		return nil, err
	}
	return s.probeAccountWithMode(ctx, accountID, settings.IntervalMinutes, false)
}

// ProbeAccounts performs a bounded manual batch with the runner's concurrency.
func (s *MirasimPlanProbeService) ProbeAccounts(ctx context.Context, accountIDs []int64) []MirasimPlanProbeResult {
	if len(accountIDs) > mirasimPlanProbeMaxPerCycle {
		accountIDs = accountIDs[:mirasimPlanProbeMaxPerCycle]
	}
	results := make([]MirasimPlanProbeResult, len(accountIDs))
	if s == nil || s.accountRepo == nil {
		for i, accountID := range accountIDs {
			results[i] = MirasimPlanProbeResult{AccountID: accountID, Error: ErrMirasimPlanProbeUnavailable.Error()}
		}
		return results
	}
	settings, settingsErr := s.getSettings(ctx)
	if settingsErr != nil {
		for i, accountID := range accountIDs {
			results[i] = MirasimPlanProbeResult{AccountID: accountID, Error: safeMirasimPlanProbeError(settingsErr)}
		}
		return results
	}
	var group errgroup.Group
	for i, accountID := range accountIDs {
		i, accountID := i, accountID
		results[i].AccountID = accountID
		group.Go(func() error {
			snapshot, err := s.probeAccountWithMode(ctx, accountID, settings.IntervalMinutes, false)
			if err != nil {
				results[i].Error = safeMirasimPlanProbeError(err)
				return nil
			}
			results[i].Snapshot = snapshot
			return nil
		})
	}
	_ = group.Wait()
	return results
}

func (s *MirasimPlanProbeService) probeAccountWithMode(ctx context.Context, accountID int64, intervalMinutes int, requireEnabled bool) (*MirasimPlanSnapshot, error) {
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
			return nil, ErrMirasimPlanProbeAccountInvalid
		}
		if requireEnabled {
			if !account.IsActive() || !MirasimPlanProbeEnabled(account) {
				return nil, nil
			}
			if snapshot := DecodeMirasimPlanSnapshot(account.Extra); snapshot != nil &&
				!snapshot.NextProbeAt.IsZero() && s.currentTime().Before(snapshot.NextProbeAt) {
				return nil, nil
			}
		}
		return s.probeLoadedAccount(ctx, account, intervalMinutes)
	})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	snapshot, ok := value.(*MirasimPlanSnapshot)
	if !ok {
		return nil, fmt.Errorf("invalid mirasim plan probe result")
	}
	return snapshot, nil
}

func (s *MirasimPlanProbeService) probeLoadedAccount(ctx context.Context, account *Account, intervalMinutes int) (*MirasimPlanSnapshot, error) {
	now := s.currentTime().UTC()
	if s.httpUpstream == nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "transport_unavailable")
	}

	// The probe MUST leave through the account's own proxy. A mirasim account is
	// pinned to one egress IP (sticky residential proxy) and the upstream binds
	// the device to it; a probe that went out direct would show the account
	// operating from two IPs at once — the single most legible way to burn it.
	proxyURL := ""
	if account.ProxyID != nil {
		if account.Proxy == nil {
			return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "proxy_unavailable")
		}
		if account.Proxy.ID != *account.ProxyID {
			return nil, ErrMirasimPlanProbeIdentityChanged
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
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "missing_device_seed")
	}

	doer := &mirasimPlanProbeDoer{
		upstream:    s.httpUpstream,
		proxyURL:    proxyURL,
		accountID:   account.ID,
		concurrency: account.Concurrency,
	}
	referral, err := s.registry.FetchReferral(ctx, account.ID, identity, doer, s.persistCredentials(account))
	if err != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, mirasim.ReferralStatus(err), mirasimPlanProbeFailureReason(err))
	}
	if referral == nil || strings.TrimSpace(referral.CurrentPlan) == "" {
		// A 200 with no current_plan is not a plan reading. Recording it as OK
		// would let an empty authoritative value outrank a real claim.
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, http.StatusOK, "empty_current_plan")
	}

	claimed := MirasimClaimedPlan(account)
	authoritative := strings.ToLower(strings.TrimSpace(referral.CurrentPlan))
	note := mirasim.PlanDiffNote(claimed, referral)
	observedAt := now
	snapshot := &MirasimPlanSnapshot{
		Status:         MirasimPlanProbeStatusOK,
		ClaimedPlan:    claimed,
		Plan:           authoritative,
		NextPlan:       strings.ToLower(strings.TrimSpace(referral.NextPlan)),
		PlanExpiresAt:  strings.TrimSpace(referral.PlanExpiresAt),
		Redeemed:       referral.Redeemed,
		Threshold:      referral.Threshold,
		Reached:        referral.Reached,
		PendingUpgrade: referral.PendingUpgrade(),
		Mismatch:       claimed != "" && claimed != authoritative,
		Note:           note,
		ObservedAt:     &observedAt,
		LastAttemptAt:  now,
		NextProbeAt:    now.Add(nextProbeDelay(intervalMinutes, 0)),
		HTTPStatus:     http.StatusOK,
	}
	if err := s.writeSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *MirasimPlanProbeService) persistProbeFailure(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	statusCode int,
	reason string,
) (*MirasimPlanSnapshot, error) {
	previous := DecodeMirasimPlanSnapshot(account.Extra)
	failureCount := 1
	if previous != nil {
		failureCount = previous.FailureCount + 1
	}
	snapshot := &MirasimPlanSnapshot{
		Status:        MirasimPlanProbeStatusFailed,
		LastAttemptAt: now,
		NextProbeAt:   now.Add(nextProbeDelay(intervalMinutes, 0)),
		FailureCount:  failureCount,
		HTTPStatus:    statusCode,
		LastError:     reason,
	}
	if previous != nil {
		// Carry the last good reading forward. A failed probe must not erase a
		// plan an operator was relying on; ResolveMirasimPlan refuses to promote
		// it precisely because the status says failed.
		snapshot.ClaimedPlan = previous.ClaimedPlan
		snapshot.Plan = previous.Plan
		snapshot.NextPlan = previous.NextPlan
		snapshot.PlanExpiresAt = previous.PlanExpiresAt
		snapshot.Redeemed = previous.Redeemed
		snapshot.Threshold = previous.Threshold
		snapshot.Reached = previous.Reached
		snapshot.PendingUpgrade = previous.PendingUpgrade
		snapshot.Mismatch = previous.Mismatch
		snapshot.Note = previous.Note
		snapshot.ObservedAt = previous.ObservedAt
	}
	if err := s.writeSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// writeSnapshot persists ONE key. The claimed fields are the importer's, and a
// probe that overwrote them would destroy the evidence that the two disagree.
func (s *MirasimPlanProbeService) writeSnapshot(ctx context.Context, account *Account, snapshot *MirasimPlanSnapshot) error {
	if s.accountRepo == nil {
		return ErrMirasimPlanProbeUnavailable
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{mirasim.ExtraPlanProbe: snapshot}); err != nil {
		return err
	}
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	account.Extra[mirasim.ExtraPlanProbe] = snapshot
	return nil
}

// persistCredentials writes a token rotation performed during a probe back to
// the account, so a refresh triggered here is not repeated by the next request.
func (s *MirasimPlanProbeService) persistCredentials(account *Account) mirasim.Persister {
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

func (s *MirasimPlanProbeService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *MirasimPlanProbeService) tryAcquireLeaderLock(ctx context.Context, key string) (func(), bool, error) {
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s.lockCache != nil {
		acquired, err := s.lockCache.TryAcquireLeaderLock(lockCtx, key, s.instanceID, mirasimPlanProbeLeaderLockTTL)
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

// mirasimPlanProbeDoer is the account-scoped egress for the probe's control
// plane calls (the referral read, and the token refresh it may trigger).
//
// Three things make it "this account's own egress":
//   - proxyURL is the account's proxy, so the request leaves from the IP the
//     upstream already associates with this device;
//   - the TLS profile is Mirasim.app's, so the handshake matches the data plane;
//   - signing is explicitly disabled, because the auth server authenticates the
//     plain access-token bearer (see mirasim.Registry.FetchReferral) and the
//     signing decorator would replace it with a relay device ticket.
type mirasimPlanProbeDoer struct {
	upstream    HTTPUpstream
	proxyURL    string
	accountID   int64
	concurrency int
}

func (d *mirasimPlanProbeDoer) Do(req *http.Request) (*http.Response, error) {
	ctx := WithMirasimSigningDisabled(WithHTTPUpstreamRedirectsDisabled(req.Context()))
	*req = *req.WithContext(ctx)
	return d.upstream.DoWithTLS(req, d.proxyURL, d.accountID, d.concurrency, tlsfingerprint.MirasimProfile())
}

// mirasimPlanProbeFailureReason maps an error to a stable, non-secret label.
// Never the error string: a transport error can quote a proxy URL, credentials
// included.
func mirasimPlanProbeFailureReason(err error) string {
	if err == nil {
		return ""
	}
	if status := mirasim.ReferralStatus(err); status > 0 {
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

func safeMirasimPlanProbeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrMirasimPlanProbeAccountInvalid) {
		return ErrMirasimPlanProbeAccountInvalid.Error()
	}
	if errors.Is(err, ErrMirasimPlanProbeUnavailable) {
		return ErrMirasimPlanProbeUnavailable.Error()
	}
	if errors.Is(err, ErrMirasimPlanProbeIdentityChanged) {
		return ErrMirasimPlanProbeIdentityChanged.Error()
	}
	return "probe_failed"
}
