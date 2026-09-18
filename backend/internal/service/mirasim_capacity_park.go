package service

// mirasim 503 `service_capacity_overloaded`：**按模型**停调一小段时间。
//
// ## 缺陷本体（生产实录）
//
// mirasim 的容量池是**每模型独立**的，时段性紧张时上游对某个模型返回
//
//	503 {"error":{"code":"service_capacity_overloaded", ...}}
//
// 而账号本身完全健康 —— 同一时刻线上截图里的一个号：5h=100% / 7d=94% /
// 7d_claude=77% / 7d_fable=99%（额度全都充足），面板却显示「最近 HTTP 503 · 服务错误」。
//
// 此前 503 被归进 MirasimActionRequestScoped（「这次请求的事，不是账号的事」），
// 于是**不写任何冷却**。后果是一个此刻没容量的号会被后续请求反复选中、反复吃 503：
// 每一次都要赔掉一次真实的上游往返，而运营经验里 503 的恢复是**十分钟量级**，
// 不是几秒。
//
// ## 为什么粒度必须是模型，不是账号
//
// 容量池每模型独立：claude-sonnet-5 没容量不代表 claude-fable-5 也没有。
// 把整号停掉会白白废掉一个健康号 —— 这正是 mirasim_capacity_park_test.go 里
// 「差分阴性 1 —— 模型隔离」那条测试钉住的行为。
//
// ## 为什么 scope key 要和窗口冷却分开
//
// 仓库里已有的模型级冷却机制（extra.model_rate_limits + SetModelRateLimit）直接复用，
// 但**不能**复用窗口冷却的 scope（mirasim:7d_claude / mirasim:7d_fable）：
//
//   - 语义不同：窗口耗尽是「这个家族的额度用完了」，容量 503 是「这个模型现在挤」。
//   - 时间尺度不同：窗口是天级（reset 由上游给出），容量是分钟级。
//   - 粒度不同：窗口是家族级（claude 全家共用一个），容量是单模型级。
//
// 混在一个 key 里两者会互相覆盖：一条 10 分钟的容量停调会把一条还剩 3 天的
// 7d_claude 冷却按 shouldPersist 之外的路径改短（模型级 scope 没有「不许缩短」的
// 保护），耗尽的号于是提前 3 天回到候选池。反过来，一条 7 天的窗口冷却也会把
// 一个只是「现在挤」的模型锁死 7 天。
//
// 也不能复用 modelRateLimitKeysForRequest 返回的裸模型名 scope（"claude-sonnet-5"）：
// 那个 scope 已经被 upstream_404_model_not_found / codex plan-gated 占用，
// 共用会让两种完全不同的原因互相顶掉，且 fail-open 会连带放开「这个号根本不支持
// 该模型」的冷却 —— 那不是容量问题，放开只会再烧一次 404。
//
// 所以取 `mirasim:capacity:<mapped-model>`：`mirasim:` 前缀与窗口冷却同源可辨识，
// `capacity:` 段与 `7d_claude` / `7d_fable` 不可能相撞，模型名做后缀保证按模型隔离。

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// mirasimCapacityRateLimitScopePrefix 是容量停调的 scope 前缀，
	// 完整形态 mirasim:capacity:<mapped-model>。理由见文件头注释。
	mirasimCapacityRateLimitScopePrefix = "mirasim:capacity:"

	// mirasimCapacityParkReason 落进 model_rate_limits.<scope>.reason，
	// 是运维在面板上区分「额度用完」与「现在挤」的唯一依据。
	mirasimCapacityParkReason = "mirasim_model_capacity_overloaded"

	// mirasimCapacityParkDefaultMinutes 是停调时长的**上限**默认值。
	//
	// 2026-09-18 之前这个值是 10，语义是「每次 503 一律停调 10 分钟」。实测把那个
	// 语义推翻了（数字见 nextMirasimCapacityParkDuration 的注释）：平坦时长两头都错，
	// 现在它只当阶梯的封顶用。
	mirasimCapacityParkDefaultMinutes = 60

	// mirasimCapacityParkMaxMinutes 给配置值封顶。容量紧张是时段性的，
	// 把它配成小时级等于用一个临时现象换一个长期不可用。
	mirasimCapacityParkMaxMinutes = 1440

	// mirasimCapacityParkBaseDuration 是阶梯的第一档。
	//
	// 取 30 秒不是折中，是**代价不对称**的结论：定短了错，代价是白烧一次换号尝试；
	// 定长了错，代价是自噬整个号池（实测 2026-09-18：103/143 个号同时被容量停调
	// 挡在 fable 之外）。所以从短起步、靠复发升档。
	//
	// 第一档短还有一个附带作用：它把仪器打开了。号被停调期间根本不可能被选中，
	// 于是「这个号多久恢复」这个问题在长停调下永远测不出来 —— 2026-09-18 试图测它
	// 时拿到的「625 次 503 里同号 10 分钟内 0 次成功」正是被 10 分钟停调本身造出来的
	// 读数，不是号的属性。30 秒起步之后这条才有真实样本。
	mirasimCapacityParkBaseDuration = 30 * time.Second

	// mirasimCapacityParkDecayWindow 是阶梯的衰减窗口。
	//
	// 上一档过期之后静默超过这么久再吃 503，就当成一次新的偶发，从第一档重来。
	// 没有衰减的纯升档会把整池推到封顶：实测 503 是**全池均摊**的（24 小时内
	// 104 个号各吃 3–9 次，集中在 5–7 次），不是某几个号坏掉，所以任何只升不降的
	// 计数器最终会把每个号都升到 1 小时。
	mirasimCapacityParkDecayWindow = 30 * time.Minute
)

// mirasimCapacityRateLimitScope 由**映射后**的模型名算出 scope key。
//
// 必须用映射后的模型名：读侧 modelRateLimitKeysForRequest 先做
// a.GetMappedModel(requestedModel) 再把 modelKey 交给 mirasimModelRateLimitKeys，
// 写侧若用请求原名，两边算出的 key 在配了 model_mapping 的账号上就对不上，
// 停调写了却永远读不到 —— 而且这种错法测试全绿（因为默认不配映射）。
//
// 小写化是为了让两侧对同一个模型只有一个 key；模型名本身大小写不敏感。
func mirasimCapacityRateLimitScope(mappedModel string) string {
	model := strings.ToLower(strings.TrimSpace(mappedModel))
	if model == "" {
		return ""
	}
	return mirasimCapacityRateLimitScopePrefix + model
}

// isMirasimCapacityParkScope 判断一个 scope key 是不是容量停调。
// fail-open 靠它把「容量停调」从其它模型级冷却里摘出来。
func isMirasimCapacityParkScope(scope string) bool {
	return strings.HasPrefix(scope, mirasimCapacityRateLimitScopePrefix)
}

// ---------------------------------------------------------------------------
// 配置：默认最高 60 分钟，0 = 完全不 park
// ---------------------------------------------------------------------------

// parseMirasimCapacityParkMinutes 把配置值翻成时长。
//
//	""/非法        → 默认 60 分钟（配置读不出来不该静默变成「不停调」）
//	0              → 0，**完全不写任何冷却**，行为与加这个功能之前逐字节一致。
//	                 这是运维的逃生口：容量紧张形态变了、停调反而伤可用性时，
//	                 一个配置就能退回原状，不需要发版。
//	<0             → 当成非法，取默认值。负数落成「关闭」是个静默陷阱：
//	                 手滑打成 -10 的人想要的是 10 分钟，不是关掉。
//	>上限          → 封顶
func parseMirasimCapacityParkMinutes(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return mirasimCapacityParkDefaultMinutes * time.Minute
	}
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes < 0 {
		return mirasimCapacityParkDefaultMinutes * time.Minute
	}
	if minutes == 0 {
		return 0
	}
	if minutes > mirasimCapacityParkMaxMinutes {
		minutes = mirasimCapacityParkMaxMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// GetMirasimCapacityParkDuration 读 503 容量停调时长。
// 读不到设置（无设置服务 / 仓库报错）一律回落默认值，与 get429FallbackCooldown 同惯例。
func (s *SettingService) GetMirasimCapacityParkDuration(ctx context.Context) time.Duration {
	if s == nil || s.settingRepo == nil {
		return mirasimCapacityParkDefaultMinutes * time.Minute
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyMirasimCapacityParkMinutes)
	if err != nil {
		slog.Warn("mirasim_capacity_park_settings_read_failed", "error", err)
		return mirasimCapacityParkDefaultMinutes * time.Minute
	}
	return parseMirasimCapacityParkMinutes(raw)
}

func (s *RateLimitService) mirasimCapacityParkDuration(ctx context.Context) time.Duration {
	if s == nil || s.settingService == nil {
		return mirasimCapacityParkDefaultMinutes * time.Minute
	}
	return s.settingService.GetMirasimCapacityParkDuration(ctx)
}

// ---------------------------------------------------------------------------
// 阶梯退避
// ---------------------------------------------------------------------------

// nextMirasimCapacityParkDuration 算出这次该停调多久。
//
// 序列是从 mirasimCapacityParkBaseDuration 起倍增、以 capLimit 封顶：
//
//	30s → 1m → 2m → 4m → 8m → 16m → 32m → 60m（默认封顶）
//
// # 档位不用计数器，用上一次的时长
//
// SetModelRateLimit 每次**整体覆写** model_rate_limits.<scope>（account_repo.go:2320
// 的 payload 是固定三个键），所以往那个对象里塞一个 strikes 计数器，下一次写入就没了。
// 但档位本来就可以从已有的两个时间戳算出来：上一次的时长 = reset_at − limited_at。
// 倍增它即可，零 schema 改动、零新字段。
//
// # 历史重置
// 同模型成功由 RecordMirasimModelRecovery 按冷却代次条件清除；其他模型成功
// 不作恢复证据。静默超过配置的衰减窗口也从第一档重新开始。
//
// # 为什么平坦时长两头都错（2026-09-18 实测）
//
//	10 分钟：一个撞上限的请求平均打在 10.71 个号上、其中 8.80 次拿到 503，
//	         每次都写一份 10 分钟停调 —— 一个失败请求连坐停掉 ~9 个号。
//	         结果此刻 fable 池 143 个号里 103 个被我们自己挡住。自噬。
//	 1 分钟：对真的坏掉的号太短，它每分钟回到池子里再烧一次换号预算。
//
// 返回停调时长与上一档时长（上一档为 0 表示这是第一档），后者只用于日志。
func nextMirasimCapacityParkDuration(a *Account, scope string, capLimit time.Duration, now time.Time) (time.Duration, time.Duration) {
	return nextMirasimCapacityParkDurationWithPolicy(a, scope, capLimit, now, mirasimCapacityParkBaseDuration, mirasimCapacityParkDecayWindow)
}

func nextMirasimCapacityParkDurationWithPolicy(a *Account, scope string, capLimit time.Duration, now time.Time, base, decay time.Duration) (time.Duration, time.Duration) {
	if capLimit > 0 && base > capLimit {
		// 运维把上限配得比第一档还短：尊重配置，不要偷偷拉长。
		base = capLimit
	}
	if a == nil || scope == "" {
		return base, 0
	}

	limitedAt := a.modelRateLimitTimestamp(scope, "rate_limited_at")
	resetAt := a.modelRateLimitTimestamp(scope, "rate_limit_reset_at")
	if limitedAt == nil || resetAt == nil {
		return base, 0
	}
	prev := resetAt.Sub(*limitedAt)
	if prev <= 0 {
		// 时间戳自相矛盾（人工改过、时钟回拨）：不拿它推档位。
		return base, 0
	}

	// LastUsedAt is account-wide: success on Opus says nothing about Fable.
	// Only the exact model's successful response may clear its failure history.
	// 规则 2：静默够久 → 衰减归零。上一档还没到期时 now.Sub 为负，判否，继续升档。
	if now.Sub(*resetAt) > decay {
		return base, prev
	}

	next := prev * 2
	if next < base {
		next = base
	}
	if capLimit > 0 && next > capLimit {
		next = capLimit
	}
	return next, prev
}

// ---------------------------------------------------------------------------
// 写侧
// ---------------------------------------------------------------------------

// parkMirasimModelCapacity 在 503 之后把「这个账号 × 这个模型」停调一段时间。
//
// 只写模型级 scope：**不碰**账号级 RateLimitResetAt，也不发
// notifyAccountSchedulingBlocked —— 账号没坏，只是这一个模型此刻挤，
// 把它当成账号级事件会让运维看到一个假的「号被限流了」。
//
// 请求模型从 ctx 取（HandleUpstreamError 入口已经 withTempUnschedulableModel 放进去了）。
// 这样做而不是给 handleMirasimUpstreamError 加参数，是为了不动共享的 ratelimit_service.go
// 调用链；代价是模型名可能为空（比如 count_tokens 这类不带模型的调用点），
// 那种情况下没有可停调的粒度，按「宁可不写」处理。
func (s *RateLimitService) parkMirasimModelCapacity(ctx context.Context, account *Account, statusCode int, responseBody []byte) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}

	capLimit := s.mirasimCapacityParkDuration(ctx)
	if capLimit <= 0 {
		// 配置为 0：逃生口生效，行为与本功能上线前完全一致（不写任何状态）。
		slog.Info("mirasim_capacity_park_disabled",
			"account_id", account.ID,
			"status_code", statusCode,
			"detail", "mirasim_capacity_park_minutes=0，503 不写任何冷却")
		return
	}

	requestedModel := tempUnschedulableModel(ctx, nil)
	scope := mirasimCapacityRateLimitScope(account.GetMappedModel(requestedModel))
	if scope == "" {
		// 不知道是哪个模型就没有「按模型停调」这回事。退成账号级停调是错的：
		// 那正是本功能要避免的「白白废掉一个健康号」。
		slog.Info("mirasim_capacity_park_skipped_no_model",
			"account_id", account.ID,
			"status_code", statusCode,
			"detail", "调用点没带请求模型，无法按模型停调；不退化成账号级")
		return
	}

	now := time.Now()
	// 阶梯退避：档位从上一次的停调时长推出来，不用计数器。判据见
	// nextMirasimCapacityParkDuration。
	policy := config.DefaultMirasimRecoveryConfig()
	if s.cfg != nil {
		if s.cfg.Gateway.MirasimCooldownBaseSeconds > 0 {
			policy.MirasimCooldownBaseSeconds = s.cfg.Gateway.MirasimCooldownBaseSeconds
		}
		if s.cfg.Gateway.MirasimCooldownDecaySeconds > 0 {
			policy.MirasimCooldownDecaySeconds = s.cfg.Gateway.MirasimCooldownDecaySeconds
		}
	}
	ttl, prevTTL := nextMirasimCapacityParkDurationWithPolicy(account, scope, capLimit, now, time.Duration(policy.MirasimCooldownBaseSeconds)*time.Second, time.Duration(policy.MirasimCooldownDecaySeconds)*time.Second)
	resetAt := now.Add(ttl)

	// 已有更长的同 scope 冷却时不缩短。与 shouldPersistAnthropicWindowLimit 同一条
	// 规则：一次新的短冷却不该把一条仍在生效的长冷却改短。
	if existing := account.modelRateLimitResetAt(scope); existing != nil && existing.After(resetAt) {
		slog.Info("mirasim_capacity_park_kept",
			"account_id", account.ID,
			"scope", scope,
			"existing_reset_at", existing.UTC().Format(time.RFC3339))
		return
	}

	if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, scope, resetAt, mirasimCapacityParkReason); err != nil {
		slog.Warn("mirasim_capacity_park_set_failed",
			"account_id", account.ID,
			"scope", scope,
			"reset_at", resetAt,
			"error", err)
		return
	}
	// 让内存里的账号快照与刚写进去的一致，否则同一轮调度决策看不到这条停调。
	setAccountModelRateLimitSnapshot(account, scope, resetAt, mirasimCapacityParkReason, now)

	slog.Info("mirasim_capacity_park_applied",
		"account_id", account.ID,
		"status_code", statusCode,
		"requested_model", requestedModel,
		"scope", scope,
		"park_for", ttl.String(),
		"prev_park_for", prevTTL.String(),
		"park_cap", capLimit.String(),
		"reset_at", resetAt.UTC().Format(time.RFC3339),
		"upstream_code", mirasimUpstreamErrorCode(responseBody),
		"detail", "503 容量池此刻没容量：只停调这个模型，账号对其它模型仍可调度；时长按复发阶梯升档")

}

// mirasimUpstreamErrorCode 只用于日志：把上游 error.code（如
// service_capacity_overloaded）带进记录，方便事后确认 503 的真实成因分布。
// 解析失败返回 ""，绝不影响停调决策 —— 决策只看状态码。
func mirasimUpstreamErrorCode(responseBody []byte) string {
	if len(responseBody) == 0 {
		return ""
	}
	return strings.TrimSpace(extractUpstreamErrorCode(responseBody))
}

// ---------------------------------------------------------------------------
// ★ fail-open：全池 503 停调时的退化
// ---------------------------------------------------------------------------

// mirasimCapacityParkIsSoleBlocker 报告「这个账号此刻不可调度，唯一的原因就是
// 503 容量停调」。
//
// 「唯一」两个字是这条判据的全部价值：fail-open 只能放开容量停调，绝不能顺手放开
// 窗口耗尽（额度真的没了，放开只会换来 429）、404 模型不支持（放开只会再烧一次
// 404）或账号级不可调度（号真的坏了）。所以这里逐个 key 检查，只允许容量 scope
// 处于限流态，其余任何一个 key 还在限流就返回 false。
func mirasimCapacityParkIsSoleBlocker(ctx context.Context, a *Account, requestedModel string) bool {
	if !IsMirasimAccount(a) {
		return false
	}
	// 账号级不可调度（禁用 / 账号级限流 / 临时不可调度）不是容量能解释的。
	if !a.IsSchedulable() {
		return false
	}
	capacityScope := mirasimCapacityRateLimitScope(a.GetMappedModel(requestedModel))
	if capacityScope == "" || !a.isRateLimitActiveForKey(capacityScope) {
		return false
	}
	for _, key := range a.modelRateLimitKeysForRequest(ctx, requestedModel) {
		if isMirasimCapacityParkScope(key) {
			continue
		}
		if a.isRateLimitActiveForKey(key) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// fail-open：判据是「严格一轮真的没选出来」，不是一个更弱的事前预测
// ---------------------------------------------------------------------------
//
// 为什么必须有这条退化：503 是时段性的，容量紧张会**同时**打到很多号上。
// 无条件停调 10 分钟的话，一次全池级紧张会把某个模型的候选全部清空，
// 故障形态就从「慢」变成「10 分钟完全不可用」—— 停调本身把故障放大了。
// 宁可撞 503（一次上游往返），也不要直接返回「无可用账号」（整条链路失败）。
//
// 第一版把这个判据写成了事前预测：先扫一遍账号，只要有任何一个过了
// IsSchedulableForModelWithContext 就判定「还有正常候选」、不退化。那是错的 ——
// 真正的候选循环在它之后还有六道过滤（渠道限制、模型不支持、平台归属、利润门、
// 配额、RPM）。只要某个账号过了前者、卡在后者，预测说「不用退化」而候选集其实是
// 空的，正好复现这个功能本来要消灭的形态。而且那个预测只接进了负载感知一条路径，
// 旧路径（load_batch_enabled=false）上全池停调会直接变成 ErrNoAvailableAccounts
// —— 那是本功能**新引入**的不可用面，加功能之前 503 不写冷却，不会有这种空候选。
//
// 现在的判据没有预测：严格选一轮，真的没选出来、且本轮确实有账号是「只被容量停调
// 挡住」，才把 ignore 标志放进 ctx 重选一轮。因为标志挂在 ctx 上、由
// isAccountSchedulableForModelSelection 这一个点消费，两条路径（含粘性 gate）的
// 全部调用点自动参与，不需要逐个改签名。

// mirasimCapacityParkObserver 记录「本轮有没有账号是只被容量停调挡住的」。
// 它只在选号期间挂在 ctx 上，所以正常请求路径上不会有任何额外开销。
type mirasimCapacityParkObserver struct {
	blocked    atomic.Bool
	earliest   atomic.Int64
	mu         sync.Mutex
	candidates map[int64]time.Time
}

// MirasimCooldownError reports the next selection wake-up. Normal admission
// observes persisted cooldowns; a distributed half-open permit may probe early.
type MirasimCooldownError struct{ RetryAt time.Time }

func (e *MirasimCooldownError) Error() string { return "eligible upstream accounts are cooling down" }
func (e *MirasimCooldownError) Unwrap() error { return ErrNoAvailableAccounts }

type mirasimCapacityParkObserverKey struct{}

type mirasimCapacityParkIgnoreKey struct{}

// withMirasimCapacityParkObserver 给一轮选号挂上观察器。
func withMirasimCapacityParkObserver(ctx context.Context) (context.Context, *mirasimCapacityParkObserver) {
	obs := &mirasimCapacityParkObserver{candidates: make(map[int64]time.Time)}
	return context.WithValue(ctx, mirasimCapacityParkObserverKey{}, obs), obs
}

func mirasimCapacityParkObserverFrom(ctx context.Context) *mirasimCapacityParkObserver {
	obs, _ := ctx.Value(mirasimCapacityParkObserverKey{}).(*mirasimCapacityParkObserver)
	return obs
}

// withMirasimCapacityParkIgnored 标记「这一轮忽略容量停调」。只有在严格一轮确实
// 返回了无可用账号、且观察器报告有账号被停调独挡时才会被设上。
func withMirasimCapacityParkIgnored(ctx context.Context) context.Context {
	return context.WithValue(ctx, mirasimCapacityParkIgnoreKey{}, true)
}

func mirasimCapacityParkIgnored(ctx context.Context) bool {
	ignored, _ := ctx.Value(mirasimCapacityParkIgnoreKey{}).(bool)
	return ignored
}

// mirasimCapacityParkAdmits 是唯一的放行入口，由
// isAccountSchedulableForModelSelection 在常规判据判否之后调用。
//
// 放行范围刻意只限「不可调度的唯一原因就是容量停调」：窗口耗尽、404 模型不支持、
// 账号级不可调度一律照旧拦住 —— 放开那些只会换来另一次必然失败的上游往返。
func mirasimCapacityParkAdmits(ctx context.Context, a *Account, requestedModel string) bool {
	// IsMirasimAccount 是 IsSoleBlocker 的第一道判据，非 mirasim 账号在这里就返回，
	// 所以对其它渠道这条路径的代价是一次类型判断。
	if !mirasimCapacityParkIsSoleBlocker(ctx, a, requestedModel) {
		return false
	}
	if mirasimCapacityParkIgnored(ctx) {
		if only, ok := ctx.Value(mirasimProbeAccountKey{}).(int64); ok && only != a.ID {
			return false
		}
		return true
	}
	// 严格轮：不放行，但记下「退化是有意义的」，供入口决定要不要重选。
	if obs := mirasimCapacityParkObserverFrom(ctx); obs != nil {
		obs.blocked.Store(true)
		if failed := a.modelRateLimitTimestamp(mirasimCapacityRateLimitScope(a.GetMappedModel(requestedModel)), "rate_limited_at"); failed != nil {
			obs.mu.Lock()
			obs.candidates[a.ID] = *failed
			obs.mu.Unlock()
		}
		if reset := a.modelRateLimitResetAt(mirasimCapacityRateLimitScope(a.GetMappedModel(requestedModel))); reset != nil {
			for old := obs.earliest.Load(); old == 0 || reset.UnixNano() < old; old = obs.earliest.Load() {
				if obs.earliest.CompareAndSwap(old, reset.UnixNano()) {
					break
				}
			}
		}
	}
	return false
}
