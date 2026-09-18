//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestMessagesLimitTraceBeforeCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, enabled := range []bool{false, true} {
		upstream := &mirasimRecoveryUpstream{}
		h, key := newMirasimRecoveryHandler(t, 1, upstream)
		h.cfg = &config.Config{RunMode: config.RunModeSimple, Gateway: config.DefaultMirasimRecoveryConfig()}
		h.cfg.Gateway.MirasimRequestLimitTraceEnabled = enabled
		core, logs := observer.New(zap.InfoLevel)
		ctx := logger.IntoContext(context.WithValue(context.Background(), ctxkey.Group, key.Group), zap.New(core).With(zap.String("request_id", "fixture")))
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		body := `{"model":"claude-fable-5-1","max_tokens":1,"stream":false,"messages":[{"role":"user","content":"hello"}]}`
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
		c.Set(string(middleware.ContextKeyAPIKey), key)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 10})
		h.Messages(c)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, []int64{2}, upstream.tokenBudgets, "trace cannot change compatibility behavior")
		trace := logs.FilterMessage("gateway.request_limit_trace").All()
		if !enabled {
			require.Empty(t, trace)
			continue
		}
		require.Len(t, trace, 2, "fake upstream skips signer; deployed signer supplies egress")
		require.Equal(t, "ingress", trace[0].ContextMap()["stage"])
		require.Equal(t, "forward", trace[1].ContextMap()["stage"])
		require.Equal(t, false, trace[1].ContextMap()["limits_changed"])
	}
}
