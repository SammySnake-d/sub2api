package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func recoveryAccount(id int64) *service.Account {
	return &service.Account{ID: id, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"provider": "mirasim", "pool_mode": true, "pool_mode_retry_count": float64(3)}}
}

func TestMirasimRecovery_RoundPreservesPermanentExclusions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := (&GatewayHandler{maxAccountSwitches: 1}).newGatewayFailoverState(true)
		gateway := &mockTempUnscheduler{}
		for id, status := range []int{503, 502, 403, 429} {
			err := &service.UpstreamFailoverError{StatusCode: status, RetryableOnSameAccount: true}
			require.Equal(t, FailoverContinue, fs.HandleAccountFailover(context.Background(), gateway, recoveryAccount(int64(id+1)), err))
		}
		require.Equal(t, FailoverContinue, fs.RecordProfitVeto(2))
		start := time.Now()
		require.Equal(t, FailoverContinue, fs.HandleSelectionExhausted(context.Background()))
		require.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond)
		require.Less(t, time.Since(start), time.Second)
		require.NotContains(t, fs.FailedAccountIDs, int64(1))
		for _, id := range []int64{2, 3, 4} {
			require.Contains(t, fs.FailedAccountIDs, id)
		}
		require.Empty(t, fs.SameAccountRetryCount, "untried accounts take priority over retrying one account")
		require.Empty(t, gateway.calls, "capacity recovery must not ban healthy accounts")
		require.True(t, fs.ForceCacheBilling, "sticky failover keeps cache billing")
	})
}

func TestMirasimRecovery_DeadlineSurvivesEmptySelections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := (&GatewayHandler{maxAccountSwitches: 1}).newGatewayFailoverState(false)
		fs.recoveryWindow = 3 * time.Second
		start := time.Now()
		require.Equal(t, FailoverContinue, fs.HandleAccountFailover(context.Background(), &mockTempUnscheduler{}, recoveryAccount(1), &service.UpstreamFailoverError{StatusCode: 503}))
		// The account can disappear from a fresh snapshot after the first round.
		// Empty selections must neither bypass backoff nor reset the deadline.
		for i := 0; i < 20; i++ {
			action := fs.HandleSelectionExhausted(context.Background())
			if action == FailoverExhausted {
				break
			}
			require.Equal(t, FailoverContinue, action)
		}
		require.True(t, fs.RecoveryExpired())
		require.Equal(t, 3*time.Second, time.Since(start))
		require.Equal(t, FailoverExhausted, fs.HandleAccountFailover(context.Background(), &mockTempUnscheduler{}, recoveryAccount(2), &service.UpstreamFailoverError{StatusCode: 502}))
		require.Equal(t, 1, fs.SwitchCount)
	})
}

func TestMirasimRecovery_CancelDuringRoundBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := (&GatewayHandler{}).newGatewayFailoverState(false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.Equal(t, FailoverContinue, fs.HandleAccountFailover(ctx, &mockTempUnscheduler{}, recoveryAccount(1), &service.UpstreamFailoverError{StatusCode: 503}))
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()
		require.Equal(t, FailoverCanceled, fs.HandleSelectionExhausted(ctx))
		require.Equal(t, 50*time.Millisecond, time.Since(start))
		require.Contains(t, fs.FailedAccountIDs, int64(1))
	})
}

func TestMirasimRecovery_OptInAndClassification(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 429, 500, 502, 503, 504, 529} {
		want := status == 502 || status == 503 || status == 504 || status == 529
		err := &service.UpstreamFailoverError{StatusCode: status}
		require.Equal(t, want, mirasimPoolRetryable(recoveryAccount(1), err))
		require.False(t, mirasimPoolRetryable(&service.Account{Platform: service.PlatformAnthropic}, err))
		err.NextAccountAction = service.NextAccountStop
		require.False(t, mirasimPoolRetryable(recoveryAccount(1), err))
	}
	for _, tc := range []struct {
		name    string
		seconds int
		want    FailoverAction
	}{
		{"disabled", 0, FailoverExhausted}, {"enabled", 20, FailoverContinue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := config.DefaultMirasimRecoveryConfig()
			policy.MirasimFailoverEnabled = tc.seconds > 0
			policy.MirasimFailoverWindowSeconds = tc.seconds
			h := &GatewayHandler{cfg: &config.Config{Gateway: policy}}
			fs := h.newGatewayFailoverState(false)
			require.Equal(t, tc.want, fs.HandleAccountFailover(context.Background(), &mockTempUnscheduler{}, recoveryAccount(1), &service.UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable}))
		})
	}
}

func TestMirasimRecovery_UnlimitedWindowAndConfiguredBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		policy := config.DefaultMirasimRecoveryConfig()
		policy.MirasimBackoffInitialMS = 2000
		policy.MirasimBackoffMaxMS = 4000
		policy.MirasimBackoffJitter = 0
		fs := (&GatewayHandler{cfg: &config.Config{Gateway: policy}}).newGatewayFailoverState(false)
		ctx := context.Background()
		require.Equal(t, FailoverContinue, fs.HandleAccountFailover(ctx, &mockTempUnscheduler{}, recoveryAccount(1), &service.UpstreamFailoverError{StatusCode: 503}))
		start := time.Now()
		for i := 0; i < 40; i++ {
			require.Equal(t, FailoverContinue, fs.HandleSelectionExhausted(ctx))
		}
		require.Equal(t, 158*time.Second, time.Since(start))
		require.False(t, fs.RecoveryExpired(), "zero window must not recreate the old 90-second cutoff")
	})
}

func TestMirasimRecovery_InitialCooldownWaitsWithoutRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := (&GatewayHandler{}).newGatewayFailoverState(false)
		reset := time.Now().Add(30 * time.Second)
		start := time.Now()
		for time.Now().Before(reset) {
			action, handled := fs.HandleCooldownSelection(context.Background(), &service.MirasimCooldownError{RetryAt: reset})
			require.True(t, handled)
			require.Equal(t, FailoverContinue, action)
		}
		require.Equal(t, 30*time.Second, time.Since(start))
		require.Zero(t, fs.SwitchCount, "waiting must not send requests to cooled accounts")
	})
}

func TestMirasimRecovery_StreamWaitSendsOnlyComments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		fs := (&GatewayHandler{}).newGatewayFailoverState(false)
		started := false
		fs.keepRecoveryStreamAlive(c, true, &started)
		require.True(t, fs.recoveryWait(context.Background(), 21*time.Second))
		require.True(t, started)
		require.Equal(t, "text/event-stream", c.Writer.Header().Get("Content-Type"))
		require.Greater(t, c.Writer.Size(), 0)
	})
}
