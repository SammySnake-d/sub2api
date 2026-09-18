package handler

import (
	"context"
	"errors"
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
	policy := config.DefaultMirasimRecoveryConfig()
	if h.cfg != nil {
		policy = h.cfg.Gateway
	}
	s.recoveryEnabled = policy.MirasimFailoverEnabled
	s.recoveryWindow = time.Duration(policy.MirasimFailoverWindowSeconds) * time.Second
	s.recoveryBackoffInitial = time.Duration(policy.MirasimBackoffInitialMS) * time.Millisecond
	s.recoveryBackoffMax = time.Duration(policy.MirasimBackoffMaxMS) * time.Millisecond
	s.recoveryJitter = policy.MirasimBackoffJitter
	s.recoveryWait = sleepWithContext
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
	if retryable && s.recoveryEnabled && !s.Recovering() {
		s.startRecovery()
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
		zap.Int("switch_count", s.SwitchCount))
	return FailoverContinue
}

func (s *FailoverState) Recovering() bool { return s.recoveryStarted }

func (s *FailoverState) startRecovery() {
	s.recoveryStarted = true
	if s.recoveryWindow > 0 {
		s.recoveryDeadline = time.Now().Add(s.recoveryWindow)
	}
	s.recoveryAccountIDs = make(map[int64]struct{})
}

// HandleCooldownSelection waits without sending upstream requests, including
// the first request after a restart when all candidates remain in cooldown.
func (s *FailoverState) HandleCooldownSelection(ctx context.Context, err error) (FailoverAction, bool) {
	var cooling *service.MirasimCooldownError
	if !s.recoveryEnabled || !errors.As(err, &cooling) {
		return FailoverExhausted, false
	}
	if ctx.Err() != nil {
		return FailoverCanceled, true
	}
	if !s.Recovering() {
		s.startRecovery()
	}
	if s.LastFailoverErr == nil {
		s.LastFailoverErr = &service.UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable}
	}
	if s.RecoveryExpired() {
		return FailoverExhausted, true
	}
	delay := time.Until(cooling.RetryAt)
	if delay > s.recoveryBackoffMax {
		delay = s.recoveryBackoffMax
	}
	if delay < 100*time.Millisecond {
		delay = 100 * time.Millisecond
	}
	if !s.recoveryDeadline.IsZero() && delay > time.Until(s.recoveryDeadline) {
		delay = time.Until(s.recoveryDeadline)
	}
	logger.FromContext(ctx).Info("gateway.waiting_capacity", zap.Time("retry_at", cooling.RetryAt), zap.Duration("wait", delay), zap.Int("switch_count", s.SwitchCount))
	if !s.recoveryWait(ctx, delay) {
		return FailoverCanceled, true
	}
	if s.RecoveryExpired() {
		return FailoverExhausted, true
	}
	return FailoverContinue, true
}

// The window bounds admission of further attempts, not the lifetime of a
// successful response. Never cut an accepted stream when this window expires.
func (s *FailoverState) RecoveryExpired() bool {
	return s.Recovering() && !s.recoveryDeadline.IsZero() && !time.Now().Before(s.recoveryDeadline)
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
	capDelay := s.recoveryBackoffInitial
	for i := 0; i < s.recoveryRound && capDelay < s.recoveryBackoffMax; i++ {
		capDelay *= 2
	}
	if capDelay > s.recoveryBackoffMax {
		capDelay = s.recoveryBackoffMax
	}
	delay := time.Duration(float64(capDelay) * (1 - s.recoveryJitter*rand.Float64()))
	if remaining := time.Until(s.recoveryDeadline); !s.recoveryDeadline.IsZero() && delay > remaining {
		delay = remaining
	}
	logger.FromContext(ctx).Warn("gateway.mirasim_pool_round_backoff",
		zap.Int("round", s.recoveryRound+1), zap.Int("retryable_accounts", len(s.recoveryAccountIDs)),
		zap.Int("switch_count", s.SwitchCount), zap.Duration("retry_delay", delay))
	if !s.recoveryWait(ctx, delay) {
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
