package service

import (
	"bytes"
	"context"
	"errors"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 回归目标（2026-09-16 生产）：saveProxyLatency 用调用方的请求 ctx 读写 Redis，
// 浏览器一断（axios 全局 30s < 单次质量检测最坏 ~80s）结果就被静默丢弃，
// 日志里只留 "Warning: store proxy latency cache failed: context canceled"；
// 又因为 SET 的 TTL=0 且没有周期性重测，面板上的旧读数会无限期留着。

type proxyLatencyPersistCtxKey struct{}

// captureProxyLatencyStdLog 抓 stdlib log 输出。logger 全局实例未初始化时
// LegacyPrintf 会回退到 log.Print（internal/pkg/logger/logger.go:488），
// 这里靠的就是那条回退路径。同包已有的 captureStdLog 带 `unit` build tag，
// 这个文件不打 tag，所以自带一份。
func captureProxyLatencyStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// fakeProxyLatencyStore 复刻 go-redis v9 的关键行为：ctx 已 Done 时在
// pool.waitTurn 的 fast path 直接返回 ctx.Err()，命令根本不会发到 Redis。
// 缺陷就是撞在这条路径上，所以假件必须保留这一条，否则这组测试证明不了什么。
type fakeProxyLatencyStore struct {
	mu         sync.Mutex
	stored     map[int64]*ProxyLatencyInfo
	getErr     error
	setErr     error
	getCalls   int
	setCalls   int
	lastGetCtx context.Context
	lastSetCtx context.Context
}

func newFakeProxyLatencyStore() *fakeProxyLatencyStore {
	return &fakeProxyLatencyStore{stored: make(map[int64]*ProxyLatencyInfo)}
}

func (f *fakeProxyLatencyStore) seed(proxyID int64, info *ProxyLatencyInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *info
	f.stored[proxyID] = &clone
}

func (f *fakeProxyLatencyStore) snapshot(proxyID int64) *ProxyLatencyInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.stored[proxyID]
	if !ok {
		return nil
	}
	clone := *info
	return &clone
}

func (f *fakeProxyLatencyStore) GetProxyLatencies(ctx context.Context, proxyIDs []int64) (map[int64]*ProxyLatencyInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	f.lastGetCtx = ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	out := make(map[int64]*ProxyLatencyInfo, len(proxyIDs))
	for _, id := range proxyIDs {
		if info, ok := f.stored[id]; ok {
			clone := *info
			out[id] = &clone
		}
	}
	return out, nil
}

func (f *fakeProxyLatencyStore) SetProxyLatency(ctx context.Context, proxyID int64, info *ProxyLatencyInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.lastSetCtx = ctx
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.setErr != nil {
		return f.setErr
	}
	if info == nil {
		return nil
	}
	clone := *info
	f.stored[proxyID] = &clone
	return nil
}

// 阳性：客户端断开（请求 ctx 已取消）之后，已经测出来的延迟必须仍然落库，
// 并且旧的质量字段仍然被合并进来。修复前 Get/Set 都拿到已死的 ctx：
// Set 丢结果、Get 丢合并，面板永远显示上一轮的值。
func TestSaveProxyLatency_PersistsAfterRequestContextCanceled(t *testing.T) {
	store := newFakeProxyLatencyStore()
	oldScore := 91
	oldCheckedAt := time.Now().Add(-time.Hour).Unix()
	oldLatency := int64(1557)
	store.seed(79, &ProxyLatencyInfo{
		Success:          true,
		LatencyMs:        &oldLatency,
		QualityStatus:    "healthy",
		QualityScore:     &oldScore,
		QualityGrade:     "A",
		QualitySummary:   "上一轮质检",
		QualityCheckedAt: &oldCheckedAt,
		QualityCFRay:     "old-ray",
	})

	svc := &adminServiceImpl{proxyLatencyCache: store}

	// 模拟 gin 在浏览器断开时取消 c.Request.Context()
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()

	freshLatency := int64(293)
	svc.saveProxyLatency(reqCtx, 79, &ProxyLatencyInfo{
		Success:   true,
		LatencyMs: &freshLatency,
		Message:   "Proxy is accessible",
		UpdatedAt: time.Now(),
	})

	got := store.snapshot(79)
	require.NotNil(t, got, "请求 ctx 已取消时新的延迟必须仍然写入缓存")
	require.NotNil(t, got.LatencyMs)
	require.Equal(t, int64(293), *got.LatencyMs, "面板上应当是本次测出来的值，不是上一轮的 1557ms")
	// 读同样要脱离取消，否则本次只测延迟时旧质量字段会整段丢失
	require.Equal(t, "A", got.QualityGrade)
	require.NotNil(t, got.QualityScore)
	require.Equal(t, 91, *got.QualityScore)
	require.Equal(t, "healthy", got.QualityStatus)
	require.Equal(t, "old-ray", got.QualityCFRay)
}

// 差分阴性 1：脱离取消不等于换成 context.Background()。
// ctx 上的请求作用域 value 要保留，同时必须自带 deadline，
// 否则 Redis 卡住时这个写入会永远挂着。
func TestSaveProxyLatency_DetachedContextKeepsValuesAndIsBounded(t *testing.T) {
	store := newFakeProxyLatencyStore()
	svc := &adminServiceImpl{proxyLatencyCache: store}

	base := context.WithValue(context.Background(), proxyLatencyPersistCtxKey{}, "req-42")
	reqCtx, cancel := context.WithCancel(base)
	cancel()

	start := time.Now()
	latency := int64(120)
	svc.saveProxyLatency(reqCtx, 7, &ProxyLatencyInfo{Success: true, LatencyMs: &latency})

	require.Equal(t, 1, store.setCalls)
	require.Equal(t, 1, store.getCalls)
	require.Equal(t, "req-42", store.lastSetCtx.Value(proxyLatencyPersistCtxKey{}), "写用的 ctx 丢了请求作用域 value")
	require.Equal(t, "req-42", store.lastGetCtx.Value(proxyLatencyPersistCtxKey{}), "读用的 ctx 丢了请求作用域 value")

	deadline, ok := store.lastSetCtx.Deadline()
	require.True(t, ok, "落库 ctx 必须自带 deadline，不能是无限期的")
	require.True(t, deadline.After(start), "deadline 必须在未来")
	require.True(t, deadline.Sub(start) <= proxyLatencyPersistTimeout+time.Second,
		"落库预算不该超过 %s，实际 %s", proxyLatencyPersistTimeout, deadline.Sub(start))
}

// 差分阴性 2：真正的缓存写失败（不是 ctx 问题）仍然要报出来，
// 修复不能退化成「吞掉所有错误」。
func TestSaveProxyLatency_StillReportsRealWriteFailure(t *testing.T) {
	buf := captureProxyLatencyStdLog(t)
	store := newFakeProxyLatencyStore()
	store.setErr = errors.New("redis: connection refused")
	svc := &adminServiceImpl{proxyLatencyCache: store}

	latency := int64(88)
	svc.saveProxyLatency(context.Background(), 5, &ProxyLatencyInfo{Success: true, LatencyMs: &latency})

	require.Nil(t, store.snapshot(5))
	require.Contains(t, buf.String(), "store proxy latency cache failed")
	require.Contains(t, buf.String(), "connection refused")
}

// 差分阴性 3：读失败不再无声，而且不连累写——本次测出来的延迟照样落库。
func TestSaveProxyLatency_ReadFailureIsLoggedAndWriteStillHappens(t *testing.T) {
	buf := captureProxyLatencyStdLog(t)
	store := newFakeProxyLatencyStore()
	store.getErr = errors.New("redis: MGET failed")
	svc := &adminServiceImpl{proxyLatencyCache: store}

	latency := int64(310)
	svc.saveProxyLatency(context.Background(), 11, &ProxyLatencyInfo{Success: true, LatencyMs: &latency})

	require.Contains(t, buf.String(), "load proxy latency cache for merge failed")
	require.Contains(t, buf.String(), "MGET failed")

	got := store.snapshot(11)
	require.NotNil(t, got, "读失败不应阻止写入")
	require.NotNil(t, got.LatencyMs)
	require.Equal(t, int64(310), *got.LatencyMs)
}

// 差分阴性 4：正常 ctx 下的合并语义一个字都不能变。
func TestSaveProxyLatency_LiveContextMergeSemanticsUnchanged(t *testing.T) {
	t.Run("本次自带质量字段时不被旧值覆盖", func(t *testing.T) {
		store := newFakeProxyLatencyStore()
		oldScore := 20
		oldCheckedAt := int64(1000)
		store.seed(3, &ProxyLatencyInfo{
			QualityStatus:    "failed",
			QualityScore:     &oldScore,
			QualityGrade:     "F",
			QualitySummary:   "旧",
			QualityCheckedAt: &oldCheckedAt,
		})
		svc := &adminServiceImpl{proxyLatencyCache: store}

		newScore := 95
		newCheckedAt := int64(2000)
		svc.saveProxyLatency(context.Background(), 3, &ProxyLatencyInfo{
			Success:          true,
			QualityStatus:    "healthy",
			QualityScore:     &newScore,
			QualityGrade:     "A",
			QualitySummary:   "新",
			QualityCheckedAt: &newCheckedAt,
		})

		got := store.snapshot(3)
		require.NotNil(t, got)
		require.Equal(t, "A", got.QualityGrade)
		require.Equal(t, 95, *got.QualityScore)
		require.Equal(t, int64(2000), *got.QualityCheckedAt)
	})

	t.Run("本次只测延迟时沿用旧质量字段", func(t *testing.T) {
		store := newFakeProxyLatencyStore()
		oldScore := 70
		oldCheckedAt := int64(1000)
		store.seed(4, &ProxyLatencyInfo{
			QualityStatus:    "healthy",
			QualityScore:     &oldScore,
			QualityGrade:     "B",
			QualitySummary:   "旧",
			QualityCheckedAt: &oldCheckedAt,
		})
		svc := &adminServiceImpl{proxyLatencyCache: store}

		latency := int64(42)
		svc.saveProxyLatency(context.Background(), 4, &ProxyLatencyInfo{Success: true, LatencyMs: &latency})

		got := store.snapshot(4)
		require.NotNil(t, got)
		require.Equal(t, int64(42), *got.LatencyMs)
		require.Equal(t, "B", got.QualityGrade)
		require.Equal(t, 70, *got.QualityScore)
		require.Equal(t, int64(1000), *got.QualityCheckedAt)
	})
}

// 差分阴性 5：修复不能退化成「无条件写」——nil 结果依然什么都不做。
func TestSaveProxyLatency_NilInfoWritesNothing(t *testing.T) {
	store := newFakeProxyLatencyStore()
	svc := &adminServiceImpl{proxyLatencyCache: store}

	svc.saveProxyLatency(context.Background(), 9, nil)

	require.Equal(t, 0, store.setCalls)
	require.Equal(t, 0, store.getCalls)
	require.Nil(t, store.snapshot(9))
}
