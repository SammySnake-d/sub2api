//go:build unit

package service

// mirasim 503 容量停调的行为锁。
//
// 每条测试都标注了它钉的是**哪一条不变量**，以及「去掉这条测试后什么错误实现能
// 蒙混过关」—— 差分阴性两条尤其如此：只写阳性的话，一个「按账号 park」的实现
// （正是本功能要避免的行为）同样能让阳性全绿。
//
// 不变量清单：
//
//	INV-1 停调生效       503 之后同一 (账号, 模型) 在 TTL 内不再被调度
//	INV-2 模型隔离       停调 claude-sonnet-5 不影响同账号的 claude-fable-5
//	INV-3 0 = 关闭       配置为 0 时行为与本功能上线前逐字节一致
//	INV-4 不写账号级状态  停调不得改 RateLimitResetAt / Status / Schedulable
//	INV-5 不与窗口冷却互相覆盖  mirasim:7d_claude / mirasim:7d_fable 与容量 scope 各管各的
//	INV-6 fail-open      全池停调时选号仍然返回账号，而不是「无可用账号」
//	INV-7 fail-open 不滥用 还有正常候选时绝不放行被停调的账号

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/stretchr/testify/require"
)

const (
	mirasimCapacityTestModel      = "claude-sonnet-5"
	mirasimCapacityTestOtherModel = "claude-fable-5"
	mirasimCapacity503Body        = `{"error":{"code":"service_capacity_overloaded","message":"service capacity overloaded","type":"overloaded_error"}}`
)

// mirasimCapacityServiceWithSetting 构造一个带真实 SettingService 的
// RateLimitService，设置值按 raw 写入（"" 表示该 key 未配置）。
// 用真实的 SettingService 而不是塞一个假时长，是为了让「设置项 → 行为」这条线
// 本身也被测试覆盖：只测内部常量的话，接线接错了测试照样全绿。
func mirasimCapacityServiceWithSetting(raw string) (*RateLimitService, *mirasimAccountRepoStub, *Account) {
	svc, repo, account := mirasimServiceWithStub()
	settingRepo := newMockSettingRepo()
	if raw != "" {
		settingRepo.data[SettingKeyMirasimCapacityParkMinutes] = raw
	}
	svc.SetSettingService(NewSettingService(settingRepo, &config.Config{}))
	return svc, repo, account
}

// ---------------------------------------------------------------------------
// INV-1 / INV-4：阳性 + 账号级状态不动
// ---------------------------------------------------------------------------

func TestMirasim503ParksTheRequestedModelForTheConfiguredTTL(t *testing.T) {
	ctx := context.Background()
	svc, repo, account := mirasimCapacityServiceWithSetting("") // 未配置 → 默认 10 分钟
	before := mirasimSnapshotAccount(account)

	shouldDisable := svc.HandleUpstreamError(ctx, account, http.StatusServiceUnavailable,
		http.Header{}, []byte(mirasimCapacity503Body), mirasimCapacityTestModel)

	require.False(t, shouldDisable, "503 是容量问题，不是账号问题；禁用账号会摘掉一个健康号")

	// INV-1：这个 (账号, 模型) 在 TTL 内不可调度。
	require.False(t, account.IsSchedulableForModelWithContext(ctx, mirasimCapacityTestModel),
		"503 之后同一账号同一模型必须停调，否则它会被后续请求反复选中、反复吃 503")

	// 落库的必须是模型级 scope，且时长是默认的 10 分钟。
	modelCalls := repo.callsOf("SetModelRateLimit")
	require.Len(t, modelCalls, 1)
	require.Equal(t, mirasimCapacityRateLimitScope(mirasimCapacityTestModel), modelCalls[0].scope)
	require.Equal(t, mirasimCapacityParkReason, modelCalls[0].reason)
	require.WithinDuration(t, time.Now().Add(mirasimCapacityParkDefaultMinutes*time.Minute),
		modelCalls[0].resetAt, 5*time.Second,
		"默认停调时长必须是 10 分钟：运营经验里 503 的恢复是十分钟量级")

	// INV-4：账号级状态一个字段都不许动。
	require.Empty(t, repo.callsOf("SetRateLimited"),
		"容量 503 绝不能写账号级限流：额度是好的，写了会让整号对所有模型下线")
	require.Empty(t, repo.callsOf("SetTempUnschedulable"))
	require.Empty(t, repo.callsOf("SetError"))
	require.Nil(t, account.RateLimitResetAt)
	after := mirasimSnapshotAccount(account)
	require.Equal(t, before.Status, after.Status)
	require.Equal(t, before.Schedulable, after.Schedulable)
	require.Equal(t, before.RateLimitResetAt, after.RateLimitResetAt)
	require.Equal(t, before.TempUnschedulableUntil, after.TempUnschedulableUntil)
	require.True(t, account.IsSchedulable(), "账号本身仍然健康可调度")
}

// ---------------------------------------------------------------------------
// INV-2：差分阴性 1 —— 模型隔离
// ---------------------------------------------------------------------------

// 这条是「按模型 park」与「按账号 park」的唯一分辨器。
// 把实现改成账号级（写 SetRateLimited 或写一个不含模型名的 scope），阳性那条依然
// 全绿，只有这条会红。
func TestMirasim503ParkDoesNotBlockOtherModelsOnTheSameAccount(t *testing.T) {
	ctx := context.Background()
	svc, _, account := mirasimCapacityServiceWithSetting("")

	svc.HandleUpstreamError(ctx, account, http.StatusServiceUnavailable,
		http.Header{}, []byte(mirasimCapacity503Body), mirasimCapacityTestModel)

	require.False(t, account.IsSchedulableForModelWithContext(ctx, mirasimCapacityTestModel),
		"前提：被 503 的模型确实已停调（这一条不成立时下面的隔离断言没有鉴别力）")
	require.True(t, account.IsSchedulableForModelWithContext(ctx, mirasimCapacityTestOtherModel),
		"mirasim 的容量池每模型独立：claude-sonnet-5 没容量不代表 claude-fable-5 也没有；"+
			"把整号停掉会白白废掉一个健康号")
}

// ---------------------------------------------------------------------------
// INV-3：差分阴性 2 —— 配置 0 = 完全不 park
// ---------------------------------------------------------------------------

func TestMirasim503ParkDisabledByZeroMinutesKeepsLegacyBehaviour(t *testing.T) {
	ctx := context.Background()
	svc, repo, account := mirasimCapacityServiceWithSetting("0")
	before := mirasimSnapshotAccount(account)

	shouldDisable := svc.HandleUpstreamError(ctx, account, http.StatusServiceUnavailable,
		http.Header{}, []byte(mirasimCapacity503Body), mirasimCapacityTestModel)

	require.False(t, shouldDisable)
	// 「行为与当前完全一致」= 一次落库都没有、账号快照逐字段不变、立刻仍可调度。
	require.Len(t, repo.calls, 0, "配置为 0 时 503 不该产生任何一次状态落库（运维逃生口）")
	require.Equal(t, before, mirasimSnapshotAccount(account),
		"配置为 0 时账号状态被改写了；差异见上（左=收到 503 之前，右=之后）")
	require.True(t, account.IsSchedulableForModelWithContext(ctx, mirasimCapacityTestModel),
		"配置为 0 时 503 之后账号必须立刻仍可调度")
}

func TestMirasimCapacityParkMinutesParsing(t *testing.T) {
	// 默认值与逃生口的判据本身。负数刻意不当成「关闭」：手滑打成 -10 的人
	// 想要的是 10 分钟，静默关停调是个陷阱。
	require.Equal(t, mirasimCapacityParkDefaultMinutes*time.Minute, parseMirasimCapacityParkMinutes(""))
	require.Equal(t, mirasimCapacityParkDefaultMinutes*time.Minute, parseMirasimCapacityParkMinutes("not-a-number"))
	require.Equal(t, mirasimCapacityParkDefaultMinutes*time.Minute, parseMirasimCapacityParkMinutes("-10"))
	require.Equal(t, time.Duration(0), parseMirasimCapacityParkMinutes("0"))
	require.Equal(t, 25*time.Minute, parseMirasimCapacityParkMinutes(" 25 "))
	require.Equal(t, mirasimCapacityParkMaxMinutes*time.Minute, parseMirasimCapacityParkMinutes("99999"))
}

// ---------------------------------------------------------------------------
// INV-5：容量停调与窗口冷却互不覆盖
// ---------------------------------------------------------------------------

func TestMirasim503ParkAndWindowCooldownsDoNotOverwriteEachOther(t *testing.T) {
	ctx := context.Background()
	svc, _, account := mirasimCapacityServiceWithSetting("")

	// 先来一条真实的 7d_claude 窗口冷却（天级），再来一条容量 503（分钟级）。
	windowReset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	setAccountModelRateLimitSnapshot(account, mirasimClaude7dRateLimitKey, windowReset,
		mirasimWindowReason(MirasimWindow7dClaude), time.Now())

	svc.HandleUpstreamError(ctx, account, http.StatusServiceUnavailable,
		http.Header{}, []byte(mirasimCapacity503Body), mirasimCapacityTestModel)

	// 两条冷却并存，各自的 reset 都没被对方改写。
	capacityReset := account.modelRateLimitResetAt(mirasimCapacityRateLimitScope(mirasimCapacityTestModel))
	require.NotNil(t, capacityReset)
	claudeReset := account.modelRateLimitResetAt(mirasimClaude7dRateLimitKey)
	require.NotNil(t, claudeReset)
	require.Equal(t, windowReset.Unix(), claudeReset.Unix(),
		"10 分钟的容量停调把一条还剩 3 天的 7d_claude 窗口冷却改短了 —— 耗尽的号会提前回到候选池")
	require.True(t, capacityReset.Before(windowReset),
		"容量停调是分钟级的，不该继承窗口冷却的天级 reset")
	require.Nil(t, account.modelRateLimitResetAt(mirasimFable7dRateLimitKey),
		"另一个家族的窗口 scope 不该被这次 503 碰到")
}

func TestMirasimCapacityScopeNeverCollidesWithWindowScopes(t *testing.T) {
	// scope 取名的机械保证：容量 scope 不可能等于任何一个窗口 scope，
	// 也不可能等于裸模型名 scope（那个被 upstream_404_model_not_found 占用）。
	for _, model := range []string{"claude-sonnet-5", "claude-fable-5", "7d_claude", "7d_fable"} {
		scope := mirasimCapacityRateLimitScope(model)
		require.NotEqual(t, mirasimClaude7dRateLimitKey, scope)
		require.NotEqual(t, mirasimFable7dRateLimitKey, scope)
		require.NotEqual(t, model, scope)
		require.True(t, isMirasimCapacityParkScope(scope))
	}
	require.False(t, isMirasimCapacityParkScope(mirasimClaude7dRateLimitKey))
	require.False(t, isMirasimCapacityParkScope(mirasimFable7dRateLimitKey))
	require.Equal(t, "", mirasimCapacityRateLimitScope("  "))
}

// 非 mirasim 的普通 anthropic 账号一个字节都不该变：503 仍旧走共享路径。
func TestPlainAnthropic503StaysOnSharedPath(t *testing.T) {
	ctx := context.Background()
	plain := plainAnthropicTestAccount()
	repo := &mirasimAccountRepoStub{bound: plain}
	svc := &RateLimitService{accountRepo: repo}

	handled, disable := svc.handleMirasimUpstreamError(ctx, plain, http.StatusServiceUnavailable,
		http.Header{}, []byte(mirasimCapacity503Body))

	require.False(t, handled, "普通 anthropic 账号绝不能进 mirasim 分支")
	require.False(t, disable)
	require.Empty(t, repo.calls)
}

// ---------------------------------------------------------------------------
// INV-6 / INV-7：fail-open
// ---------------------------------------------------------------------------

// mirasimCapacityParkedAccount 造一个「除了容量停调之外一切健康」的 mirasim 账号。
func mirasimCapacityParkedAccount(id int64, parkedModel string) Account {
	acc := Account{
		ID:          id,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Priority:    int(id),
		Concurrency: 5,
		Credentials: map[string]any{
			mirasim.CredProvider: mirasim.ProviderMirasim,
		},
	}
	if parkedModel != "" {
		setAccountModelRateLimitSnapshot(&acc, mirasimCapacityRateLimitScope(parkedModel),
			time.Now().Add(10*time.Minute), mirasimCapacityParkReason, time.Now())
	}
	return acc
}

// mirasimCapacityGatewayFixture 走负载感知选号路径（LoadBatchEnabled + 并发服务）。
func mirasimCapacityGatewayFixture(t *testing.T, accounts []Account) (*GatewayService, context.Context, int64) {
	t.Helper()
	groupID := int64(20)

	accountRepo := &mockAccountRepoForPlatform{
		accounts:     accounts,
		accountsByID: map[int64]*Account{},
	}
	for i := range accountRepo.accounts {
		accountRepo.accountsByID[accountRepo.accounts[i].ID] = &accountRepo.accounts[i]
	}
	group := &Group{ID: groupID, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true

	svc := &GatewayService{
		accountRepo:        accountRepo,
		groupRepo:          &mockGroupRepoForGateway{groups: map[int64]*Group{groupID: group}},
		cache:              &mockGatewayCacheForPlatform{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(&mockConcurrencyCache{}),
	}
	return svc, context.WithValue(context.Background(), ctxkey.Group, group), groupID
}

// ★ 最重要的一条：全池 503 停调时，选号必须仍然返回一个账号。
// 没有这条退化，一次全池级容量紧张会让这个模型从「慢」直接变成「10 分钟完全不可用」。
func TestSelectAccountFailsOpenWhenWholePoolIsCapacityParked(t *testing.T) {
	svc, ctx, groupID := mirasimCapacityGatewayFixture(t, []Account{
		mirasimCapacityParkedAccount(1, mirasimCapacityTestModel),
		mirasimCapacityParkedAccount(2, mirasimCapacityTestModel),
	})

	// 前提自证：这两个号在正常判据下确实都不可调度（否则这条测试没有鉴别力）。
	for _, acc := range svc.accountRepo.(*mockAccountRepoForPlatform).accounts {
		require.False(t, acc.IsSchedulableForModelWithContext(ctx, mirasimCapacityTestModel),
			"前提：账号 %d 本该因容量停调而不可调度", acc.ID)
	}

	result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", mirasimCapacityTestModel, nil, "", 0)

	require.NoError(t, err, "全池被 503 停调时必须退化放行，宁可撞 503 也不能返回「无可用账号」")
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
}

// INV-7 的反向对照：只要还有一个健康候选，被停调的号就绝不能被选中。
// 少了这条，把 fail-open 写成「恒为真」也能让上面那条全绿 —— 而那等于容量停调从未生效。
func TestSelectAccountSkipsCapacityParkedAccountWhenHealthyOneExists(t *testing.T) {
	svc, ctx, groupID := mirasimCapacityGatewayFixture(t, []Account{
		mirasimCapacityParkedAccount(1, mirasimCapacityTestModel), // 优先级更高但被停调
		mirasimCapacityParkedAccount(2, ""),                       // 健康
	})

	result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", mirasimCapacityTestModel, nil, "", 0)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
	require.Equal(t, int64(2), result.Account.ID,
		"还有健康候选时必须跳过被 503 停调的号 —— 否则停调等于没写")
}

// ★ 这条锁的是 fail-open 判据本身的形状，不是它的效果。
//
// 第一版判据是事前预测：扫一遍**原始**账号列表，只要有任何一个过了
// IsSchedulableForModelWithContext 就判「还有正常候选」、不退化。可那个函数只看
// 「账号可调度 + 没有模型级限流」两件事，真正的候选循环在它之后还要过模型白名单、
// 渠道限价、利润门等好几道。于是「有个号过了可调度性、却卡在后面某一道」时，
// 预测说不用退化，而候选集其实是空的 —— 请求拿到 ErrNoAvailableAccounts，
// 正好是这个功能要消灭的形态；而且那是本功能**新引入**的不可用面
// （加它之前 503 不写冷却，不会有空候选）。
//
// 这里的 2 号就是那种账号：Schedulable、没有任何冷却，所以过得了弱判据，
// 但它的 model_mapping 只认另一个模型，会被候选循环里的模型白名单挡掉。
//
// 选 model_mapping 而不是配额/平台/窗口费用，是因为只有它同时满足三个条件：
// 不被仓库查询层的 IsSchedulable 预先滤掉（配额会）、不被按平台的查询滤掉
// （平台会）、也不对 APIKey 账号直接短路（窗口费用与 RPM 会）。
func TestSelectAccountFailsOpenWhenSurvivorIsFilteredByALaterGate(t *testing.T) {
	parked := mirasimCapacityParkedAccount(1, mirasimCapacityTestModel)

	survivorAccount := mirasimCapacityParkedAccount(2, "") // 无停调 → 过得了弱判据
	survivorAccount.Credentials["model_mapping"] = map[string]any{
		"some-other-model": "some-other-model", // …但白名单里没有被请求的那个模型
	}

	svc, ctx, groupID := mirasimCapacityGatewayFixture(t, []Account{parked, survivorAccount})

	// 前提自证必须打在**流水线里的那份账号**上，不是本地副本：
	// fixture 把账号拷进仓库，而仓库查询层本身还会按平台 + IsSchedulable 滤一轮。
	// （先前两版这条测试分别用了「平台不符」和「配额耗尽」的号 —— 两者都在查询层
	//   就被滤掉、根本没进 accounts，前提为假，测试等于空转。）
	pipelineAccounts, err := svc.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, groupID, PlatformAnthropic)
	require.NoError(t, err)
	require.Len(t, pipelineAccounts, 2, "前提：两个号都必须真的进到候选流水线里")

	var survivor *Account
	for i := range pipelineAccounts {
		if pipelineAccounts[i].ID == 2 {
			survivor = &pipelineAccounts[i]
		}
	}
	require.NotNil(t, survivor)
	require.True(t, survivor.IsSchedulableForModelWithContext(ctx, mirasimCapacityTestModel),
		"前提：这个号必须过得了『可调度性』这道弱判据，缺陷才成立")
	require.False(t, svc.isModelSupportedByAccountWithContext(ctx, survivor, mirasimCapacityTestModel),
		"前提：它必须被候选循环里的模型白名单挡住")

	result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", mirasimCapacityTestModel, nil, "", 0)

	require.NoError(t, err,
		"真正的候选集是空的（唯一的幸存者被模型白名单挡掉），必须退化放行被停调的号")
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
	require.Equal(t, int64(1), result.Account.ID, "退化放行的应当是那个仅被容量停调挡住的号")
}

// fail-open 只放行「唯一原因是容量停调」的账号：窗口耗尽的号必须照旧被挡住，
// 放开它只会换来一次必然的 429。
func TestCapacityFailOpenDoesNotUnblockWindowExhaustedAccounts(t *testing.T) {
	ctx := context.Background()

	capacityOnly := mirasimCapacityParkedAccount(1, mirasimCapacityTestModel)
	require.True(t, mirasimCapacityParkIsSoleBlocker(ctx, &capacityOnly, mirasimCapacityTestModel))

	alsoWindowExhausted := mirasimCapacityParkedAccount(2, mirasimCapacityTestModel)
	setAccountModelRateLimitSnapshot(&alsoWindowExhausted, mirasimClaude7dRateLimitKey,
		time.Now().Add(72*time.Hour), mirasimWindowReason(MirasimWindow7dClaude), time.Now())
	require.False(t, mirasimCapacityParkIsSoleBlocker(ctx, &alsoWindowExhausted, mirasimCapacityTestModel),
		"7d_claude 窗口也耗尽时不能借 fail-open 放行：额度真的没了，放行只会换来 429")

	accountLevelBlocked := mirasimCapacityParkedAccount(3, mirasimCapacityTestModel)
	accountLevelBlocked.Schedulable = false
	require.False(t, mirasimCapacityParkIsSoleBlocker(ctx, &accountLevelBlocked, mirasimCapacityTestModel),
		"账号级不可调度不是容量能解释的")

	plain := plainAnthropicTestAccount()
	require.False(t, mirasimCapacityParkIsSoleBlocker(ctx, plain, mirasimCapacityTestModel),
		"非 mirasim 账号与这条退化路径无关")

	// 放行门本身：严格轮一律不放行，只记一笔；只有入口在严格轮确实选不出账号后
	// 挂上 ignore 标志重选时才放行。
	//
	// 第一版这里断言的是 mirasimCapacityParkFailOpen —— 一个「扫一遍账号预测候选会不会
	// 为空」的事前判据。那个形状本身是错的：真正的候选循环在可调度性之后还有渠道限制、
	// 模型不支持、平台归属、利润门、配额、RPM 六道过滤，预测说「还有候选」而候选集其实
	// 是空的，正好复现这个功能要消灭的形态。现在判据是「严格一轮真的没选出来」。
	strictCtx, obs := withMirasimCapacityParkObserver(ctx)
	require.False(t, mirasimCapacityParkAdmits(strictCtx, &capacityOnly, mirasimCapacityTestModel),
		"严格轮绝不放行：候选到底空不空要等全部过滤跑完才知道")
	require.True(t, obs.blocked.Load(),
		"严格轮必须记下『有号仅被容量停调挡住』，否则入口无从判断该不该退化")

	ignoredCtx, ignoredObs := withMirasimCapacityParkObserver(withMirasimCapacityParkIgnored(ctx))
	require.True(t, mirasimCapacityParkAdmits(ignoredCtx, &capacityOnly, mirasimCapacityTestModel),
		"退化轮放行仅被容量停调挡住的号")
	require.False(t, mirasimCapacityParkAdmits(ignoredCtx, &alsoWindowExhausted, mirasimCapacityTestModel),
		"退化轮也不放行窗口耗尽的号")
	require.False(t, mirasimCapacityParkAdmits(ignoredCtx, plain, mirasimCapacityTestModel),
		"非 mirasim 账号不受这条路径影响")
	_ = ignoredObs

	// 没挂观察器时（正常请求路径，不在选号中）不得 panic，也不放行。
	require.False(t, mirasimCapacityParkAdmits(ctx, &capacityOnly, mirasimCapacityTestModel))
}
