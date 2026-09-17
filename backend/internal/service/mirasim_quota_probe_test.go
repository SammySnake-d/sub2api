//go:build unit

package service

// Tests for the mirasim quota probe and its scheduling linkage.
//
// EVIDENCE GRADE OF THE FIXTURES: the /v1/limits response bodies are modelled on
// ma-relay internal/relay/quota.go probeQuotaOne, which reads this endpoint in
// production for these same accounts. The window NAMES 7d_claude and 7d_fable
// are this repository's unverified expectations (mirasimWindowTokenEvidence);
// what these tests prove is that the pipeline handles the documented shape —
// including an absent window and an unrecognised name — not that those names are
// what the relay emits.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// mirasimQuotaTestSeed is an obviously-fake but structurally valid 32-byte
// ed25519 device seed. No real credential appears in this file.
const mirasimQuotaTestSeed = "dGVzdC1taXJhc2ltLXF1b3RhLXNlZWQtMDAwMDAwMDA"

const (
	mirasimQuotaTestOpusModel  = "claude-opus-4-6"
	mirasimQuotaTestFableModel = "claude-fable-5"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type mirasimQuotaCall struct {
	method           string
	url              string
	proxyURL         string
	accountID        int64
	authorization    string
	acceptEncoding   string
	tlsProfile       string
	signingDisabled  bool
	redirectDisabled bool
	plaintextMirasim []string
}

// mirasimQuotaUpstream is a fake HTTPUpstream that answers the device-session
// mint and /v1/limits, recording how each request was dispatched — which is what
// makes the egress and signing assertions possible.
type mirasimQuotaUpstream struct {
	mu           sync.Mutex
	calls        []mirasimQuotaCall
	limitsStatus int
	limitsBody   string
}

func (u *mirasimQuotaUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (u *mirasimQuotaUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	call := mirasimQuotaCall{
		method:           req.Method,
		url:              req.URL.String(),
		proxyURL:         proxyURL,
		accountID:        accountID,
		authorization:    req.Header.Get("authorization"),
		acceptEncoding:   req.Header.Get("accept-encoding"),
		signingDisabled:  MirasimSigningDisabled(req.Context()),
		redirectDisabled: HTTPUpstreamRedirectsDisabled(req.Context()),
	}
	if profile != nil {
		call.tlsProfile = profile.Name
	}
	for name := range req.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-mirasim-") && lower != "x-mirasim-client" && lower != "x-mirasim-enc" {
			call.plaintextMirasim = append(call.plaintextMirasim, lower)
		}
	}
	u.mu.Lock()
	u.calls = append(u.calls, call)
	u.mu.Unlock()

	switch req.URL.Path {
	case "/v1/device/session":
		return mirasimQuotaResponse(http.StatusOK, `{"ticket":"tkt_quota","expiresIn":600}`), nil
	case mirasim.LimitsPath:
		status := u.limitsStatus
		if status == 0 {
			status = http.StatusOK
		}
		return mirasimQuotaResponse(status, u.limitsBody), nil
	}
	return mirasimQuotaResponse(http.StatusNotFound, `{}`), nil
}

func mirasimQuotaResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func (u *mirasimQuotaUpstream) limitsCall(t *testing.T) mirasimQuotaCall {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.calls) - 1; i >= 0; i-- {
		if strings.Contains(u.calls[i].url, mirasim.LimitsPath) {
			return u.calls[i]
		}
	}
	t.Fatalf("the probe issued no %s request at all (calls=%+v)", mirasim.LimitsPath, u.calls)
	return mirasimQuotaCall{}
}

// mirasimQuotaRepo is the narrow slice of AccountRepository the probe and the
// cooldown writer use. The interface is embedded so an unexpected call panics
// loudly instead of silently returning a zero value.
type mirasimQuotaRepo struct {
	AccountRepository
	mu              sync.Mutex
	account         *Account
	rateLimitCalls  []time.Time
	modelLimitCalls []mirasimQuotaModelLimitCall
}

type mirasimQuotaModelLimitCall struct {
	scope   string
	resetAt time.Time
	reason  string
}

func (r *mirasimQuotaRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account == nil || r.account.ID != id {
		return nil, ErrAccountNotFound
	}
	return r.account, nil
}

func (r *mirasimQuotaRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func (r *mirasimQuotaRepo) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rateLimitCalls = append(r.rateLimitCalls, resetAt)
	if r.account != nil && r.account.ID == id {
		reset := resetAt
		r.account.RateLimitResetAt = &reset
	}
	return nil
}

func (r *mirasimQuotaRepo) SetModelRateLimit(ctx context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := mirasimQuotaModelLimitCall{scope: scope, resetAt: resetAt}
	if len(reason) > 0 {
		call.reason = reason[0]
	}
	r.modelLimitCalls = append(r.modelLimitCalls, call)
	return nil
}

func (r *mirasimQuotaRepo) cooldownWriteCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rateLimitCalls) + len(r.modelLimitCalls)
}

func newMirasimQuotaAccount() *Account {
	proxyID := int64(7)
	return &Account{
		ID:          4242,
		Name:        "mirasim-quota",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 3,
		Credentials: map[string]any{
			mirasim.CredProvider:    mirasim.ProviderMirasim,
			mirasim.CredDeviceSeed:  mirasimQuotaTestSeed,
			mirasim.CredAccessToken: "test-access-token-quota",
			mirasim.CredAuthBase:    "https://auth.mirasim.test",
			"base_url":              "https://relay.mirasim.test",
			"api_key":               "mirasim-signed",
		},
		Extra:   map[string]any{"anthropic_passthrough": true},
		ProxyID: &proxyID,
		Proxy: &Proxy{
			ID:       7,
			Name:     "mirasim-egress-7",
			Protocol: "socks5h",
			Host:     "proxy.example.test",
			Port:     1080,
			Username: "mirasim7",
			Password: "test-proxy-secret",
			Status:   StatusActive,
		},
	}
}

// newMirasimQuotaProbe wires a probe whose cooldown writer is the real
// RateLimitService, so the tests exercise the production write path rather than
// a stand-in.
func newMirasimQuotaProbe(account *Account, upstream *mirasimQuotaUpstream) (*MirasimQuotaProbeService, *mirasimQuotaRepo) {
	repo := &mirasimQuotaRepo{account: account}
	rateLimit := &RateLimitService{accountRepo: repo}
	return NewMirasimQuotaProbeService(repo, upstream, rateLimit, nil), repo
}

// mirasimLimitsBody renders a /v1/limits response in ma-relay's shape.
func mirasimLimitsBody(t *testing.T, suspended bool, windows ...mirasim.LimitsWindow) string {
	t.Helper()
	raw, err := json.Marshal(mirasim.LimitsInfo{Suspended: suspended, Windows: windows})
	require.NoError(t, err)
	return string(raw)
}

// storedMirasimQuotaSnapshot reads back what the probe persisted, through the
// same decoder production uses and after a JSON round-trip: accounts.extra is
// JSONB, so what comes back from the database is never the Go struct that went
// in.
func storedMirasimQuotaSnapshot(t *testing.T, account *Account) *MirasimQuotaProbeSnapshot {
	t.Helper()
	raw, err := json.Marshal(account.Extra)
	require.NoError(t, err)
	var reloaded map[string]any
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	snapshot := DecodeMirasimQuotaProbeSnapshot(reloaded)
	require.NotNilf(t, snapshot, "no decodable snapshot under extra[%s]; keys = %v", mirasim.ExtraQuotaProbe, reloaded)
	return snapshot
}

func mirasimQuotaWindowNames(windows []MirasimQuotaWindow) []string {
	names := make([]string, 0, len(windows))
	for _, window := range windows {
		names = append(names, window.Name)
	}
	return names
}

func mirasimQuotaWindowByName(t *testing.T, windows []MirasimQuotaWindow, name string) MirasimQuotaWindow {
	t.Helper()
	for _, window := range windows {
		if window.Name == name {
			return window
		}
	}
	t.Fatalf("window %q is absent; got %v", name, mirasimQuotaWindowNames(windows))
	return MirasimQuotaWindow{}
}

// ---------------------------------------------------------------------------
// The reading itself
// ---------------------------------------------------------------------------

// TestMirasimQuotaProbeRecordsTheWindowsUpstreamReturned is the positive case:
// a four-window response becomes a four-window snapshot with absolute counters,
// derived utilization and reset times — and the request left through the
// account's own egress, signed by the package rather than by the decorator.
func TestMirasimQuotaProbeRecordsTheWindowsUpstreamReturned(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	account := newMirasimQuotaAccount()
	upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
		mirasim.LimitsWindow{Name: MirasimWindow5h, Used: 100, Budget: 400, ResetAt: reset.Unix()},
		mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 5000, Budget: 20000, ResetAt: reset.Unix()},
		mirasim.LimitsWindow{Name: MirasimWindow7dClaude, Used: 3000, Budget: 12000, ResetAt: reset.Unix()},
		mirasim.LimitsWindow{Name: MirasimWindow7dFable, Used: 10, Budget: 900, ResetAt: reset.Unix()},
	)}
	probe, _ := newMirasimQuotaProbe(account, upstream)

	snapshot, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.Equal(t, MirasimQuotaProbeStatusOK, snapshot.Status)
	require.Equal(t, MirasimQuotaSourceLimits, snapshot.Source)
	require.NotNil(t, snapshot.ObservedAt)

	stored := storedMirasimQuotaSnapshot(t, account)
	require.Equal(t,
		[]string{MirasimWindow5h, MirasimWindow7d, MirasimWindow7dClaude, MirasimWindow7dFable},
		mirasimQuotaWindowNames(stored.Windows))

	claude := mirasimQuotaWindowByName(t, stored.Windows, MirasimWindow7dClaude)
	require.NotNil(t, claude.Used)
	require.NotNil(t, claude.Budget)
	require.Equal(t, 3000.0, *claude.Used)
	require.Equal(t, 12000.0, *claude.Budget)
	require.NotNil(t, claude.Utilization, "utilization must be derived when budget > 0")
	require.InDelta(t, 0.25, *claude.Utilization, 1e-9)
	require.NotNil(t, claude.ResetAt)
	require.Equal(t, reset.UTC(), claude.ResetAt.UTC())

	// Nothing is exhausted, so nothing was cooled down.
	require.Empty(t, stored.AppliedWindows)
	require.Empty(t, stored.UnknownWindows)
	require.True(t, account.IsSchedulableForModelWithContext(context.Background(), mirasimQuotaTestOpusModel))

	// The request must look exactly like the real client's usage probe, sent
	// from this account's own egress.
	call := upstream.limitsCall(t)
	require.Equal(t, http.MethodGet, call.method)
	require.True(t, strings.HasPrefix(call.url, account.GetCredential("base_url")),
		"probe hit %s, want the account's own relay base %s", call.url, account.GetCredential("base_url"))
	require.Equal(t, account.Proxy.URL(), call.proxyURL,
		"the probe must leave through the account's own proxy; a direct probe shows the account on two IPs at once")
	require.Equal(t, tlsfingerprint.MirasimProfile().Name, call.tlsProfile)
	require.True(t, call.signingDisabled,
		"the decorator must not also sign: two signatures over different header sets cannot be verified")
	require.True(t, call.redirectDisabled)
	require.Equal(t, "identity", call.acceptEncoding,
		"accept-encoding=identity is the observable proof the /v1/limits branch ran, i.e. x-mirasim-probe was set before sealing")
	require.Equal(t, "Bearer tkt_quota", call.authorization)
	require.Empty(t, call.plaintextMirasim,
		"every x-mirasim-* header except client/enc must travel inside the seal; these leaked: %v", call.plaintextMirasim)
}

// TestMirasimQuotaProbeOmitsWindowsTheUpstreamDidNotReturn is the differential
// negative for the reading: a response missing one of the four windows must
// produce a snapshot missing that window — NOT a zero-filled placeholder.
//
// A placeholder would carry budget 0, which every downstream reader (the
// exhaustion check, the progress bar) treats as "measured and empty" rather than
// "never measured".
func TestMirasimQuotaProbeOmitsWindowsTheUpstreamDidNotReturn(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	account := newMirasimQuotaAccount()
	upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
		mirasim.LimitsWindow{Name: MirasimWindow5h, Used: 100, Budget: 400, ResetAt: reset.Unix()},
		mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 5000, Budget: 20000, ResetAt: reset.Unix()},
		mirasim.LimitsWindow{Name: MirasimWindow7dClaude, Used: 3000, Budget: 12000, ResetAt: reset.Unix()},
		// 7d_fable deliberately absent.
	)}
	probe, _ := newMirasimQuotaProbe(account, upstream)

	_, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err)

	stored := storedMirasimQuotaSnapshot(t, account)
	require.Equal(t,
		[]string{MirasimWindow5h, MirasimWindow7d, MirasimWindow7dClaude},
		mirasimQuotaWindowNames(stored.Windows),
		"an unreturned window must be absent, not zero-filled")
	for _, window := range stored.Windows {
		require.NotEqual(t, MirasimWindow7dFable, window.Name)
	}

	// The composed view must not invent it either, and the fable family stays
	// fully schedulable on an account whose fable window was never measured.
	view := BuildMirasimQuotaSnapshot(account)
	require.NotNil(t, view)
	require.Equal(t, MirasimQuotaSourceLimits, view.Source)
	require.Equal(t, 3, len(view.Windows))
	require.True(t, account.IsSchedulableForModelWithContext(context.Background(), mirasimQuotaTestFableModel))
}

// TestMirasimQuotaProbeDistinguishesZeroBudgetFromExhaustion: a window whose
// budget the relay did not state (0) is NOT exhausted, no matter what `used`
// says. Zero budget is the shared-placeholder signature and the "we could not
// read it" signature at once; treating it as a full window would park a healthy
// account for days.
func TestMirasimQuotaProbeDistinguishesZeroBudgetFromExhaustion(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	account := newMirasimQuotaAccount()
	upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
		mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 500, Budget: 0, ResetAt: reset.Unix()},
		mirasim.LimitsWindow{Name: MirasimWindow7dClaude, Used: 0, Budget: 0, ResetAt: reset.Unix()},
	)}
	probe, repo := newMirasimQuotaProbe(account, upstream)

	_, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err)

	stored := storedMirasimQuotaSnapshot(t, account)
	sevenDay := mirasimQuotaWindowByName(t, stored.Windows, MirasimWindow7d)
	require.NotNil(t, sevenDay.Budget, "a stated 0 budget must survive as a measured 0, not as an absent field")
	require.Equal(t, 0.0, *sevenDay.Budget)
	require.Nil(t, sevenDay.Utilization, "utilization must stay unknown when budget is 0; 0.0 would render as a full window")

	require.Empty(t, stored.AppliedWindows)
	require.Equal(t, 0, repo.cooldownWriteCount(), "an unreadable budget must never produce a cooldown")
	require.True(t, account.IsSchedulableForModelWithContext(context.Background(), mirasimQuotaTestOpusModel))
	require.True(t, account.IsSchedulableForModelWithContext(context.Background(), mirasimQuotaTestFableModel))
}

// ---------------------------------------------------------------------------
// The scheduling linkage
// ---------------------------------------------------------------------------

// TestMirasimQuotaExhaustedFamilyWindowGatesTheRightModels pins the per-(account,
// scope) storage AND the containment between the two family windows.
//
// 2026-09-17 语义更正：claude ⊇ fable，两个家族不是并列的。
//
//	7d_claude 耗尽 → opus 停，fable **也**停（fable 的额度算在 claude 窗口里）
//	7d_fable  耗尽 → 只有 fable 停，opus 照常
//
// 这条不对称正是判据的鉴别力所在：把两边都写成「全停」或「只停自己」都会让其中
// 一臂变红。原版第一臂断言 7d_claude 耗尽时 fable 仍可调度，那是缺陷本体 ——
// 生产上调度器据此反复选中已耗尽的号，一路撞上游 429。
//
// 账号级标量那条断言保留：家族窗口永远不该写到 account scalar 上。
func TestMirasimQuotaExhaustedFamilyWindowGatesTheRightModels(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	cases := []struct {
		name          string
		exhausted     string
		scope         string
		blockedModels []string
		servedModels  []string
	}{
		// 包含方耗尽 → 两个都停。
		{"claude family spent blocks fable too", MirasimWindow7dClaude, mirasimClaude7dRateLimitKey,
			[]string{mirasimQuotaTestOpusModel, mirasimQuotaTestFableModel}, nil},
		// 被包含方耗尽 → 只停自己。
		{"fable family spent blocks only fable", MirasimWindow7dFable, mirasimFable7dRateLimitKey,
			[]string{mirasimQuotaTestFableModel}, []string{mirasimQuotaTestOpusModel}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			windows := []mirasim.LimitsWindow{
				{Name: MirasimWindow5h, Used: 10, Budget: 400, ResetAt: reset.Unix()},
				{Name: MirasimWindow7d, Used: 5000, Budget: 20000, ResetAt: reset.Unix()},
				{Name: MirasimWindow7dClaude, Used: 1, Budget: 12000, ResetAt: reset.Unix()},
				{Name: MirasimWindow7dFable, Used: 1, Budget: 900, ResetAt: reset.Unix()},
			}
			for i := range windows {
				if windows[i].Name == tc.exhausted {
					windows[i].Used = windows[i].Budget
				}
			}
			account := newMirasimQuotaAccount()
			upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false, windows...)}
			probe, repo := newMirasimQuotaProbe(account, upstream)

			_, err := probe.ProbeAccount(context.Background(), account.ID)
			require.NoError(t, err)

			stored := storedMirasimQuotaSnapshot(t, account)
			require.Equal(t, []string{tc.exhausted}, stored.AppliedWindows)

			repo.mu.Lock()
			modelCalls := append([]mirasimQuotaModelLimitCall(nil), repo.modelLimitCalls...)
			accountCalls := len(repo.rateLimitCalls)
			repo.mu.Unlock()
			require.Len(t, modelCalls, 1)
			require.Equal(t, tc.scope, modelCalls[0].scope)
			require.Equal(t, mirasimWindowReason(tc.exhausted), modelCalls[0].reason)
			require.Equal(t, reset.UTC(), modelCalls[0].resetAt.UTC())
			require.Equal(t, 0, accountCalls,
				"a family window must never reach the account-level scalar; that would block the other family too")

			ctx := context.Background()
			for _, blocked := range tc.blockedModels {
				require.Falsef(t, account.IsSchedulableForModelWithContext(ctx, blocked),
					"%s 耗尽，%s 必须停止被调度到这个账号", tc.exhausted, blocked)
			}
			for _, served := range tc.servedModels {
				require.Truef(t, account.IsSchedulableForModelWithContext(ctx, served),
					"%s 耗尽不影响 %s —— 它不消耗那个窗口", tc.exhausted, served)
			}
			require.True(t, account.IsSchedulable(),
				"the account itself is healthy; only one family is gated")
		})
	}
}

// TestMirasimQuotaGlobal7dGatesTheAccountButA5hSpendDoesNot pins the split
// ma-relay also makes: the 7-day global window is cooled proactively, the
// transient 5-hour one is left to reactive 429 handling.
//
// 5h is a ROLLING window whose `used` decays continuously while reset_at is only
// the point it would fully clear; a proactive cooldown until reset_at would park
// a healthy account for hours it does not owe. The second arm is what keeps a
// future "just gate every window" simplification from passing.
func TestMirasimQuotaGlobal7dGatesTheAccountButA5hSpendDoesNot(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	fiveHourReset := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	t.Run("7d spent cools the account-level scalar", func(t *testing.T) {
		account := newMirasimQuotaAccount()
		upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
			mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 20000, Budget: 20000, ResetAt: reset.Unix()},
		)}
		probe, repo := newMirasimQuotaProbe(account, upstream)

		_, err := probe.ProbeAccount(context.Background(), account.ID)
		require.NoError(t, err)

		stored := storedMirasimQuotaSnapshot(t, account)
		require.Equal(t, []string{MirasimWindow7d}, stored.AppliedWindows)
		repo.mu.Lock()
		require.Len(t, repo.rateLimitCalls, 1)
		require.Empty(t, repo.modelLimitCalls)
		repo.mu.Unlock()
		require.False(t, account.IsSchedulable())
	})

	t.Run("5h spent is left to the reactive 429 path", func(t *testing.T) {
		account := newMirasimQuotaAccount()
		upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
			mirasim.LimitsWindow{Name: MirasimWindow5h, Used: 400, Budget: 400, ResetAt: fiveHourReset.Unix()},
			mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 1, Budget: 20000, ResetAt: reset.Unix()},
		)}
		probe, repo := newMirasimQuotaProbe(account, upstream)

		_, err := probe.ProbeAccount(context.Background(), account.ID)
		require.NoError(t, err)

		stored := storedMirasimQuotaSnapshot(t, account)
		require.Empty(t, stored.AppliedWindows, "5h must not be proactively cooled")
		// The reading itself is still recorded — the operator sees it, the
		// scheduler just does not act on it.
		fiveHour := mirasimQuotaWindowByName(t, stored.Windows, MirasimWindow5h)
		require.NotNil(t, fiveHour.Utilization)
		require.InDelta(t, 1.0, *fiveHour.Utilization, 1e-9)
		require.Equal(t, 0, repo.cooldownWriteCount())
		require.True(t, account.IsSchedulable())
	})
}

// TestMirasimQuotaUnrecognizedWindowNameIsRecordedNotActedOn: a window under a
// token this repository does not grade is stored verbatim and flagged, and
// drives nothing.
//
// Two of the four tokens we act on have never been observed and the
// counter-evidence says both are probably wrong (see
// mirasimWindowTokenCounterEvidence). A probe that quietly discarded an
// unrecognised name would destroy the only evidence that can settle it.
func TestMirasimQuotaUnrecognizedWindowNameIsRecordedNotActedOn(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	account := newMirasimQuotaAccount()
	upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
		mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 1, Budget: 20000, ResetAt: reset.Unix()},
		// "7d_oi" is what anthropic actually calls the fable 7-day window, and
		// the leading candidate for what mirasim calls it too.
		mirasim.LimitsWindow{Name: "7d_oi", Used: 900, Budget: 900, ResetAt: reset.Unix()},
	)}
	probe, repo := newMirasimQuotaProbe(account, upstream)

	_, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err)

	stored := storedMirasimQuotaSnapshot(t, account)
	require.Equal(t, []string{"7d_oi"}, stored.UnknownWindows)
	require.Equal(t, []string{MirasimWindow7d, "7d_oi"}, mirasimQuotaWindowNames(stored.Windows),
		"the unrecognised window must be stored verbatim; normalising or dropping it destroys the evidence")
	require.Empty(t, stored.AppliedWindows,
		"an ungraded token must not drive a cooldown — we do not know which family it belongs to")
	require.Equal(t, 0, repo.cooldownWriteCount())
	require.True(t, account.IsSchedulableForModelWithContext(context.Background(), mirasimQuotaTestFableModel))
}

// TestMirasimQuotaMillisecondResetIsNormalisedNotDropped: a 13-digit reset_at is
// a millisecond timestamp, and this repository already normalises exactly that
// for the anthropic reset header (parseAnthropicResetTimestamp). Read as
// seconds it would land in the year ~57680, which encoding/json REFUSES to
// marshal — so without normalisation the whole snapshot write fails and a
// perfectly readable response records nothing at all.
func TestMirasimQuotaMillisecondResetIsNormalisedNotDropped(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	account := newMirasimQuotaAccount()
	upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
		mirasim.LimitsWindow{Name: MirasimWindow7dClaude, Used: 12000, Budget: 12000, ResetAt: reset.UnixMilli()},
	)}
	probe, repo := newMirasimQuotaProbe(account, upstream)

	_, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err, "a millisecond reset_at must not make the snapshot unserialisable")

	stored := storedMirasimQuotaSnapshot(t, account)
	claude := mirasimQuotaWindowByName(t, stored.Windows, MirasimWindow7dClaude)
	require.NotNil(t, claude.ResetAt)
	require.Equal(t, reset.UTC(), claude.ResetAt.UTC(),
		"a 13-digit reset_at must be read as milliseconds, the same rule parseAnthropicResetTimestamp applies")
	require.Equal(t, []string{MirasimWindow7dClaude}, stored.AppliedWindows)
	require.Equal(t, 1, repo.cooldownWriteCount())
}

// TestMirasimQuotaUnusableResetDegradesToAbsentNotToAFailedWrite: a value that
// is not a timestamp even after millisecond normalisation is recorded as "no
// reset stated" — a state every consumer already handles — rather than being
// kept and taking the whole snapshot write down with it.
func TestMirasimQuotaUnusableResetDegradesToAbsentNotToAFailedWrite(t *testing.T) {
	account := newMirasimQuotaAccount()
	upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
		mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 4000, Budget: 20000, ResetAt: 1_000_000_000_000_000},
	)}
	probe, repo := newMirasimQuotaProbe(account, upstream)

	snapshot, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, MirasimQuotaProbeStatusOK, snapshot.Status)

	stored := storedMirasimQuotaSnapshot(t, account)
	sevenDay := mirasimQuotaWindowByName(t, stored.Windows, MirasimWindow7d)
	require.Nil(t, sevenDay.ResetAt, "an unusable reset_at must be dropped, not kept")
	require.NotNil(t, sevenDay.Used, "dropping the reset must not cost the rest of the reading")
	require.Equal(t, 4000.0, *sevenDay.Used)
	require.Equal(t, 0, repo.cooldownWriteCount())
}

// TestMirasimQuotaExhaustedWindowWithAnUnusableResetWritesNoCooldown covers the
// two ways a spent window can still be unactionable, because a cooldown is a
// promise about WHEN scheduling resumes and neither of these can make it:
// a reset further out than any real window can last, and no reset at all.
//
// In both cases the reading is still recorded verbatim; only the enforcement is
// withheld.
func TestMirasimQuotaExhaustedWindowWithAnUnusableResetWritesNoCooldown(t *testing.T) {
	cases := []struct {
		name    string
		resetAt int64
	}{
		// 30 days out: past the 8-day sanity bound, but still a plausible
		// timestamp, so it IS recorded — it just cannot be enforced.
		{"reset further out than any real window", time.Now().Add(30 * 24 * time.Hour).Unix()},
		// Spent, with no stated end at all.
		{"no reset at all", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := newMirasimQuotaAccount()
			upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
				mirasim.LimitsWindow{Name: MirasimWindow7dClaude, Used: 12000, Budget: 12000, ResetAt: tc.resetAt},
			)}
			probe, repo := newMirasimQuotaProbe(account, upstream)

			_, err := probe.ProbeAccount(context.Background(), account.ID)
			require.NoError(t, err)

			stored := storedMirasimQuotaSnapshot(t, account)
			require.Equal(t, MirasimQuotaProbeStatusOK, stored.Status)
			// The numbers are recorded; only the enforcement is withheld.
			claude := mirasimQuotaWindowByName(t, stored.Windows, MirasimWindow7dClaude)
			require.NotNil(t, claude.Utilization)
			require.InDelta(t, 1.0, *claude.Utilization, 1e-9)

			require.Empty(t, stored.AppliedWindows,
				"a cooldown must not be written when its end time is unusable")
			require.Equal(t, 0, repo.cooldownWriteCount())
			require.True(t, account.IsSchedulableForModelWithContext(context.Background(), mirasimQuotaTestOpusModel),
				"an unusable reset_at must not park the account; the reactive 429 path still has current headers")
		})
	}
}

// TestMirasimQuotaAlreadyResetWindowIsNotExhausted: a snapshot whose reset_at
// has already passed describes a window that has since cleared, and re-cooling
// it would re-park an account that just recovered. ma-relay makes the same call
// ("A window past its reset time is treated as fresh").
func TestMirasimQuotaAlreadyResetWindowIsNotExhausted(t *testing.T) {
	account := newMirasimQuotaAccount()
	upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
		mirasim.LimitsWindow{
			Name:    MirasimWindow7dClaude,
			Used:    12000,
			Budget:  12000,
			ResetAt: time.Now().Add(-2 * time.Hour).Unix(),
		},
	)}
	probe, repo := newMirasimQuotaProbe(account, upstream)

	_, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err)

	stored := storedMirasimQuotaSnapshot(t, account)
	require.Empty(t, stored.AppliedWindows)
	require.Equal(t, 0, repo.cooldownWriteCount())
	require.True(t, account.IsSchedulableForModelWithContext(context.Background(), mirasimQuotaTestOpusModel))
}

// ---------------------------------------------------------------------------
// Failure is loud
// ---------------------------------------------------------------------------

// TestMirasimQuotaProbeFailuresNeverLookLikeAReading walks the three ways a
// probe can fail after a good reading exists. In every one the snapshot must
// flip to failed with a status and a stable reason, the surviving numbers must
// keep their ORIGINAL observed_at, and no cooldown may be written.
//
// The single failure this guards against: a failed probe recorded as a healthy
// zero. Downstream, "budget 0 / used 0" is indistinguishable from a real reading
// of an empty account, and the account either gets parked forever or scheduled
// into a window that is actually spent.
func TestMirasimQuotaProbeFailuresNeverLookLikeAReading(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	goodBody := mirasimLimitsBody(t, false,
		mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 4000, Budget: 20000, ResetAt: reset.Unix()},
		mirasim.LimitsWindow{Name: MirasimWindow7dClaude, Used: 1000, Budget: 12000, ResetAt: reset.Unix()},
	)

	account := newMirasimQuotaAccount()
	upstream := &mirasimQuotaUpstream{limitsBody: goodBody}
	probe, repo := newMirasimQuotaProbe(account, upstream)

	_, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err)
	good := storedMirasimQuotaSnapshot(t, account)
	require.Equal(t, MirasimQuotaProbeStatusOK, good.Status)
	require.NotNil(t, good.ObservedAt)
	goodObservedAt := *good.ObservedAt
	goodWindows := mirasimQuotaWindowNames(good.Windows)
	cooldownsAfterGoodProbe := repo.cooldownWriteCount()

	failures := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantReason string
	}{
		{"upstream refused", http.StatusServiceUnavailable, goodBody, http.StatusServiceUnavailable, "upstream_error"},
		{"credential rejected", http.StatusUnauthorized, goodBody, http.StatusUnauthorized, "credential_rejected"},
		{"body is not json", http.StatusOK, "<html>proxy auth required</html>", http.StatusOK, "invalid_body"},
		{"json we cannot read", http.StatusOK, `{"quotas":[{"label":"7d","consumed":1,"cap":2}]}`, http.StatusOK, "empty_windows"},
	}
	for i, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			upstream.mu.Lock()
			upstream.limitsStatus = tc.status
			upstream.limitsBody = tc.body
			upstream.mu.Unlock()

			snapshot, err := probe.ProbeAccount(context.Background(), account.ID)
			require.NoError(t, err)
			require.NotNil(t, snapshot)

			stored := storedMirasimQuotaSnapshot(t, account)
			require.Equal(t, MirasimQuotaProbeStatusFailed, stored.Status)
			require.Equal(t, tc.wantStatus, stored.HTTPStatus)
			require.Equal(t, tc.wantReason, stored.LastError)
			require.Equal(t, i+1, stored.FailureCount, "consecutive failures must accumulate so the backoff can grow")

			// The last good reading survives, and it still says WHEN it was read.
			require.Equal(t, goodWindows, mirasimQuotaWindowNames(stored.Windows))
			require.NotNil(t, stored.ObservedAt)
			require.Equal(t, goodObservedAt.UTC(), stored.ObservedAt.UTC(),
				"a failed probe must not restamp the surviving reading as fresh")
			require.True(t, stored.LastAttemptAt.After(goodObservedAt) || stored.LastAttemptAt.Equal(goodObservedAt),
				"last_attempt_at must move even though observed_at does not")

			// Nothing was measured, so nothing may be enforced.
			require.Equal(t, cooldownsAfterGoodProbe, repo.cooldownWriteCount(),
				"a failed probe wrote a cooldown; 'could not read' is not 'exhausted'")

			// The composed view carries the failure forward instead of
			// presenting the stale numbers as current.
			view := BuildMirasimQuotaSnapshot(account)
			require.NotNil(t, view)
			require.Equal(t, MirasimQuotaProbeStatusFailed, view.ProbeStatus)
			require.Equal(t, tc.wantReason, view.ProbeError)
			require.Equal(t, goodObservedAt.UTC(), view.ObservedAt.UTC())
		})
	}

	// And the backoff really grows. The bound has to clear the JITTERED ceiling
	// of a non-backed-off delay (interval + interval/5), otherwise "no backoff at
	// all" passes on a lucky jitter draw — which is exactly how the first version
	// of this assertion survived a mutation that removed the backoff.
	stored := storedMirasimQuotaSnapshot(t, account)
	interval := time.Duration(mirasimQuotaProbeDefaultIntervalMinutes) * time.Minute
	delay := stored.NextProbeAt.Sub(stored.LastAttemptAt)
	require.Greaterf(t, delay, 2*interval,
		"after %d consecutive failures the next attempt is only %s out (interval=%s); "+
			"a dead account would keep consuming a cycle slot every interval forever",
		stored.FailureCount, delay, interval)
}

// TestMirasimQuotaNextProbeDelayBacksOffOnConsecutiveFailures is the direct
// differential on the scheduler arithmetic.
//
// It exists because the shared nextProbeDelay takes a RETRY-AFTER duration as
// its second argument, not a failure count — so the plan probe, which passes 0,
// has no backoff at all. Anything that "simplified" this function back to that
// one would reintroduce the same hole, and the account-level test above can only
// see it through a jittered delay.
func TestMirasimQuotaNextProbeDelayBacksOffOnConsecutiveFailures(t *testing.T) {
	const interval = mirasimQuotaProbeDefaultIntervalMinutes
	base := time.Duration(interval) * time.Minute

	// Success: the configured interval, jitter only (±20%, capped at 5m).
	for i := 0; i < 20; i++ {
		delay := mirasimQuotaNextProbeDelay(interval, 0)
		require.GreaterOrEqual(t, delay, base-base/5)
		require.LessOrEqual(t, delay, base+base/5)
	}

	// The FIRST failure retries at the normal cadence: one timeout says nothing
	// about the account, and punishing it immediately would delay the recovery of
	// a pool that just rode out a transient upstream blip.
	first := mirasimQuotaNextProbeDelay(interval, 1)
	require.GreaterOrEqual(t, first, base-base/5)
	require.LessOrEqual(t, first, base+base/5)

	// From the second consecutive failure on, each one lands strictly beyond the
	// JITTERED CEILING of the previous step — not merely beyond the previous
	// draw, which a lucky jitter could satisfy with no backoff at all.
	previous := base + base/5
	for failures := 2; failures <= 5; failures++ {
		delay := mirasimQuotaNextProbeDelay(interval, failures)
		require.Greaterf(t, delay, previous,
			"failure #%d scheduled %s, not beyond the previous step's ceiling %s", failures, delay, previous)
		require.LessOrEqualf(t, delay, mirasimQuotaProbeMaxDelay+5*time.Minute,
			"failure #%d scheduled %s, past the %s ceiling — a recovered account would never be rediscovered",
			failures, delay, mirasimQuotaProbeMaxDelay)
		previous = delay
	}

	// A pathological failure count must still land at the ceiling, not overflow
	// into a negative or absurd duration.
	require.LessOrEqual(t, mirasimQuotaNextProbeDelay(interval, 4096), mirasimQuotaProbeMaxDelay+5*time.Minute)
	require.Greater(t, mirasimQuotaNextProbeDelay(interval, 4096), time.Duration(0))

	// The floor holds even when a caller asks for an absurdly small interval.
	require.GreaterOrEqual(t, mirasimQuotaNextProbeDelay(0, 0),
		time.Duration(mirasimQuotaProbeMinIntervalMinutes)*time.Minute-time.Minute)
}

// TestMirasimQuotaProbeFailsBeforeSendingAnUnsignableRequest: without a device
// seed the request cannot be signed, and an UNSIGNED /v1/limits does not fail —
// the relay answers it with a shared placeholder budget. Sending it anyway would
// feed the scheduler a number belonging to nobody.
func TestMirasimQuotaProbeFailsBeforeSendingAnUnsignableRequest(t *testing.T) {
	account := newMirasimQuotaAccount()
	delete(account.Credentials, mirasim.CredDeviceSeed)
	upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
		mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 1, Budget: 2, ResetAt: time.Now().Add(time.Hour).Unix()},
	)}
	probe, repo := newMirasimQuotaProbe(account, upstream)

	snapshot, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, MirasimQuotaProbeStatusFailed, snapshot.Status)
	require.Equal(t, "missing_device_seed", snapshot.LastError)
	require.Empty(t, snapshot.Windows)

	upstream.mu.Lock()
	calls := len(upstream.calls)
	upstream.mu.Unlock()
	require.Equal(t, 0, calls, "an unsignable probe must not reach the relay at all")
	require.Equal(t, 0, repo.cooldownWriteCount())
}

// TestMirasimQuotaProbeRefusesAnAccountWhoseProxyChanged: the egress identity is
// read from the account snapshot, so a proxy swapped between load and send must
// abort rather than silently probe from a different IP.
func TestMirasimQuotaProbeRefusesAnAccountWhoseProxyChanged(t *testing.T) {
	account := newMirasimQuotaAccount()
	other := int64(99)
	account.ProxyID = &other // the bound proxy no longer matches the loaded one
	upstream := &mirasimQuotaUpstream{limitsBody: "{}"}
	probe, _ := newMirasimQuotaProbe(account, upstream)

	_, err := probe.ProbeAccount(context.Background(), account.ID)
	require.ErrorIs(t, err, ErrMirasimQuotaProbeIdentityChanged)

	upstream.mu.Lock()
	calls := len(upstream.calls)
	upstream.mu.Unlock()
	require.Equal(t, 0, calls)
}

// TestMirasimQuotaSuspendedAccountIsARealReadingNotAFailure: an account the
// upstream has halted can legitimately report no windows at all. That is the one
// zero-window 200 which is NOT "we could not read it", and collapsing the two
// would either lose the suspension or mark a readable response as broken.
func TestMirasimQuotaSuspendedAccountIsARealReadingNotAFailure(t *testing.T) {
	account := newMirasimQuotaAccount()
	upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, true)}
	probe, repo := newMirasimQuotaProbe(account, upstream)

	snapshot, err := probe.ProbeAccount(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, MirasimQuotaProbeStatusOK, snapshot.Status,
		"a suspended account with no windows is a successful reading, not a failed probe")

	stored := storedMirasimQuotaSnapshot(t, account)
	require.True(t, stored.Suspended)
	require.Empty(t, stored.Windows)
	require.Empty(t, stored.AppliedWindows)
	require.Equal(t, 0, repo.cooldownWriteCount())

	// The suspension must survive into the view even though there is no window
	// to hang it on.
	view := BuildMirasimQuotaSnapshot(account)
	require.NotNil(t, view)
	require.True(t, view.Suspended, "suspension is a fact about the account, not about a window")
	require.Empty(t, view.Windows)
	require.NotNil(t, view.ObservedAt)
}

// ---------------------------------------------------------------------------
// The composed view
// ---------------------------------------------------------------------------

// TestBuildMirasimQuotaSnapshotProvenance: with no probe reading the view falls
// back to the passive header samples, labels itself "headers", and leaves the
// absolute counters ABSENT rather than back-deriving them from a budget read at
// some other time.
func TestBuildMirasimQuotaSnapshotProvenance(t *testing.T) {
	t.Run("non-mirasim account has no mirasim quota", func(t *testing.T) {
		require.Nil(t, BuildMirasimQuotaSnapshot(plainAnthropicTestAccount()))
	})

	t.Run("nothing observed yet", func(t *testing.T) {
		view := BuildMirasimQuotaSnapshot(newMirasimQuotaAccount())
		require.NotNil(t, view)
		require.Equal(t, "", view.Source, "never-probed must be its own state, not 'headers' with zero windows")
		require.Empty(t, view.Windows)
	})

	t.Run("header samples only", func(t *testing.T) {
		reset := time.Now().Add(48 * time.Hour).Truncate(time.Second)
		sampledAt := time.Now().Add(-90 * time.Second).UTC().Truncate(time.Second)
		account := newMirasimQuotaAccount()
		account.Extra[mirasimPassive5hUtilizationKey] = 0.42
		account.Extra[mirasimPassive7dUtilizationKey] = 0.0
		account.Extra[mirasimPassive7dResetKey] = float64(reset.Unix())
		account.Extra[mirasimPassiveSampledAtKey] = sampledAt.Format(time.RFC3339)

		view := BuildMirasimQuotaSnapshot(account)
		require.NotNil(t, view)
		require.Equal(t, MirasimQuotaSourceHeaders, view.Source)
		require.Equal(t, []string{MirasimWindow5h, MirasimWindow7d}, mirasimQuotaWindowNames(view.Windows))
		require.NotNil(t, view.ObservedAt)
		require.Equal(t, sampledAt, view.ObservedAt.UTC())

		fiveHour := mirasimQuotaWindowByName(t, view.Windows, MirasimWindow5h)
		require.NotNil(t, fiveHour.Utilization)
		require.InDelta(t, 0.42, *fiveHour.Utilization, 1e-9)
		require.Nil(t, fiveHour.Used, "the headers never carried an absolute used; inventing one would fake a billing-grade number")
		require.Nil(t, fiveHour.Budget)

		sevenDay := mirasimQuotaWindowByName(t, view.Windows, MirasimWindow7d)
		require.NotNil(t, sevenDay.Utilization, "a sampled 0.0 is a reading, not an absence")
		require.Equal(t, 0.0, *sevenDay.Utilization)
		require.NotNil(t, sevenDay.ResetAt)
		require.Equal(t, reset.UTC(), sevenDay.ResetAt.UTC())
	})

	t.Run("a probe reading outranks header samples", func(t *testing.T) {
		reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
		account := newMirasimQuotaAccount()
		account.Extra[mirasimPassive5hUtilizationKey] = 0.9
		account.Extra[mirasimPassiveSampledAtKey] = time.Now().UTC().Format(time.RFC3339)
		upstream := &mirasimQuotaUpstream{limitsBody: mirasimLimitsBody(t, false,
			mirasim.LimitsWindow{Name: MirasimWindow7d, Used: 4000, Budget: 20000, ResetAt: reset.Unix()},
		)}
		probe, _ := newMirasimQuotaProbe(account, upstream)
		_, err := probe.ProbeAccount(context.Background(), account.ID)
		require.NoError(t, err)

		view := BuildMirasimQuotaSnapshot(account)
		require.NotNil(t, view)
		require.Equal(t, MirasimQuotaSourceLimits, view.Source)
		require.Equal(t, []string{MirasimWindow7d}, mirasimQuotaWindowNames(view.Windows),
			"the richer source is published whole; header windows must not be merged in alongside it")
		sevenDay := mirasimQuotaWindowByName(t, view.Windows, MirasimWindow7d)
		require.NotNil(t, sevenDay.Used)
		require.Equal(t, 4000.0, *sevenDay.Used)
	})
}
