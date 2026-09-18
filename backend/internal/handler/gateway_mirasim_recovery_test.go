//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type mirasimRecoveryUpstream struct {
	failedAttempts int
	accounts       []int64
	usage          []*service.UsageLog
}

func (u *mirasimRecoveryUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.accounts = append(u.accounts, accountID)
	requestBody, _ := io.ReadAll(req.Body)
	status := http.StatusOK
	contentType := "application/json"
	body := `{"id":"msg_recovered","type":"message","role":"assistant","model":"claude-fable-5-1","content":[{"type":"text","text":"recovered"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	if len(u.accounts) <= u.failedAttempts {
		status = http.StatusServiceUnavailable
		body = `{"error":{"type":"overloaded_error","code":"service_capacity_overloaded","message":"model capacity exhausted"}}`
	}
	if status == http.StatusOK && gjson.GetBytes(requestBody, "stream").Bool() {
		contentType = "text/event-stream"
		body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_recovered\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-fable-5-1\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (u *mirasimRecoveryUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

type mirasimRecoveryUsageRepo struct {
	service.UsageLogRepository
	upstream *mirasimRecoveryUpstream
}

func (r *mirasimRecoveryUsageRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	r.upstream.usage = append(r.upstream.usage, log)
	return true, nil
}

func newMirasimRecoveryHandler(t *testing.T, poolSize int, upstream *mirasimRecoveryUpstream) (*GatewayHandler, *service.APIKey) {
	t.Helper()
	group := &service.Group{ID: 9900, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	accounts := make([]*service.Account, poolSize)
	for i := range accounts {
		id := int64(i + 1)
		accounts[i] = &service.Account{
			ID: id, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: i,
			Credentials:   map[string]any{"provider": "mirasim", "api_key": "test-key", "pool_mode": true, "pool_mode_retry_count": float64(0)},
			Extra:         map[string]any{"anthropic_passthrough": true},
			AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: group.ID}},
		}
	}
	h, cleanup := newTestGatewayHandler(t, group, accounts)
	t.Cleanup(cleanup)
	h.maxAccountSwitches = 10
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	h.gatewayService = service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, &mirasimRecoveryUsageRepo{upstream: upstream}, nil, nil, nil, nil, nil, cfg, snapshot,
		nil, service.NewBillingService(cfg, nil), nil, nil, nil, upstream, service.NewDeferredService(nil, nil, time.Minute), nil, nil, nil, nil, nil,
		&service.TLSFingerprintProfileService{}, nil, nil, nil, nil, nil,
	)
	key := &service.APIKey{ID: 9901, UserID: 9902, GroupID: &group.ID, Group: group, Status: service.StatusActive,
		User: &service.User{ID: 9902, Concurrency: 10, Balance: 100}}
	return h, key
}

func TestMirasimRecoveryHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	endpoints := []struct{ path, body string }{
		{"/v1/messages", `{"model":"claude-fable-5-1","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/responses", `{"model":"claude-fable-5-1","input":"hello"}`},
		{"/v1/chat/completions", `{"model":"claude-fable-5-1","messages":[{"role":"user","content":"hello"}]}`},
	}
	scenarios := []struct {
		name                 string
		pool, failed, window int
		minWait, maxWait     time.Duration
		success              bool
	}{
		{"healthy_beyond_ten", 13, 12, 90, 0, time.Second, true},
		{"recovers_after_two_full_rounds", 3, 6, 90, 1500 * time.Millisecond, 3 * time.Second, true},
		{"all_down_stops_at_window", 2, 10000, 2, 2 * time.Second, 2*time.Second + time.Nanosecond, false},
	}
	for _, endpoint := range endpoints {
		for _, scenario := range scenarios {
			for _, stream := range []bool{false, true} {
				name := endpoint.path + "/" + scenario.name + "/json"
				if stream {
					name = endpoint.path + "/" + scenario.name + "/stream"
				}
				t.Run(name, func(t *testing.T) {
					upstream := &mirasimRecoveryUpstream{failedAttempts: scenario.failed}
					h, key := newMirasimRecoveryHandler(t, scenario.pool, upstream)
					h.cfg = &config.Config{RunMode: config.RunModeSimple, Gateway: config.GatewayConfig{MirasimFailoverWindowSeconds: scenario.window}}
					synctest.Test(t, func(t *testing.T) {
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						ctx := context.WithValue(context.Background(), ctxkey.Group, key.Group)
						body := strings.TrimSuffix(endpoint.body, "}") + `,"stream":` + strconv.FormatBool(stream) + `}`
						c.Request = httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(body)).WithContext(ctx)
						c.Request.Header.Set("Content-Type", "application/json")
						c.Set(string(middleware.ContextKeyAPIKey), key)
						c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 10})
						start := time.Now()
						switch endpoint.path {
						case "/v1/messages":
							h.Messages(c)
						case "/v1/responses":
							h.Responses(c)
						default:
							h.ChatCompletions(c)
						}
						elapsed := time.Since(start)
						require.GreaterOrEqual(t, elapsed, scenario.minWait)
						require.Less(t, elapsed, scenario.maxWait)
						if scenario.success {
							require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
							require.Contains(t, rec.Body.String(), "recovered")
							require.Len(t, upstream.accounts, scenario.failed+1)
							require.Len(t, upstream.usage, 1, "only the successful attempt records usage")
						} else {
							require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
							require.Empty(t, upstream.usage)
							require.Less(t, len(upstream.accounts), 20, "exhaustion must not spin")
						}
						for offset := 0; offset < len(upstream.accounts); offset += scenario.pool {
							seen := map[int64]bool{}
							for _, id := range upstream.accounts[offset:min(offset+scenario.pool, len(upstream.accounts))] {
								require.False(t, seen[id], "must exhaust untried accounts before starting another round")
								seen[id] = true
							}
						}
					})
				})
			}
		}
	}
}
