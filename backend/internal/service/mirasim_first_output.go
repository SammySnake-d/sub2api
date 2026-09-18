package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var errMirasimFirstOutputTimeout = errors.New("mirasim first meaningful output timeout")

type mirasimBufferedBody struct {
	io.Reader
	body    io.Closer
	cleanup func()
}

func (b *mirasimBufferedBody) Close() error { b.cleanup(); return b.body.Close() }

// Headers/pings/message_start are not useful model output. Tool starts and
// thinking ARE progress: never replay a stream after these have been exposed.
func anthropicMeaningfulOutput(data string) bool {
	v := gjson.Parse(data)
	switch v.Get("type").String() {
	case "content_block_delta":
		d := v.Get("delta")
		return d.Get("text").String() != "" || d.Get("thinking").String() != "" || d.Get("partial_json").String() != "" || d.Get("signature").String() != ""
	case "content_block_start":
		b := v.Get("content_block")
		return b.Get("type").String() == "tool_use" || b.Get("type").String() == "server_tool_use" || b.Get("text").String() != "" || b.Get("thinking").String() != ""
	}
	return false
}

// Own the pre-output deadline across signing/auth, headers, and SSE prelude.
// No downstream bytes are written here, so a failed pre-output attempt cannot
// splice two responses. Once meaningful output is ready, the ordinary stream
// lifecycle takes over; this deadline never truncates accepted output.
func (s *GatewayService) doMirasimAwareUpstream(ctx context.Context, c *gin.Context, account *Account, req *http.Request, proxy, model string, stream bool) (*http.Response, error) {
	seconds := 0
	if s.cfg != nil {
		seconds = s.cfg.Gateway.MirasimFirstOutputTimeoutSeconds
	}
	if !IsMirasimAccount(account) || seconds <= 0 {
		return s.httpUpstream.DoWithTLS(req, proxy, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	}
	guardCtx, cancel := context.WithCancelCause(req.Context())
	timerDone := make(chan struct{})
	timer := time.AfterFunc(time.Duration(seconds)*time.Second, func() { cancel(errMirasimFirstOutputTimeout); close(timerDone) })
	var timerOnce sync.Once
	stopTimer := func() {
		timerOnce.Do(func() {
			if !timer.Stop() {
				<-timerDone
			}
		})
	}
	stopClient := context.AfterFunc(ctx, func() { cancel(ctx.Err()) })
	cleanup := func() { stopTimer(); stopClient(); cancel(context.Canceled) }
	resp, err := s.httpUpstream.DoWithTLS(req.Clone(guardCtx), proxy, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err == nil && resp != nil && resp.Body != nil {
		body := resp.Body
		stopClose := context.AfterFunc(guardCtx, func() { _ = body.Close() })
		var reader io.Reader = body
		if stream && resp.StatusCode < 400 {
			var buffered []byte
			var br *bufio.Reader
			buffered, br, err = readAnthropicPrelude(body)
			if err == nil {
				reader = io.MultiReader(bytes.NewReader(buffered), br)
			}
		} else if !stream && resp.StatusCode < 400 {
			limit := config.DefaultUpstreamResponseReadMaxBytes
			if s.cfg.Gateway.UpstreamResponseReadMaxBytes > 0 {
				limit = s.cfg.Gateway.UpstreamResponseReadMaxBytes
			}
			var buffered []byte
			buffered, err = io.ReadAll(io.LimitReader(body, limit+1))
			if err == nil && int64(len(buffered)) > limit {
				err = fmt.Errorf("upstream response exceeds configured limit")
			}
			if err == nil {
				reader = bytes.NewReader(buffered)
			}
		}
		stopTimer()
		stopClient()
		stopClose()
		if cause := context.Cause(guardCtx); cause != nil {
			err = cause
		}
		if err == nil {
			resp.Body = &mirasimBufferedBody{Reader: reader, body: body, cleanup: cleanup}
			return resp, nil
		}
		_ = body.Close()
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	cause := context.Cause(guardCtx)
	cleanup()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(cause, errMirasimFirstOutputTimeout) || errors.Is(err, errMirasimFirstOutputTimeout) {
		body := []byte(`{"error":{"type":"overloaded_error","message":"upstream did not produce meaningful output within the configured attempt deadline"}}`)
		if s.rateLimitService != nil {
			s.rateLimitService.HandleUpstreamError(ctx, account, http.StatusGatewayTimeout, nil, body, model)
		}
		return nil, &UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout, ResponseBody: body}
	}
	return nil, err
}

// Bound memory and wait for a complete SSE frame. Pings are discarded only
// before output, rather than growing a buffer forever. Unknown semantic frames
// are passed through conservatively (never discarded or transparently retried).
func readAnthropicPrelude(body io.Reader) ([]byte, *bufio.Reader, error) {
	br := bufio.NewReaderSize(body, 32*1024)
	var prelude, frame bytes.Buffer
	ready := false
	ping := false
	for {
		fragment, err := br.ReadSlice('\n')
		frame.Write(fragment)
		if frame.Len()+prelude.Len() > 256*1024 {
			return nil, br, fmt.Errorf("anthropic pre-output prelude exceeds 256 KiB")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if err == io.EOF {
				prelude.Write(frame.Bytes())
				return prelude.Bytes(), br, nil
			}
			return nil, br, err
		}
		// Frames, not individual scanner lines, are the commit boundary.
		raw := frame.Bytes()
		if !(bytes.HasSuffix(raw, []byte("\n\n")) || bytes.HasSuffix(raw, []byte("\r\n\r\n"))) {
			continue
		}
		for _, line := range strings.Split(frame.String(), "\n") {
			data, ok := extractAnthropicSSEDataLine(strings.TrimSuffix(line, "\r"))
			if !ok {
				continue
			}
			kind := gjson.Get(data, "type").String()
			if kind == "ping" {
				ping = true
				continue
			}
			if anthropicMeaningfulOutput(data) || anthropicStreamEventIsTerminal("", data) {
				ready = true
			}
			switch kind {
			case "message_start", "content_block_start", "content_block_delta":
			default:
				ready = true
			}
		}
		if !ping || ready {
			prelude.Write(frame.Bytes())
		}
		frame.Reset()
		ping = false
		if ready {
			return prelude.Bytes(), br, nil
		}
	}
}
