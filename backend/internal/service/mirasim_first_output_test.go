package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type firstOutputUpstream struct {
	call func(*http.Request) (*http.Response, error)
}

func (u *firstOutputUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.call(r)
}
func (u *firstOutputUpstream) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, p, id, n)
}
func firstOutputFixture(t *testing.T, u *firstOutputUpstream) (*GatewayService, *Account, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	return &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MirasimFirstOutputTimeoutSeconds: 5}}, httpUpstream: u, tlsFPProfileService: &TLSFingerprintProfileService{}}, &Account{ID: 1, Platform: PlatformAnthropic, Credentials: map[string]any{"provider": "mirasim"}}, c
}
func TestMirasimFirstOutputPingCannotKeepAttemptAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pr, pw := io.Pipe()
		defer pw.Close()
		u := &firstOutputUpstream{call: func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: pr, Header: make(http.Header)}, nil
		}}
		svc, account, c := firstOutputFixture(t, u)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				if _, err := pw.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")); err != nil {
					return
				}
				time.Sleep(time.Second)
			}
		}()
		start := time.Now()
		resp, err := svc.doMirasimAwareUpstream(c.Request.Context(), c, account, c.Request, "", "claude-fable-5-1", true)
		var failure *UpstreamFailoverError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, 504, failure.StatusCode)
		require.Nil(t, resp)
		require.Equal(t, 5*time.Second, time.Since(start))
		require.False(t, c.Writer.Written(), "no prelude from failed attempt may reach client")
		_ = pw.Close()
		<-done
	})
}
func TestMirasimFirstOutputHeadersTimeoutAndClientCancel(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			u := &firstOutputUpstream{call: func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }}
			svc, account, c := firstOutputFixture(t, u)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelClient {
				time.AfterFunc(time.Second, cancel)
			}
			_, err := svc.doMirasimAwareUpstream(ctx, c, account, c.Request, "", "claude-fable-5-1", true)
			if cancelClient {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				var failure *UpstreamFailoverError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, 504, failure.StatusCode)
			}
		})
	}
}
func TestMirasimFirstOutputReplaysPreludeOnceAndDoesNotTruncateAcceptedStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prelude := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"one\"}}\n\n"
		output := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"working\"}}\n\n"
		terminal := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		pr, pw := io.Pipe()
		defer pw.Close()
		var sent *http.Request
		u := &firstOutputUpstream{call: func(r *http.Request) (*http.Response, error) {
			sent = r
			return &http.Response{StatusCode: 200, Body: pr, Header: make(http.Header)}, nil
		}}
		svc, account, c := firstOutputFixture(t, u)
		go func() {
			_, _ = pw.Write([]byte(prelude))
			time.Sleep(2 * time.Second)
			_, _ = pw.Write([]byte(output))
			time.Sleep(time.Minute)
			_, _ = pw.Write([]byte(terminal))
			pw.Close()
		}()
		resp, err := svc.doMirasimAwareUpstream(context.Background(), c, account, c.Request, "", "model", true)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, prelude+output+terminal, string(body))
		require.NoError(t, sent.Context().Err())
		require.NoError(t, resp.Body.Close())
	})
}
func TestAnthropicMeaningfulOutputExcludesMetadata(t *testing.T) {
	require.False(t, anthropicMeaningfulOutput(`{"type":"ping"}`))
	require.False(t, anthropicMeaningfulOutput(`{"type":"message_start"}`))
	require.False(t, anthropicMeaningfulOutput(`{"type":"content_block_start","content_block":{"type":"text","text":""}}`))
	require.True(t, anthropicMeaningfulOutput(`{"type":"content_block_start","content_block":{"type":"tool_use","name":"read"}}`))
	require.True(t, anthropicMeaningfulOutput(`{"type":"content_block_delta","delta":{"thinking":"thinking"}}`))
	payload := "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	p, _, err := readAnthropicPrelude(strings.NewReader(payload))
	require.NoError(t, err)
	require.Equal(t, payload, string(p))
}

func TestMirasimFirstOutputNonStreamDoesNotStopDeadlineAtHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pr, pw := io.Pipe()
		defer pw.Close()
		u := &firstOutputUpstream{call: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: pr}, nil
		}}
		svc, account, c := firstOutputFixture(t, u)
		start := time.Now()
		_, err := svc.doMirasimAwareUpstream(context.Background(), c, account, c.Request, "", "model", false)
		var fail *UpstreamFailoverError
		require.ErrorAs(t, err, &fail)
		require.Equal(t, 504, fail.StatusCode)
		require.Equal(t, 5*time.Second, time.Since(start))
	})
}
