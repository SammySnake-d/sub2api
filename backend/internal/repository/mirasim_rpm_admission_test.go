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

func TestMirasimRPMAdmissionAtomicAcrossInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	now := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	mr.SetTime(now)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c.Close()
	a, b := &RPMCacheImpl{rdb: c}, &RPMCacheImpl{rdb: c}
	ctx := context.Background()
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cache := a
			if i%2 == 0 {
				cache = b
			}
			ok, err := cache.TryAcquireRPM(ctx, 71, 4)
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, int32(4), admitted.Load())
	count, err := a.GetRPM(ctx, 71)
	require.NoError(t, err)
	require.Equal(t, 4, count)
	ok, err := a.TryAcquireRPM(ctx, 72, 4)
	require.NoError(t, err)
	require.True(t, ok, "other account independent")
	ok, err = b.TryAcquireRPM(ctx, 71, 0)
	require.NoError(t, err)
	require.True(t, ok, "disabled config")
	mr.SetTime(now.Add(time.Minute))
	ok, err = b.TryAcquireRPM(ctx, 71, 4)
	require.NoError(t, err)
	require.True(t, ok, "next minute recovers without resetting quota state")
}
