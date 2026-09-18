package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// A distributed, rate-limited half-open permit. Missing/unavailable storage is
// fail-closed: ordinary persisted cooldowns continue to apply.
type mirasimProbeCache interface {
	AcquireMirasimProbe(context.Context, string, time.Duration, time.Duration) (string, error)
	RenewMirasimProbe(context.Context, string, string, time.Duration) (bool, error)
	ReleaseMirasimProbe(context.Context, string, string) error
}

type mirasimProbeCandidate struct {
	id       int64
	failedAt time.Time
}
type mirasimProbeAccountKey struct{}

func (s *GatewayService) mirasimProbeInterval() time.Duration {
	if s.cfg == nil || !s.cfg.Gateway.MirasimFailoverEnabled {
		return 0
	}
	return time.Duration(s.cfg.Gateway.MirasimRecoveryProbeIntervalSeconds) * time.Second
}

func (s *GatewayService) tryMirasimCapacityProbe(ctx context.Context, groupID *int64, sessionHash, model string, excluded map[int64]struct{}, metadata string, userID int64, obs *mirasimCapacityParkObserver) *AccountSelectionResult {
	interval := s.mirasimProbeInterval()
	cache, ok := s.cache.(mirasimProbeCache)
	if interval <= 0 || !ok || ctx.Err() != nil {
		return nil
	}
	// Respect the initial memory window even when every account is cooling.
	age := time.Duration(s.cfg.Gateway.MirasimCooldownBaseSeconds) * time.Second
	if age < interval {
		age = interval
	}
	obs.mu.Lock()
	candidates := make([]mirasimProbeCandidate, 0, len(obs.candidates))
	for id, failedAt := range obs.candidates {
		if !failedAt.IsZero() && time.Since(failedAt) >= age {
			candidates = append(candidates, mirasimProbeCandidate{id, failedAt})
		}
	}
	obs.mu.Unlock()
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].failedAt.Equal(candidates[j].failedAt) {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].failedAt.Before(candidates[j].failedAt)
	})
	// Sharing admission rate across groups cannot grant cross-group access.
	// Full selector guards still run; no assumption that all accounts recovered.
	hash := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(model))))
	scope := hex.EncodeToString(hash[:])
	const leaseTTL = 30 * time.Second
	token, err := cache.AcquireMirasimProbe(ctx, scope, interval, leaseTTL)
	if err != nil {
		logger.FromContext(ctx).Warn("gateway.mirasim_probe_unavailable", zap.Error(err))
		return nil
	}
	if token == "" {
		return nil
	}
	releaseLease := func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = cache.ReleaseMirasimProbe(c, scope, token)
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		probeCtx := context.WithValue(withMirasimCapacityParkIgnored(ctx), mirasimProbeAccountKey{}, candidate.id)
		selected, selectErr := s.selectAccountWithLoadAwarenessOnce(probeCtx, groupID, sessionHash, model, excluded, metadata, userID)
		if selectErr != nil || selected == nil || selected.Account == nil {
			continue
		}
		if !selected.Acquired {
			if selected.ReleaseFunc != nil {
				selected.ReleaseFunc()
			}
			s.ReleaseAccountSession(ctx, selected.Account, sessionHash)
			continue // Never occupy the shared probe permit in an account wait queue.
		}
		previousRelease := selected.ReleaseFunc
		stop := make(chan struct{})
		var once sync.Once
		release := func() {
			once.Do(func() {
				close(stop)
				if previousRelease != nil {
					previousRelease()
				}
				releaseLease()
			})
		}
		stopOnCancel := context.AfterFunc(ctx, release)
		selected.ReleaseFunc = func() { stopOnCancel(); release() }
		go func() {
			ticker := time.NewTicker(leaseTTL / 3)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					renewCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					renewed, err := cache.RenewMirasimProbe(renewCtx, scope, token, leaseTTL)
					cancel()
					if err != nil || !renewed {
						logger.FromContext(ctx).Warn("gateway.mirasim_probe_lease_lost", zap.Int64("account_id", selected.Account.ID))
						return
					}
				}
			}
		}()
		logger.FromContext(ctx).Info("gateway.mirasim_capacity_probe", zap.Int64("account_id", selected.Account.ID), zap.String("model", model), zap.Duration("since_failure", time.Since(candidate.failedAt)))
		return selected
	}
	releaseLease()
	return nil
}
