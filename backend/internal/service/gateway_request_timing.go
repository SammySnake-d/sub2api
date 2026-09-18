package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Freeze end-to-end timing before handing the result to the async billing worker.
// Token amounts, prices and idempotency identities are deliberately untouched.
func recordGatewayEndToEndTiming(ctx context.Context, attemptStart time.Time, result *ForwardResult) {
	if result == nil {
		return
	}
	ingress, ok := ctx.Value(ctxkey.RequestStartedAt).(time.Time)
	if !ok || ingress.IsZero() || ingress.After(attemptStart) {
		return
	}
	preceding := attemptStart.Sub(ingress)
	attemptDuration := result.Duration
	result.Duration += preceding
	var firstOutput any
	if result.FirstTokenMs != nil {
		v := *result.FirstTokenMs + int(preceding.Milliseconds())
		result.FirstTokenMs = &v
		firstOutput = v
	}
	logger.FromContext(ctx).Info("gateway.request_timing",
		zap.Int64("total_duration_ms", result.Duration.Milliseconds()),
		zap.Int64("final_attempt_ms", attemptDuration.Milliseconds()),
		zap.Int64("pre_attempt_ms", preceding.Milliseconds()),
		zap.Any("first_output_ms", firstOutput),
		zap.Bool("client_disconnected", result.ClientDisconnect))
}
