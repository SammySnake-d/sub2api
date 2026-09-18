package repository

import (
	"context"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMirasimProbeDistributedLeaseAndRate(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	a, b := &gatewayCache{rdb: client}, &gatewayCache{rdb: client}
	ctx := context.Background()
	var won atomic.Int32
	var winner string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := a.AcquireMirasimProbe(ctx, "model", 5*time.Second, 30*time.Second)
			if err != nil {
				t.Error(err)
			}
			if token != "" {
				won.Add(1)
				mu.Lock()
				winner = token
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), won.Load())
	require.NoError(t, b.ReleaseMirasimProbe(ctx, "model", "wrong-owner"))
	other, err := b.AcquireMirasimProbe(ctx, "other-model", 5*time.Second, 30*time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, other)
	mr.FastForward(6 * time.Second)
	denied, err := b.AcquireMirasimProbe(ctx, "model", 5*time.Second, 30*time.Second)
	require.NoError(t, err)
	require.Empty(t, denied, "lease excludes parallel probe after rate token expired")
	renewed, err := a.RenewMirasimProbe(ctx, "model", winner, 30*time.Second)
	require.NoError(t, err)
	require.True(t, renewed)
	require.NoError(t, a.ReleaseMirasimProbe(ctx, "model", winner))
	next, err := b.AcquireMirasimProbe(ctx, "model", 5*time.Second, 30*time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, next)
	require.NoError(t, a.ReleaseMirasimProbe(ctx, "model", winner))
	ok, err := b.RenewMirasimProbe(ctx, "model", next, 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok, "old owner cannot delete new lease")
	require.NoError(t, b.ReleaseMirasimProbe(ctx, "model", next))
	denied, err = a.AcquireMirasimProbe(ctx, "model", 5*time.Second, 30*time.Second)
	require.NoError(t, err)
	require.Empty(t, denied, "release must not erase minimum start interval")
	mr.FastForward(31 * time.Second)
	next, err = a.AcquireMirasimProbe(ctx, "model", 5*time.Second, 30*time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, next)
}
