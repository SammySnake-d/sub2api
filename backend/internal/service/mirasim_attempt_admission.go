package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
	"net/http"
)

// Separate optional cache capability keeps ordinary providers and existing
// OAuth soft-limit counters unchanged. The production Redis cache implements it.
type rpmAttemptAdmitter interface {
	TryAcquireRPM(context.Context, int64, int) (bool, error)
}

func (s *GatewayService) admitMirasimAttempt(ctx context.Context, a *Account) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !IsMirasimAccount(a) || a.GetBaseRPM() <= 0 {
		return nil
	}
	cache, ok := s.rpmCache.(rpmAttemptAdmitter)
	admitted := false
	var err error
	if ok {
		admitted, err = cache.TryAcquireRPM(ctx, a.ID, a.GetBaseRPM())
	}
	if admitted && err == nil {
		return nil
	}
	// A configured protection must not silently disappear when Redis is absent.
	logger.FromContext(ctx).Warn("gateway.mirasim_attempt_budget_blocked", zap.Int64("account_id", a.ID), zap.Bool("cache_available", ok && err == nil))
	return &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, RequestScopedTransient: true,
		ResponseHeaders: http.Header{"Retry-After": []string{"1"}},
		ResponseBody:    []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Account local request budget unavailable; retry another eligible account"}}`)}
}
