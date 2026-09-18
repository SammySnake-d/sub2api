package service

import (
	"context"
	"math"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type limitField struct {
	Kind   string  `json:"kind"`
	Number float64 `json:"number"`
}
type requestLimits struct {
	MaxTokens           limitField `json:"max_tokens"`
	MaxOutputTokens     limitField `json:"max_output_tokens"`
	MaxCompletionTokens limitField `json:"max_completion_tokens"`
	Stream              string     `json:"stream"`
}
type requestLimitTrace struct {
	Inbound  requestLimits
	APIKeyID int64
}
type requestLimitTraceKey struct{}

// Only typed budget fields are retained, never a body, prompt, credential or
// arbitrary string. In particular, a malformed string limit is logged by TYPE.
func parseRequestLimits(body []byte) requestLimits {
	field := func(key string) limitField {
		v := gjson.GetBytes(body, key)
		if !v.Exists() {
			return limitField{Kind: "absent"}
		}
		switch v.Type {
		case gjson.Number:
			n := v.Float()
			if math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) > 1<<53 {
				return limitField{Kind: "out_of_range"}
			}
			return limitField{Kind: "number", Number: n}
		case gjson.Null:
			return limitField{Kind: "null"}
		case gjson.String:
			return limitField{Kind: "string"}
		case gjson.True, gjson.False:
			return limitField{Kind: "boolean"}
		default:
			return limitField{Kind: "structured"}
		}
	}
	stream := "absent"
	v := gjson.GetBytes(body, "stream")
	if v.Exists() {
		switch v.Type {
		case gjson.True:
			stream = "true"
		case gjson.False:
			stream = "false"
		default:
			stream = "invalid"
		}
	}
	return requestLimits{field("max_tokens"), field("max_output_tokens"), field("max_completion_tokens"), stream}
}

// BeginRequestLimitTrace snapshots the raw ingress JSON before parsing,
// conversion, group policy, model mapping or provider compatibility rewrites.
// Its caller owns the opt-in switch. Invalid JSON is deliberately not logged.
func BeginRequestLimitTrace(ctx context.Context, body []byte, apiKeyID int64) context.Context {
	if !gjson.ValidBytes(body) {
		return ctx
	}
	if _, exists := ctx.Value(requestLimitTraceKey{}).(requestLimitTrace); exists {
		return ctx
	}
	trace := requestLimitTrace{Inbound: parseRequestLimits(body), APIKeyID: apiKeyID}
	ctx = context.WithValue(ctx, requestLimitTraceKey{}, trace)
	RecordRequestLimitTrace(ctx, "ingress", body, 0)
	return ctx
}

// RecordRequestLimitTrace is called again at Forward entry and after the
// transport signer has finalized the actual wire body. It has no request writes.
func RecordRequestLimitTrace(ctx context.Context, stage string, body []byte, accountID int64) {
	trace, enabled := ctx.Value(requestLimitTraceKey{}).(requestLimitTrace)
	if !enabled {
		return
	}
	current := parseRequestLimits(body)
	logger.FromContext(ctx).Info("gateway.request_limit_trace",
		zap.String("stage", stage), zap.Int64("api_key_id", trace.APIKeyID), zap.Int64("account_id", accountID),
		zap.Any("inbound_limits", trace.Inbound), zap.Any("current_limits", current),
		zap.Bool("limits_changed", current != trace.Inbound), zap.Bool(logger.OpsSystemLogSkipField, true))
}
