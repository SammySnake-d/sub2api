//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// accountTestQuotaRepo 记录额度落账真正写到账号上的东西。
// 断言打在仓储写入上而不是"我调了某个函数"上：后者换个实现就能造绿。
type accountTestQuotaRepo struct {
	mockAccountRepoForGemini
	sessionWindowCalls []accountTestSessionWindowWrite
	extraUpdates       []map[string]any
}

type accountTestSessionWindowWrite struct {
	id     int64
	start  *time.Time
	end    *time.Time
	status string
}

func (r *accountTestQuotaRepo) UpdateSessionWindow(_ context.Context, id int64, start, end *time.Time, status string) error {
	r.sessionWindowCalls = append(r.sessionWindowCalls, accountTestSessionWindowWrite{id: id, start: start, end: end, status: status})
	return nil
}

func (r *accountTestQuotaRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.extraUpdates = append(r.extraUpdates, updates)
	return nil
}

// lastExtraWith 返回最后一次带指定键的 Extra 写入。窗口初始化会先写一次"清空上个窗口"
// 的全 nil，真正的采样值在它之后。
func (r *accountTestQuotaRepo) lastExtraWith(key string) map[string]any {
	for i := len(r.extraUpdates) - 1; i >= 0; i-- {
		if value, ok := r.extraUpdates[i][key]; ok && value != nil {
			return r.extraUpdates[i]
		}
	}
	return nil
}

// failingHTTPUpstream 模拟"上游根本没应答"：传输层直接出错，没有任何响应头可读。
type failingHTTPUpstream struct {
	calls int
}

func (u *failingHTTPUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected Do call")
}

func (u *failingHTTPUpstream) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls++
	return nil, fmt.Errorf("dial tcp: connection refused")
}

// mirasimQuotaHeaders 复刻实测的 mirasim 响应头：只有 5h/7d 的 reset + utilization，
// 没有 -status、没有 7d_oi（ratelimit_service.go:1977 记录的那次真实观测）。
func mirasimQuotaHeaders(resetAt time.Time) http.Header {
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(resetAt.Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(resetAt.Add(72*time.Hour).Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "0.11")
	return headers
}

// newClaudeTestServiceWithQuotaRecorder 用**真实**的 RateLimitService 当落账口，
// 只把仓储换成可观测的假件——落账口本身是转发主干在用的那一个，不另造一份行为。
func newClaudeTestServiceWithQuotaRecorder(account *Account, upstream HTTPUpstream) (*AccountTestService, *accountTestQuotaRepo) {
	repo := &accountTestQuotaRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{account.ID: account},
		},
	}
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: cfg}
	svc.SetRateLimitService(NewRateLimitService(repo, nil, cfg, nil, nil))
	return svc, repo
}

func claudeSSEResponse(status int, headers http.Header, body string) *http.Response {
	resp := newJSONResponse(status, "")
	if headers != nil {
		resp.Header = headers
	}
	resp.Body = io.NopCloser(strings.NewReader(body))
	return resp
}

// 正例：一次成功的账号测试之后，这次调用烧掉的额度确实被记到了账号上。
func TestAccountTestService_RecordsUpstreamQuotaOnSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	account := newMirasimShapedAccount(801, map[string]any{"claude-haiku-4-6": "claude-haiku-4-6"})
	resetAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		claudeSSEResponse(http.StatusOK, mirasimQuotaHeaders(resetAt), "data: {\"type\":\"message_stop\"}\n\n"),
	}}
	svc, repo := newClaudeTestServiceWithQuotaRecorder(account, upstream)

	require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault))

	// [[cov:QR:success-writes-window]] 5h 窗口边界按上游 reset 头建起来。
	require.Len(t, repo.sessionWindowCalls, 1)
	require.Equal(t, account.ID, repo.sessionWindowCalls[0].id)
	require.NotNil(t, repo.sessionWindowCalls[0].end)
	require.Equal(t, resetAt.Unix(), repo.sessionWindowCalls[0].end.Unix())
	require.Equal(t, resetAt.Add(-5*time.Hour).Unix(), repo.sessionWindowCalls[0].start.Unix())

	// [[cov:QR:success-writes-utilization]] 额度调度器读的那几个键真的落了值。
	sampled := repo.lastExtraWith("session_window_utilization")
	require.NotNil(t, sampled)
	require.Equal(t, 0.42, sampled["session_window_utilization"])
	require.Equal(t, 0.11, sampled["passive_usage_7d_utilization"])
	require.Equal(t, resetAt.Add(72*time.Hour).Unix(), sampled["passive_usage_7d_reset"])
}

// 失败的测试（非 200）同样如实记录：422 的额度是真烧掉的。
// 与正例只差状态码这一个开关。
func TestAccountTestService_RecordsUpstreamQuotaOnNon200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	account := newMirasimShapedAccount(802, map[string]any{"claude-haiku-4-6": "claude-haiku-4-6"})
	resetAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	// 实测的 mirasim 422：型号不受支持。
	body := `{"error":{"message":"model \"claude-sonnet-4-5-20250929\" is not supported at this time"}}`
	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		claudeSSEResponse(http.StatusUnprocessableEntity, mirasimQuotaHeaders(resetAt), body),
	}}
	svc, repo := newClaudeTestServiceWithQuotaRecorder(account, upstream)

	err := svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault)

	// 落账不得吞掉失败本身。
	require.Error(t, err)
	require.Contains(t, err.Error(), "422")

	// [[cov:QR:failure-still-recorded]] 非 200 一样把消耗记回账号。
	require.Len(t, repo.sessionWindowCalls, 1)
	require.Equal(t, account.ID, repo.sessionWindowCalls[0].id)
	sampled := repo.lastExtraWith("session_window_utilization")
	require.NotNil(t, sampled)
	require.Equal(t, 0.42, sampled["session_window_utilization"])
}

// "没调用成功"必须和"调用了但没记"分得开：上游根本没应答时零落账。
func TestAccountTestService_NoQuotaRecordWhenUpstreamNeverAnswered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	account := newMirasimShapedAccount(803, map[string]any{"claude-haiku-4-6": "claude-haiku-4-6"})
	upstream := &failingHTTPUpstream{}
	svc, repo := newClaudeTestServiceWithQuotaRecorder(account, upstream)

	err := svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault)

	require.Error(t, err)
	require.Contains(t, err.Error(), "Request failed")
	// [[cov:QR:no-response-no-record]] 确实打过一次上游，但没有响应 → 一条都不记。
	require.Equal(t, 1, upstream.calls)
	require.Empty(t, repo.sessionWindowCalls)
	require.Empty(t, repo.extraUpdates)
}

// 差分阴性：落账完全由上游额度头驱动。上游没回额度头的既有路径（普通 anthropic
// 账号常见）不会因为这次改动凭空多出账号状态写入。
// 与正例只差"响应带不带 anthropic-ratelimit-unified-* 头"这一个开关。
func TestAccountTestService_QuotaRecordingIsHeaderDrivenOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	account := newMirasimShapedAccount(804, map[string]any{"claude-haiku-4-6": "claude-haiku-4-6"})
	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		claudeSSEResponse(http.StatusOK, nil, "data: {\"type\":\"message_stop\"}\n\n"),
	}}
	svc, repo := newClaudeTestServiceWithQuotaRecorder(account, upstream)

	require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault))

	// [[cov:QR:no-headers-no-write]]
	require.Len(t, upstream.requests, 1)
	require.Empty(t, repo.sessionWindowCalls)
	require.Empty(t, repo.extraUpdates)
}
