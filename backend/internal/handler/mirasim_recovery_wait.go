package handler

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	"time"
)

// Writes only SSE comments and only from the request goroutine. No background
// writer races with Forward, and semantic stream guards remain unchanged.
func (s *FailoverState) keepRecoveryStreamAlive(c *gin.Context, stream bool, started *bool) {
	if !stream {
		return
	}
	lastPing := time.Now()
	s.recoveryWait = func(ctx context.Context, d time.Duration) bool {
		end := time.Now().Add(d)
		for {
			remaining := time.Until(end)
			if remaining <= 0 {
				return ctx.Err() == nil
			}
			untilPing := time.Until(lastPing.Add(10 * time.Second))
			if untilPing > 0 {
				if !sleepWithContext(ctx, min(remaining, untilPing)) {
					return false
				}
				if time.Since(lastPing) < 10*time.Second {
					continue
				}
			}
			if !*started {
				c.Header("Content-Type", "text/event-stream")
				c.Header("Cache-Control", "no-cache")
				c.Header("X-Accel-Buffering", "no")
				*started = true
			}
			n, err := fmt.Fprint(c.Writer, ": waiting for upstream capacity\n\n")
			if err != nil {
				return false
			}
			recordGatewayStreamHeartbeat(c, n)
			c.Writer.Flush()
			lastPing = time.Now()
		}
	}
}
