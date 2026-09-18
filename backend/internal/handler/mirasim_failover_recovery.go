package handler

import (
	"context"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

// newGatewayFailoverState keeps the ordinary count budget until a Mirasim
// transient failure is observed. Recovery then scans the actual eligible pool;
// it does not estimate eligibility by counting unfiltered database accounts.
func (h *GatewayHandler) newGatewayFailoverState(hasBoundSession bool) *FailoverState {
	s := NewFailoverState(h.maxAccountSwitches, hasBoundSession)
	seconds := config.DefaultMirasimFailoverWindowSeconds
	if h.cfg != nil {
		seconds = h.cfg.Gateway.MirasimFailoverWindowSeconds
	}
	s.recoveryWindow = time.Duration(seconds) * time.Second
	return s
}

func mirasimPoolRetryable(account *service.Account, err *service.UpstreamFailoverError) bool {
	if !service.IsMirasimAccount(account) || err == nil || !err.ShouldRetryNextAccount() || err.IsCredentialFailure() {
		return false
	}
	switch err.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	default:
		return false
	}
}

// HandleAccountFailover is used by all three Anthropic-compatible entrypoints.
// Try a different eligible account immediately, even beyond MaxSwitches. Only
// the selector declaring the pool exhausted permits a delayed new round.
func (s *FailoverState) HandleAccountFailover(ctx context.Context, gateway TempUnscheduler, account *service.Account, err *service.UpstreamFailoverError) FailoverAction {
	if ctx.Err() != nil {
		return FailoverCanceled
	}
	retryable := mirasimPoolRetryable(account, err)
	if retryable && s.recoveryWindow > 0 && !s.Recovering() {
		s.recoveryDeadline = time.Now().Add(s.recoveryWindow)
		s.recoveryAccountIDs = make(map[int64]struct{})
	}
	if !s.Recovering() {
		return s.HandleFailoverError(ctx, gateway, account.ID, account.Platform, account.GetPoolModeRetryCount(), err)
	}
	s.LastFailoverErr = err
	if err == nil || !err.ShouldRetryNextAccount() {
		return FailoverExhausted
	}
	s.FailedAccountIDs[account.ID] = struct{}{}
	if retryable {
		s.recoveryAccountIDs[account.ID] = struct{}{}
	} else {
		// Credential/quota/permission failures must not be resurrected next round.
		delete(s.recoveryAccountIDs, account.ID)
	}
	if needForceCacheBilling(s.hasBoundSession, err, false) {
		s.ForceCacheBilling = true
	}
	if s.RecoveryExpired() {
		return FailoverExhausted
	}
	s.SwitchCount++
	logger.FromContext(ctx).Warn("gateway.mirasim_pool_switch",
		zap.Int64("account_id", account.ID), zap.Int("upstream_status", err.StatusCode),
		zap.Int("switch_count", s.SwitchCount), zap.Duration("recovery_remaining", time.Until(s.recoveryDeadline)))
	return FailoverContinue
}

func (s *FailoverState) Recovering() bool { return !s.recoveryDeadline.IsZero() }

// The window bounds admission of further attempts, not the lifetime of a
// successful response. Never cut an accepted stream when this window expires.
func (s *FailoverState) RecoveryExpired() bool {
	return s.Recovering() && !time.Now().Before(s.recoveryDeadline)
}

func (s *FailoverState) retryMirasimPool(ctx context.Context) FailoverAction {
	if ctx.Err() != nil {
		return FailoverCanceled
	}
	for id := range s.profitVetoedAccountIDs {
		delete(s.recoveryAccountIDs, id)
	}
	if s.RecoveryExpired() || len(s.recoveryAccountIDs) == 0 {
		return FailoverExhausted
	}
	// Exponential round backoff with equal jitter: 0.5–1s, 1–2s, 2–4s,
	// then 4–8s. New accounts within a round never incur this delay.
	capDelay := time.Second
	for i := 0; i < s.recoveryRound && capDelay < 8*time.Second; i++ {
		capDelay *= 2
	}
	delay := capDelay/2 + time.Duration(rand.Int64N(int64(capDelay/2)))
	if remaining := time.Until(s.recoveryDeadline); delay > remaining {
		delay = remaining
	}
	logger.FromContext(ctx).Warn("gateway.mirasim_pool_round_backoff",
		zap.Int("round", s.recoveryRound+1), zap.Int("retryable_accounts", len(s.recoveryAccountIDs)),
		zap.Int("switch_count", s.SwitchCount), zap.Duration("retry_delay", delay))
	if !sleepWithContext(ctx, delay) {
		return FailoverCanceled
	}
	if s.RecoveryExpired() {
		return FailoverExhausted
	}
	s.recoveryRound++
	for id := range s.recoveryAccountIDs {
		delete(s.FailedAccountIDs, id)
	}
	return FailoverContinue
}
