package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestGatewayTimingIncludesRecoveryAndPreservesUsage(t *testing.T) {
	start := time.Now()
	ctx := context.WithValue(context.Background(), ctxkey.RequestStartedAt, start)
	first := 3000
	r := &ForwardResult{Duration: 127 * time.Second, FirstTokenMs: &first, Usage: ClaudeUsage{OutputTokens: 8672}}
	recordGatewayEndToEndTiming(ctx, start.Add(74*time.Minute), r)
	require.Equal(t, 74*time.Minute+127*time.Second, r.Duration)
	require.Equal(t, 4443000, *r.FirstTokenMs)
	require.Equal(t, 8672, r.Usage.OutputTokens)
	require.Equal(t, 3000, first)
	r = &ForwardResult{Duration: time.Second}
	recordGatewayEndToEndTiming(context.Background(), start, r)
	require.Equal(t, time.Second, r.Duration)
}
