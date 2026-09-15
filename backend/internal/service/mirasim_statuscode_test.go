//go:build unit

package service

// Status-code semantics for the mirasim channel.
//
// Every test here drives the real production entry point
// (RateLimitService.HandleUpstreamError) with a real mirasim account — that is,
// platform=anthropic plus credentials.provider=mirasim — and asserts on what
// reached the account repository. A fake repository is used because the
// question under test is "what account state does this status code write", and
// the repository call IS that state.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type mirasimRepoCall struct {
	method  string
	id      int64
	scope   string
	resetAt time.Time
	reason  string
	until   time.Time
	message string
}

// mirasimAccountRepoStub records every account-state write and applies the ones
// whose local effect the tests observe, mirroring accountRepository: SetError
// flips status to error AND clears schedulable (account_repo.go SetError).
//
// The embedded interface is nil: any repository method these paths are not
// supposed to touch panics loudly instead of silently succeeding.
type mirasimAccountRepoStub struct {
	AccountRepository

	bound *Account
	calls []mirasimRepoCall
}

func (r *mirasimAccountRepoStub) record(call mirasimRepoCall) {
	r.calls = append(r.calls, call)
}

func (r *mirasimAccountRepoStub) callsOf(method string) []mirasimRepoCall {
	var out []mirasimRepoCall
	for _, call := range r.calls {
		if call.method == method {
			out = append(out, call)
		}
	}
	return out
}

func (r *mirasimAccountRepoStub) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	r.record(mirasimRepoCall{method: "SetRateLimited", id: id, resetAt: resetAt})
	if r.bound != nil && r.bound.ID == id {
		reset := resetAt
		r.bound.RateLimitResetAt = &reset
	}
	return nil
}

func (r *mirasimAccountRepoStub) SetModelRateLimit(ctx context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	call := mirasimRepoCall{method: "SetModelRateLimit", id: id, scope: scope, resetAt: resetAt}
	if len(reason) > 0 {
		call.reason = reason[0]
	}
	r.record(call)
	return nil
}

func (r *mirasimAccountRepoStub) SetError(ctx context.Context, id int64, errorMsg string) error {
	r.record(mirasimRepoCall{method: "SetError", id: id, message: errorMsg})
	if r.bound != nil && r.bound.ID == id {
		r.bound.Status = StatusError
		r.bound.ErrorMessage = errorMsg
		r.bound.Schedulable = false
	}
	return nil
}

func (r *mirasimAccountRepoStub) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	r.record(mirasimRepoCall{method: "SetTempUnschedulable", id: id, until: until, reason: reason})
	if r.bound != nil && r.bound.ID == id {
		stop := until
		r.bound.TempUnschedulableUntil = &stop
		r.bound.TempUnschedulableReason = reason
	}
	return nil
}

func (r *mirasimAccountRepoStub) UpdateSessionWindow(ctx context.Context, id int64, start, end *time.Time, status string) error {
	r.record(mirasimRepoCall{method: "UpdateSessionWindow", id: id, reason: status})
	return nil
}

func (r *mirasimAccountRepoStub) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.record(mirasimRepoCall{method: "UpdateExtra", id: id})
	return nil
}

func (r *mirasimAccountRepoStub) GetByID(ctx context.Context, id int64) (*Account, error) {
	if r.bound != nil && r.bound.ID == id {
		return r.bound, nil
	}
	return nil, errors.New("mirasim stub: account not bound")
}

// mirasimTokenCacheInvalidatorSpy records credential-refresh triggers.
type mirasimTokenCacheInvalidatorSpy struct {
	invalidated []int64
}

func (s *mirasimTokenCacheInvalidatorSpy) InvalidateToken(ctx context.Context, account *Account) error {
	if account != nil {
		s.invalidated = append(s.invalidated, account.ID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// mirasimTestAccount builds the exact shape production recognises: an anthropic
// api-key account carrying the provider marker. No real credential material is
// used — the refresh token is a placeholder string.
func mirasimTestAccount() *Account {
	return &Account{
		ID:          9001,
		Name:        "mirasim-test",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			mirasim.CredProvider:     mirasim.ProviderMirasim,
			mirasim.CredRefreshToken: "placeholder-not-a-real-token",
		},
	}
}

// plainAnthropicTestAccount is the same account WITHOUT the provider marker —
// an ordinary anthropic channel, used to prove nothing changed for it.
func plainAnthropicTestAccount() *Account {
	return &Account{
		ID:          9002,
		Name:        "anthropic-test",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{},
	}
}

func mirasimServiceWithStub() (*RateLimitService, *mirasimAccountRepoStub, *Account) {
	account := mirasimTestAccount()
	repo := &mirasimAccountRepoStub{bound: account}
	return &RateLimitService{accountRepo: repo}, repo, account
}

// mirasimWindowRejectedHeaders marks exactly one window as rejected with a
// parsable reset, using the unified-ratelimit header family mirasim relays.
func mirasimWindowRejectedHeaders(window string, resetAt time.Time) http.Header {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-"+window+"-status", "rejected")
	h.Set("anthropic-ratelimit-unified-"+window+"-reset", strconv.FormatInt(resetAt.Unix(), 10))
	return h
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestMirasim503CapacityLeavesAccountHealthy(t *testing.T) {
	// [[cov:SC:503-capacity]]
	svc, repo, account := mirasimServiceWithStub()

	shouldDisable := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusServiceUnavailable,
		http.Header{},
		[]byte(`{"error":{"type":"overloaded_error","message":"model capacity exhausted"}}`),
		"claude-opus-4-6",
	)

	require.False(t, shouldDisable)
	// The account itself is healthy: this model just had no capacity right now.
	require.Empty(t, repo.calls, "a 503 must not write any account state")
	require.Nil(t, account.RateLimitResetAt)
	require.Nil(t, account.TempUnschedulableUntil)
	require.True(t, account.IsSchedulable())
	require.True(t, account.IsSchedulableForModelWithContext(context.Background(), "claude-opus-4-6"))
}

func TestMirasim4295hWindowCoolsAccountLevelScalarOnly(t *testing.T) {
	// [[cov:SC:429-5h]]
	svc, repo, account := mirasimServiceWithStub()
	reset := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		mirasimWindowRejectedHeaders(MirasimWindow5h, reset),
		nil,
		"claude-opus-4-6",
	)

	// 5h is a global window → the account-level scalar, at exactly the reset the
	// upstream named (no fallback cooldown, no rounding).
	rateLimited := repo.callsOf("SetRateLimited")
	require.Len(t, rateLimited, 1)
	require.Equal(t, reset.Unix(), rateLimited[0].resetAt.Unix())
	// The other three windows are untouched: neither family scope was written,
	// and the only window this response reported is 5h.
	require.Empty(t, repo.callsOf("SetModelRateLimit"))
	require.False(t, account.isRateLimitActiveForKey(mirasimClaude7dRateLimitKey))
	require.False(t, account.isRateLimitActiveForKey(mirasimFable7dRateLimitKey))
	selected := selectMirasimExhaustedWindows(mirasimWindowRejectedHeaders(MirasimWindow5h, reset), time.Now())
	require.Len(t, selected, 1)
	require.Equal(t, MirasimWindow5h, selected[0].window)
}

func TestMirasim4297dWindowCoolsAccountLevelScalarOnly(t *testing.T) {
	// [[cov:SC:429-7d]]
	svc, repo, account := mirasimServiceWithStub()
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	headers := mirasimWindowRejectedHeaders(MirasimWindow7d, reset)

	svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, headers, nil, "claude-sonnet-4-5")

	rateLimited := repo.callsOf("SetRateLimited")
	require.Len(t, rateLimited, 1)
	require.Equal(t, reset.Unix(), rateLimited[0].resetAt.Unix())
	// 5h was not reported as exhausted by this response, so it is not marked:
	// the only window selected is 7d. (5h and 7d intentionally share the
	// account-level scalar, so this is where that distinction is observable.)
	selected := selectMirasimExhaustedWindows(headers, time.Now())
	require.Len(t, selected, 1)
	require.Equal(t, MirasimWindow7d, selected[0].window)
	require.Empty(t, repo.callsOf("SetModelRateLimit"))
}

func TestMirasim4297dClaudeLocksClaudeFamilyOnly(t *testing.T) {
	// [[cov:SC:429-7d-claude]]
	ctx := context.Background()
	svc, repo, account := mirasimServiceWithStub()
	reset := time.Now().Add(48 * time.Hour).Truncate(time.Second)

	svc.HandleUpstreamError(ctx, account, http.StatusTooManyRequests,
		mirasimWindowRejectedHeaders(MirasimWindow7dClaude, reset), nil, "claude-opus-4-6")

	scoped := repo.callsOf("SetModelRateLimit")
	require.Len(t, scoped, 1)
	require.Equal(t, mirasimClaude7dRateLimitKey, scoped[0].scope)
	require.Equal(t, mirasimWindowReason(MirasimWindow7dClaude), scoped[0].reason)
	// A family window must never touch the global scalar.
	require.Empty(t, repo.callsOf("SetRateLimited"))
	require.True(t, account.IsSchedulable())
	// claude family stopped, fable family still served by the same account.
	require.False(t, account.IsSchedulableForModelWithContext(ctx, "claude-opus-4-6"))
	require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-fable-5"))
}

func TestMirasim4297dFableLocksFableFamilyOnly(t *testing.T) {
	// [[cov:SC:429-7d-fable]]
	ctx := context.Background()
	svc, repo, account := mirasimServiceWithStub()
	reset := time.Now().Add(96 * time.Hour).Truncate(time.Second)

	svc.HandleUpstreamError(ctx, account, http.StatusTooManyRequests,
		mirasimWindowRejectedHeaders(MirasimWindow7dFable, reset), nil, "claude-fable-5")

	scoped := repo.callsOf("SetModelRateLimit")
	require.Len(t, scoped, 1)
	require.Equal(t, mirasimFable7dRateLimitKey, scoped[0].scope)
	require.Empty(t, repo.callsOf("SetRateLimited"))
	require.False(t, account.IsSchedulableForModelWithContext(ctx, "claude-fable-5"))
	require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-opus-4-6"))
}

func TestMirasim403DisablesAccountViaUnchangedSharedPath(t *testing.T) {
	// [[cov:SC:403-banned]]
	// 403 means the account is banned upstream. The operator decision is that a
	// banned account gets disabled; this test pins that behaviour so a later
	// refactor of the mirasim branch cannot quietly soften it.
	svc, repo, account := mirasimServiceWithStub()

	// The mirasim branch explicitly declines to handle 403: it must reach the
	// shared handle403 path byte-for-byte.
	handled, _ := svc.handleMirasimUpstreamError(context.Background(), account,
		http.StatusForbidden, http.Header{}, []byte(`{"error":{"message":"account suspended"}}`))
	require.False(t, handled)
	require.Equal(t, MirasimActionAccountFatal, MirasimClassifyStatus(http.StatusForbidden, nil))

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden,
		http.Header{}, []byte(`{"error":{"message":"account suspended"}}`), "claude-opus-4-6")

	require.True(t, shouldDisable)
	require.Len(t, repo.callsOf("SetError"), 1)
	require.Contains(t, repo.callsOf("SetError")[0].message, "403")
	require.Equal(t, StatusError, account.Status)
	require.False(t, account.IsSchedulable())
}

func TestMirasim400DoesNotPolluteRequestsThatDifferOnlyInHeaders(t *testing.T) {
	// [[cov:SC:400-badrequest]]
	ctx := context.Background()
	svc, repo, account := mirasimServiceWithStub()

	body := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}]}`)
	withBeta := http.Header{}
	withBeta.Set("anthropic-beta", "some-unsupported-beta-2026-01-01")
	withBeta.Set("content-type", "application/json")
	withoutBeta := http.Header{}
	withoutBeta.Set("content-type", "application/json")

	// A header-shaped 400 is a property of THIS request, so nothing about the
	// account is recorded and the next request starts clean.
	shouldDisable := svc.HandleUpstreamError(ctx, account, http.StatusBadRequest, withBeta,
		[]byte(`{"error":{"type":"invalid_request_error","message":"unsupported beta header"}}`),
		"claude-opus-4-6")
	require.False(t, shouldDisable)
	require.Empty(t, repo.calls, "a header-shaped 400 must not persist account state")
	require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-opus-4-6"))
	require.Equal(t, MirasimActionRequestScoped,
		MirasimClassifyStatus(http.StatusBadRequest, []byte(`{"error":{"message":"unsupported beta header"}}`)))

	// And any memo of that 400 is keyed on the headers too: same session, same
	// body, different headers must never collide.
	require.NotEqual(t,
		MirasimRequestMemoKey("session_abc", body, withBeta),
		MirasimRequestMemoKey("session_abc", body, withoutBeta),
	)
	require.Equal(t,
		MirasimRequestMemoKey("session_abc", body, withBeta),
		MirasimRequestMemoKey("session_abc", body, withBeta.Clone()),
	)
}

func TestMirasim401RefreshesCredentialWithoutDisablingAccount(t *testing.T) {
	// [[cov:SC:401-expired]]
	ctx := context.Background()
	svc, repo, account := mirasimServiceWithStub()
	spy := &mirasimTokenCacheInvalidatorSpy{}
	svc.SetTokenCacheInvalidator(spy)

	shouldDisable := svc.HandleUpstreamError(ctx, account, http.StatusUnauthorized, http.Header{},
		[]byte(`{"error":{"type":"authentication_error","message":"token expired"}}`), "claude-opus-4-6")

	// The stale credential is dropped so the next attempt re-resolves it...
	require.Equal(t, []int64{account.ID}, spy.invalidated)
	// ...and the account survives: no disable, no error status, still schedulable
	// (a mirasim account is type=apikey, which the shared 401 path would have
	// sent straight to SetError).
	require.False(t, shouldDisable)
	require.Empty(t, repo.callsOf("SetError"))
	require.Empty(t, repo.callsOf("SetTempUnschedulable"))
	require.Equal(t, StatusActive, account.Status)
	require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-opus-4-6"))
}

func TestMirasim402StopsSchedulingSoNextRequestIsInterceptedLocally(t *testing.T) {
	// [[cov:SC:402-nobalance]]
	ctx := context.Background()
	svc, repo, account := mirasimServiceWithStub()

	shouldDisable := svc.HandleUpstreamError(ctx, account, http.StatusPaymentRequired, http.Header{},
		[]byte(`{"error":{"message":"insufficient balance"}}`), "claude-opus-4-6")

	require.True(t, shouldDisable)
	require.Len(t, repo.callsOf("SetError"), 1)
	// Scheduling now stops locally: the next request is rejected by
	// IsSchedulableForModelWithContext before any upstream call is built.
	require.False(t, account.IsSchedulable())
	require.False(t, account.IsSchedulableForModelWithContext(ctx, "claude-opus-4-6"))
	require.False(t, account.IsSchedulableForModelWithContext(ctx, "claude-fable-5"))
}

func TestMirasim499ClientCancelDoesNotRetryOnAnotherAccount(t *testing.T) {
	// [[cov:SC:499-cancel]]
	ctx := context.Background()
	svc, repo, account := mirasimServiceWithStub()

	// 499 is the downstream client hanging up. The gateway must not spend
	// another account on a response nobody is waiting for.
	gateway := &GatewayService{}
	require.False(t, gateway.shouldFailoverUpstreamError(statusMirasimClientClosedRequest))

	shouldDisable := svc.HandleUpstreamError(ctx, account, statusMirasimClientClosedRequest,
		http.Header{}, nil, "claude-opus-4-6")

	require.False(t, shouldDisable)
	require.Empty(t, repo.calls, "a cancelled request must not write account state")
	require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-opus-4-6"))
}

func TestExistingChannelsUnchangedByMirasimBranch(t *testing.T) {
	// [[cov:RG:existing-channels-unchanged]]
	ctx := context.Background()

	// 1. The mirasim branch is unreachable for a plain anthropic account.
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusTooManyRequests, http.StatusServiceUnavailable, statusMirasimClientClosedRequest} {
		plain := plainAnthropicTestAccount()
		svc := &RateLimitService{accountRepo: &mirasimAccountRepoStub{bound: plain}}
		handled, _ := svc.handleMirasimUpstreamError(ctx, plain, status, http.Header{}, nil)
		require.Falsef(t, handled, "status %d must stay on the shared path", status)
	}

	// 2. Cooldown duration for an existing anthropic 429 is untouched: the 5h
	//    window still lands on the account scalar at the upstream's own reset.
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	plain := plainAnthropicTestAccount()
	repo := &mirasimAccountRepoStub{bound: plain}
	svc := &RateLimitService{accountRepo: repo}
	svc.HandleUpstreamError(ctx, plain, http.StatusTooManyRequests,
		mirasimWindowRejectedHeaders("5h", reset), nil, "claude-opus-4-6")
	rateLimited := repo.callsOf("SetRateLimited")
	require.Len(t, rateLimited, 1)
	require.Equal(t, reset.Unix(), rateLimited[0].resetAt.Unix())
	require.Empty(t, repo.callsOf("SetModelRateLimit"))

	// 3. Disable condition for an existing anthropic 403 is untouched.
	banned := plainAnthropicTestAccount()
	bannedRepo := &mirasimAccountRepoStub{bound: banned}
	bannedSvc := &RateLimitService{accountRepo: bannedRepo}
	require.True(t, bannedSvc.HandleUpstreamError(ctx, banned, http.StatusForbidden,
		http.Header{}, []byte(`{"error":{"message":"forbidden"}}`), "claude-opus-4-6"))
	require.Len(t, bannedRepo.callsOf("SetError"), 1)
	require.Equal(t, StatusError, banned.Status)

	// 4. Scope derivation for an existing anthropic account gains no mirasim key:
	//    Fable still resolves to the legacy family scope and nothing else.
	fableKeys := plainAnthropicTestAccount().modelRateLimitKeysForRequest(ctx, "claude-fable-5[1m]")
	require.Contains(t, fableKeys, anthropicFableRateLimitKey)
	require.NotContains(t, fableKeys, mirasimFable7dRateLimitKey)
	require.NotContains(t, fableKeys, mirasimClaude7dRateLimitKey)
	opusKeys := plainAnthropicTestAccount().modelRateLimitKeysForRequest(ctx, "claude-opus-4-6")
	require.Equal(t, []string{"claude-opus-4-6"}, opusKeys)
}
