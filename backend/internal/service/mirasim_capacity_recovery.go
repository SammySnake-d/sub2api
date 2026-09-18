package service

import (
	"context"
	"log/slog"
	"time"
)

// Only the exact cooldown generation observed by this attempt may be cleared.
// The repository implements the compare-and-delete atomically; a concurrent
// newer failure must survive this successful response.
type mirasimCapacityRecoveryRepository interface {
	ClearMirasimCapacityIfUnchanged(context.Context, int64, string, string, string) (bool, error)
}

func (s *GatewayService) RecordMirasimModelRecovery(ctx context.Context, account *Account, requestedModel string) {
	if s == nil || !IsMirasimAccount(account) {
		return
	}
	repo, ok := s.accountRepo.(mirasimCapacityRecoveryRepository)
	if !ok {
		return
	}
	scope := mirasimCapacityRateLimitScope(account.GetMappedModel(requestedModel))
	limited := account.modelRateLimitTimestamp(scope, "rate_limited_at")
	reset := account.modelRateLimitTimestamp(scope, "rate_limit_reset_at")
	if limited == nil || reset == nil {
		return
	}
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := repo.ClearMirasimCapacityIfUnchanged(stateCtx, account.ID, scope, limited.UTC().Format(time.RFC3339), reset.UTC().Format(time.RFC3339)); err != nil {
		slog.Warn("mirasim_capacity_recovery_clear_failed", "account_id", account.ID, "scope", scope, "error", err)
	}
}
