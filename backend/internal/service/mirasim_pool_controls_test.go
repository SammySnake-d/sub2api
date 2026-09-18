package service

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
	"time"
)

type poolControlCache struct {
	SessionLimitCache
	registrations, releases int
}

func (c *poolControlCache) RegisterSession(context.Context, int64, string, int, time.Duration) (bool, error) {
	c.registrations++
	return false, nil
}
func (c *poolControlCache) UnregisterSession(context.Context, int64, string) error {
	c.releases++
	return nil
}
func (c *poolControlCache) GetWindowCost(context.Context, int64) (float64, bool, error) {
	return 99, true, nil
}

type poolRPMCache struct{ RPMCache }

func (c *poolRPMCache) GetRPM(context.Context, int64) (int, error) { return 100, nil }
func poolControlAccount() *Account {
	return &Account{ID: 9001, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"provider": "mirasim"}, Extra: map[string]any{"base_rpm": 10, "max_sessions": 2, "window_cost_limit": 50.0}}
}
func TestMirasimPoolControlsNoLongerSkippedByAPIKeyType(t *testing.T) {
	a := poolControlAccount()
	cache := &poolControlCache{}
	s := &GatewayService{sessionLimitCache: cache, rpmCache: &poolRPMCache{}}
	require.False(t, s.isAccountSchedulableForRPM(context.Background(), a, false), "Mira APIKey must honor configured RPM")
	require.False(t, s.isAccountSchedulableForWindowCost(context.Background(), a, false), "Mira must honor configured spending window")
	require.False(t, s.checkAndRegisterSession(context.Background(), a, "session"), "Mira must honor configured active sessions")
	require.Equal(t, 1, cache.registrations)
	s.ReleaseAccountSession(context.Background(), a, "session")
	require.Equal(t, 1, cache.releases)
}
func TestOrdinaryAPIKeyStillOutsideOAuthPoolControls(t *testing.T) {
	a := poolControlAccount()
	a.Credentials = map[string]any{}
	s := &GatewayService{}
	require.True(t, s.isAccountSchedulableForRPM(context.Background(), a, false))
	require.True(t, s.isAccountSchedulableForWindowCost(context.Background(), a, false))
	require.True(t, s.checkAndRegisterSession(context.Background(), a, "session"))
}

type attemptBudgetCache struct {
	RPMCache
	admitted bool
	calls    int
	err      error
}

func (c *attemptBudgetCache) TryAcquireRPM(context.Context, int64, int) (bool, error) {
	c.calls++
	return c.admitted, c.err
}
func TestMirasimAttemptBudgetBlocksBeforeTransportAndNeverParksAccount(t *testing.T) {
	a := poolControlAccount()
	cache := &attemptBudgetCache{}
	s := &GatewayService{rpmCache: cache}
	err := s.admitMirasimAttempt(context.Background(), a)
	var fail *UpstreamFailoverError
	require.ErrorAs(t, err, &fail)
	require.Equal(t, 429, fail.StatusCode)
	require.True(t, fail.RequestScopedTransient)
	require.True(t, fail.ShouldRetryNextAccount())
	require.Equal(t, 1, cache.calls)
	cache.admitted = true
	require.NoError(t, s.admitMirasimAttempt(context.Background(), a))
	require.Equal(t, 2, cache.calls)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, s.admitMirasimAttempt(ctx, a), context.Canceled)
	require.Equal(t, 2, cache.calls)
	s.rpmCache = nil
	require.Error(t, s.admitMirasimAttempt(context.Background(), a), "missing distributed budget fails closed")
	a.Extra["base_rpm"] = 0
	require.NoError(t, s.admitMirasimAttempt(context.Background(), a))
}
func TestMirasimWindowAndSessionDefaultsAreExplicit(t *testing.T) {
	a := poolControlAccount()
	require.False(t, a.IsAnthropicOAuthOrSetupToken(), "do not reclassify credentials")
	now := time.Now()
	require.WithinDuration(t, now.Add(-5*time.Hour), a.GetCurrentWindowStartTime(), time.Second)
	start, end := now.Add(-time.Hour), now.Add(4*time.Hour)
	a.SessionWindowStart = &start
	a.SessionWindowEnd = &end
	require.Equal(t, start, a.GetCurrentWindowStartTime())
	a.Extra["window_cost_sticky_reserve"] = 0
	require.Zero(t, a.GetWindowCostStickyReserve())
	require.Equal(t, WindowCostNotSchedulable, a.CheckWindowCostSchedulability(50))
	a.Extra["rpm_strategy"] = "sticky_exempt"
	require.Equal(t, WindowCostNotSchedulable, a.CheckRPMSchedulability(10), "Mira attempt ceiling has no sticky exemption")
	s := &GatewayService{}
	require.False(t, s.checkAndRegisterSession(context.Background(), a, "session"))
	require.False(t, s.isAccountSchedulableForWindowCost(context.Background(), a, false))
}

func TestMirasimAttemptBudgetDenialNeverCallsUpstream(t *testing.T) {
	calls := 0
	upstream := &firstOutputUpstream{call: func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") }}
	s, a, c := firstOutputFixture(t, upstream)
	a.Extra = map[string]any{"base_rpm": 1}
	s.rpmCache = &attemptBudgetCache{}
	_, err := s.doMirasimAwareUpstream(context.Background(), c, a, c.Request, "", "claude-opus-5", false)
	var fail *UpstreamFailoverError
	require.ErrorAs(t, err, &fail)
	require.Zero(t, calls)
}
