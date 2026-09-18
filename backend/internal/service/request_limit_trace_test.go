package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRequestLimitTraceCorrelatesOriginalAndWireWithoutMutatingBody(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "trace-fixture")))
	original := []byte(`{"model":"claude-opus-5","max_tokens":1,"stream":false,"messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`)
	wire := []byte(`{"model":"claude-opus-5","max_tokens":2,"stream":false,"messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`)
	before := string(original)
	ctx = BeginRequestLimitTrace(ctx, original, 11)
	RecordRequestLimitTrace(ctx, "forward", original, 7)
	RecordRequestLimitTrace(ctx, "egress", wire, 7)
	require.Equal(t, before, string(original))
	require.Equal(t, 3, logs.Len())
	for i, e := range logs.All() {
		m := e.ContextMap()
		require.Equal(t, "trace-fixture", m["request_id"])
		require.Equal(t, i == 2, m["limits_changed"])
		encoded, err := json.Marshal(m)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "PRIVATE_PROMPT")
		require.NotContains(t, string(encoded), "messages")
	}
	require.Equal(t, float64(1), ctx.Value(requestLimitTraceKey{}).(requestLimitTrace).Inbound.MaxTokens.Number)
	require.Equal(t, ctx, BeginRequestLimitTrace(ctx, wire, 11), "retries must not replace ingress snapshot")
}
func TestRequestLimitTracePreservesMissingZeroFractionAndAllBudgets(t *testing.T) {
	cases := []struct {
		body, kind string
		number     float64
	}{
		{`{}`, "absent", 0}, {`{"max_tokens":null}`, "null", 0}, {`{"max_tokens":0}`, "number", 0},
		{`{"max_tokens":1.5}`, "number", 1.5}, {`{"max_tokens":"PRIVATE_TOKEN"}`, "string", 0},
		{`{"max_tokens":1e100}`, "out_of_range", 0},
	}
	for _, tc := range cases {
		v := parseRequestLimits([]byte(tc.body))
		require.Equal(t, tc.kind, v.MaxTokens.Kind)
		require.Equal(t, tc.number, v.MaxTokens.Number)
		raw, _ := json.Marshal(v)
		require.NotContains(t, string(raw), "PRIVATE_TOKEN")
	}
	fields := parseRequestLimits([]byte(`{"max_output_tokens":4096,"max_completion_tokens":2048,"stream":true}`))
	require.Equal(t, float64(4096), fields.MaxOutputTokens.Number)
	require.Equal(t, float64(2048), fields.MaxCompletionTokens.Number)
	require.Equal(t, "true", fields.Stream)
	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	RecordRequestLimitTrace(ctx, "egress", []byte(`{"max_tokens":1}`), 7)
	require.Zero(t, logs.Len())
	require.Equal(t, ctx, BeginRequestLimitTrace(ctx, []byte(`{broken`), 1))
	require.Zero(t, logs.Len())
}
