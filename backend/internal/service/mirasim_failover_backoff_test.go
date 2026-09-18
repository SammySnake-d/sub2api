package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayForward_MirasimCapacityMarksRequestScopedFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, provider := range []string{"mirasim", ""} {
			name := provider + "/normal"
			if passthrough {
				name = provider + "/passthrough"
			}
			t.Run(name, func(t *testing.T) {
				account := newAnthropicAPIKeyAccountForTest()
				account.Credentials["provider"] = provider
				account.Extra["anthropic_passthrough"] = passthrough
				// Pool mode returns the first 503 to the handler without service retries.
				account.Credentials["pool_mode"] = true
				upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"service_capacity_overloaded","type":"overloaded_error"}}`)),
				}}
				svc := &GatewayService{cfg: &config.Config{}, httpUpstream: upstream, tlsFPProfileService: &TLSFingerprintProfileService{}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				body := []byte(`{"model":"claude-fable-5-1","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
				_, err := svc.Forward(context.Background(), c, account, &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-fable-5-1"})
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, http.StatusServiceUnavailable, failover.StatusCode)
				require.Equal(t, provider == "mirasim", failover.RequestScopedTransient)
				require.Empty(t, rec.Body.String())
			})
		}
	}
}
