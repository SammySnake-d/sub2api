//go:build unit

package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

type probeCacheStub struct {
	GatewayCache
	claimed bool
	calls   int
	release int
}

func (p *probeCacheStub) AcquireMirasimProbe(context.Context, string, time.Duration, time.Duration) (string, error) {
	p.calls++
	if p.claimed {
		return "", nil
	}
	p.claimed = true
	return "lease", nil
}
func (p *probeCacheStub) RenewMirasimProbe(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (p *probeCacheStub) ReleaseMirasimProbe(context.Context, string, string) error {
	p.release++
	return nil
}

func TestMirasimProbeRecoversHourParkWithoutUnlockingQuotaOrOtherAccounts(t *testing.T) {
	now := time.Now()
	model := mirasimCapacityTestModel
	scope := mirasimCapacityRateLimitScope(model)
	accounts := []Account{mirasimCapacityParkedAccount(1, ""), mirasimCapacityParkedAccount(2, ""), mirasimCapacityParkedAccount(3, "")}
	for i := range accounts {
		setAccountModelRateLimitSnapshot(&accounts[i], scope, now.Add(time.Hour), mirasimCapacityParkReason, now.Add(-time.Duration(60+i)*time.Second))
	}
	setAccountModelRateLimitSnapshot(&accounts[2], mirasimClaude7dRateLimitKey, now.Add(24*time.Hour), "quota", now)
	svc, ctx, gid := mirasimCapacityGatewayFixture(t, accounts)
	cache := &probeCacheStub{GatewayCache: svc.cache}
	svc.cache = cache
	svc.cfg.Gateway = config.DefaultMirasimRecoveryConfig()
	svc.cfg.Gateway.Scheduling.LoadBatchEnabled = true
	selected, err := svc.SelectAccountWithLoadAwareness(ctx, &gid, "", model, nil, "", 0)
	require.NoError(t, err)
	require.Equal(t, int64(2), selected.Account.ID, "oldest eligible failure, not priority1 or quota3")
	require.True(t, selected.Account.isRateLimitActiveForKey(scope), "probe admission never clears persisted history")
	selected.ReleaseFunc()
	selected.ReleaseFunc()
	require.Equal(t, 1, cache.release)
	second, err := svc.SelectAccountWithLoadAwareness(ctx, &gid, "", model, nil, "", 0)
	var cooling *MirasimCooldownError
	require.ErrorAs(t, err, &cooling)
	require.Nil(t, second, "another request cannot multiply recovery probes")
	require.LessOrEqual(t, time.Until(cooling.RetryAt), 5*time.Second)
}

func TestMirasimProbeDoesNotRetryFreshFailureOrUnsupportedAccount(t *testing.T) {
	now := time.Now()
	model := mirasimCapacityTestModel
	scope := mirasimCapacityRateLimitScope(model)
	fresh := mirasimCapacityParkedAccount(1, model)
	unsupported := mirasimCapacityParkedAccount(2, model)
	unsupported.Credentials["model_mapping"] = map[string]any{"other": "other"}
	setAccountModelRateLimitSnapshot(&unsupported, scope, now.Add(time.Hour), mirasimCapacityParkReason, now.Add(-time.Minute))
	svc, ctx, gid := mirasimCapacityGatewayFixture(t, []Account{fresh, unsupported})
	cache := &probeCacheStub{GatewayCache: svc.cache}
	svc.cache = cache
	svc.cfg.Gateway = config.DefaultMirasimRecoveryConfig()
	svc.cfg.Gateway.Scheduling.LoadBatchEnabled = true
	selected, err := svc.SelectAccountWithLoadAwareness(ctx, &gid, "", model, nil, "", 0)
	var cooling *MirasimCooldownError
	require.ErrorAs(t, err, &cooling)
	require.Nil(t, selected)
}
