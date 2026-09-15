package service

// mirasim 计费口径门 —— ACCEPTANCE-mirasim.md §K 的 K1 / K2。
//
// 被测的是**生产计价路径本身**，不是本文件里的复算：
//
//	BillingService.CalculateCost            (billing_service.go:1700)
//	  → calculateCostInternalWithPolicy     (billing_service.go:1722)
//	      → GetModelPricing                 (billing_service.go:1230)  // 价卡从哪来
//	      → computeTokenBreakdown           (billing_service.go:1507)  // 钱怎么算
//	          → computeCacheCreationCost    (billing_service.go:1624)  // 5m/1h 分档
//
// 价卡来源刻意用**真实的**那一份：resources/model-pricing/model_prices_and_context_window.json
// （config.go:2295 的 pricing.fallback_file 默认值），经生产解析路径
// PricingService.parsePricingData 装进 PricingService。目录里没有的条目
// （fable）自然落到 billing_service.go:428-450 的硬编码价卡 —— 那也是生产行为。
// 本文件不自造一份简化价表，否则断言的只是 fixture 自己的算术。
//
// ── 为什么这组断言必须钉「方向」而不只是「有折扣」 ──
// cache_read 的折扣若被算反（0.1x 写成 10x），损失与缓存命中率**同向放大**：
// 命中率越高亏得越多。而我们正在优化命中率。所以每个测试都同时断言
//   (a) 精确倍率，和
//   (b) 与「同 token 数的新鲜输入」相比的大小方向。
// 只断言 (a) 的话，把 0.1 写成 10 仍可能被一个同样算反的期望值掩盖；
// 只断言 (b) 的话，0.1 写成 0.09 抓不住。两条一起才是完整的门。
//
// ── Anthropic 官方口径（K1 判据）──
//	cache_read_input_tokens          = 0.1x  基础输入价
//	cache_creation  5m TTL           = 1.25x 基础输入价
//	cache_creation  1h TTL           = 2x    基础输入价
//
// 唯一的成文例外是 Fable 5.1 的 cache read（$0.25 vs $10 输入 = 0.025x），
// 见 billing_service.go:427-440 的注释与价卡。它比 0.1x 更便宜，因此不违反
// 「缓存必须比重发便宜」这条方向不变量；本文件按模型逐条钉死期望倍率，
// 同时对**所有**模型施加 <= 0.1x 的上限，算反必破上限。

import (
	"fmt"
	"io"
	"log"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 真实价卡文件（config.go:2295 pricing.fallback_file 的默认值）。
const mirasimBillingCatalogPath = "../../resources/model-pricing/model_prices_and_context_window.json"

// Anthropic 官方缓存倍率（相对基础输入价）。
const (
	mirasimCacheReadRatioCeiling = 0.1  // 任何模型的 cache read 都不得高于此
	mirasimCacheWrite5mRatio     = 1.25 // 5m TTL
	mirasimCacheWrite1hRatio     = 2.0  // 1h TTL
)

// 相对误差容限：价卡是 1e-6 量级的浮点，比例运算会带 ulp 级噪声。
const mirasimBillingEpsilon = 1e-9

// mirasimBillingModel 是「mirasim 已启用模型」表的一行。
//
// **口径来源与其局限（重要，判决时请读）**：
// sub2api 侧目前**没有** mirasim 模型白名单 —— ACCEPTANCE-mirasim.md:474-476 (L1)
// 明说白名单只在上游 ma-relay `internal/relay/catalog.go` 里实现，尚未移植。
// 因此本表是 fixture，不是从生产白名单派生的。每行的 source 是它在本仓内的
// 实际出处；L1 落地后本表应改为直接读那份白名单（见文末 TODO）。
type mirasimBillingModel struct {
	model string
	// quotaFamily 对应 ACCEPTANCE-mirasim.md:314-317 的四层窗口分族。
	quotaFamily string
	// cacheReadRatio 该模型 cache_read 相对基础输入价的官方倍率。
	cacheReadRatio float64
	source         string
}

var mirasimBillingEnabledModels = []mirasimBillingModel{
	{
		model:          "claude-opus-5",
		quotaFamily:    "7d_claude",
		cacheReadRatio: 0.1,
		source:         "repository/mirasim_upstream_test.go:160,198,220,321; pkg/mirasim/differential_test.go:131; ACCEPTANCE-mirasim.md:316",
	},
	{
		model:          "claude-sonnet-4-6",
		quotaFamily:    "7d_claude",
		cacheReadRatio: 0.1,
		source:         "service/mirasim_cache_prefix_test.go:36 (mirasimTestModel); ACCEPTANCE-mirasim.md:316 \"sonnet\"",
	},
	{
		model:          "claude-sonnet-5",
		quotaFamily:    "7d_claude",
		cacheReadRatio: 0.1,
		// **证据最弱的一行，读结果时请连这段一起读**：
		// ACCEPTANCE-mirasim.md:316 只写了 "sonnet"，没写完整 id。本仓里带
		// mirasim 归属的 sonnet id 有两个：mirasim_cache_prefix_test.go:36 的
		// claude-sonnet-4-6（那里是按协议族挑的），和 ccore/smoke_test.go:14 的
		// claude-sonnet-5（那里只是签名 smoke 的任意 body 字节，不是目录声明）。
		// 与 claude-opus-5 同代这点让 claude-sonnet-5 更像真的启用型号，故保留。
		// 这一行判红只有两种解释，两种都需要 ma-relay internal/relay/catalog.go 才能定论：
		//   (a) sonnet-5 确实启用 → 真实 K2 缺口，它没有任何显式价卡条目；
		//   (b) 启用的是 sonnet-4-6 → 本 fixture 多列了一行。
		// 计费门宁可保守，不删这一行。
		source: "pkg/mirasim/ccore/smoke_test.go:14（弱证据）; ACCEPTANCE-mirasim.md:316 \"sonnet\"",
	},
	{
		model:          "claude-haiku-4-5",
		quotaFamily:    "7d_claude",
		cacheReadRatio: 0.1,
		source:         "repository/mirasim_live_test.go:47 (MIRASIM_MODEL 默认值); ACCEPTANCE-mirasim.md:27,316",
	},
	{
		model:       "claude-fable-5-1",
		quotaFamily: "7d_fable",
		// 成文例外：Fable 5.1 把 cache read 从 $1 降到 $0.25 per MTok，
		// 而输入价仍是 $10 per MTok → 0.025x。见 billing_service.go:427-440。
		cacheReadRatio: 0.025,
		source:         "ACCEPTANCE-mirasim.md:317; billing_service.go:437-446 (claude-fable-5-1 价卡)",
	},
}

// newMirasimBillingService 用**真实**价卡目录 + 生产解析路径构造计费服务。
func newMirasimBillingService(t *testing.T) *BillingService {
	t.Helper()
	body, err := os.ReadFile(mirasimBillingCatalogPath)
	require.NoError(t, err, "真实价卡文件必须可读；读不到则本文件全部断言失去意义")
	return NewBillingService(&config.Config{}, newStubPricingServiceFromJSON(t, string(body)))
}

// mirasimBillingBaseline 取该模型的价卡与「n 个新鲜输入 token」的基准费用。
// 所有缓存倍率都以这个基准为分母，避免各测试各写一份手算常数。
func mirasimBillingBaseline(t *testing.T, svc *BillingService, model string, n int) (*ModelPricing, *CostBreakdown) {
	t.Helper()
	pricing, err := svc.GetModelPricing(model)
	require.NoErrorf(t, err, "%s 取不到价卡", model)
	require.Positivef(t, pricing.InputPricePerToken, "%s 基础输入价为 0，倍率无从谈起", model)

	fresh, err := svc.CalculateCost(model, UsageTokens{InputTokens: n}, 1.0)
	require.NoErrorf(t, err, "%s 基准输入费用算不出", model)
	require.Positivef(t, fresh.InputCost, "%s 基准输入费用为 0", model)
	return pricing, fresh
}

// TestMirasimBillingCacheReadChargedAtTenPercentOfInput 钉 K1 的 cache read 一侧。
//
// 两层断言：
//  1. 方向 —— 同样 n 个 token，走缓存的实际记账必须**严格小于**重发。算反（10x）
//     时这条先破，且它正是「命中率越高亏越多」的直接否定。
//  2. 精确 —— CacheReadCost 必须恰好是基准输入费用的 cacheReadRatio 倍，
//     且价卡里的 CacheReadPricePerToken 对 InputPricePerToken 同比例。
//     另加 <= 0.1x 的全模型上限，任何模型被配成高于官方折扣都会红。
func TestMirasimBillingCacheReadChargedAtTenPercentOfInput(t *testing.T) {
	// [[cov:BL:cache-read-discount]]
	svc := newMirasimBillingService(t)
	const n = 100_000

	for _, m := range mirasimBillingEnabledModels {
		t.Run(m.model, func(t *testing.T) {
			pricing, fresh := mirasimBillingBaseline(t, svc, m.model, n)

			cached, err := svc.CalculateCost(m.model, UsageTokens{CacheReadTokens: n}, 1.0)
			require.NoError(t, err)

			// (1) 方向门
			require.Lessf(t, cached.CacheReadCost, fresh.InputCost,
				"%s: %d 个 token 走 cache_read 记了 %.10f，重发只要 %.10f —— 折扣方向反了，命中率越高亏越多",
				m.model, n, cached.CacheReadCost, fresh.InputCost)
			require.LessOrEqualf(t, pricing.CacheReadPricePerToken,
				mirasimCacheReadRatioCeiling*pricing.InputPricePerToken*(1+mirasimBillingEpsilon),
				"%s: cache_read 单价 %.12g 超过官方 0.1x 上限（输入价 %.12g）",
				m.model, pricing.CacheReadPricePerToken, pricing.InputPricePerToken)

			// (2) 精确倍率：价卡侧与记账侧各一条
			require.InEpsilonf(t, m.cacheReadRatio*pricing.InputPricePerToken, pricing.CacheReadPricePerToken,
				mirasimBillingEpsilon, "%s: cache_read 单价不是输入价的 %gx", m.model, m.cacheReadRatio)
			require.InEpsilonf(t, m.cacheReadRatio*fresh.InputCost, cached.CacheReadCost,
				mirasimBillingEpsilon, "%s: cache_read 记账不是基准输入费用的 %gx", m.model, m.cacheReadRatio)

			// cache_read 必须全额落在 CacheReadCost 上，不得漏进别的分项。
			require.InEpsilonf(t, cached.CacheReadCost, cached.TotalCost, mirasimBillingEpsilon,
				"%s: TotalCost 与 CacheReadCost 不一致，说明 cache_read token 被重复或错位计费", m.model)
		})
	}
}

// TestMirasimBillingCacheWrite5mChargedAt125PercentOfInput 钉 K1 的 5m TTL 一侧。
//
// 走 UsageTokens.CacheCreation5mTokens（billing_service.go:189），
// 由 computeCacheCreationCost 分档到 CacheCreation5mPrice。
// 方向断言用的是「写入是溢价不是折扣」：把 1.25x 误配成 0.8x 之类会直接破。
func TestMirasimBillingCacheWrite5mChargedAt125PercentOfInput(t *testing.T) {
	// [[cov:BL:cache-write-5m]]
	svc := newMirasimBillingService(t)
	const n = 100_000

	for _, m := range mirasimBillingEnabledModels {
		t.Run(m.model, func(t *testing.T) {
			pricing, fresh := mirasimBillingBaseline(t, svc, m.model, n)

			write5m, err := svc.CalculateCost(m.model, UsageTokens{
				CacheCreationTokens:   n,
				CacheCreation5mTokens: n,
			}, 1.0)
			require.NoError(t, err)

			// 方向门：写缓存是溢价
			require.Greaterf(t, write5m.CacheCreationCost, fresh.InputCost,
				"%s: 5m cache_creation 记了 %.10f，比同量新鲜输入 %.10f 还便宜 —— 写入溢价方向反了",
				m.model, write5m.CacheCreationCost, fresh.InputCost)

			require.InEpsilonf(t, mirasimCacheWrite5mRatio*pricing.InputPricePerToken, pricing.CacheCreation5mPrice,
				mirasimBillingEpsilon, "%s: CacheCreation5mPrice 不是输入价的 1.25x", m.model)
			require.InEpsilonf(t, mirasimCacheWrite5mRatio*fresh.InputCost, write5m.CacheCreationCost,
				mirasimBillingEpsilon, "%s: 5m cache_creation 记账不是基准输入费用的 1.25x", m.model)
		})
	}
}

// TestMirasimBillingCacheWrite1hChargedAtDoubleInput 钉 K1 的 1h TTL 一侧。
//
// 这条额外守一个静默失败：若价卡的 SupportsCacheBreakdown 为 false，
// computeCacheCreationCost (billing_service.go:1626) 根本不看 1h 明细，
// 1h 写入会被按标准价（= 5m 价）少收 —— 不报错、不告警，只是账少了 37.5%。
// 因此先断言分档开关，再断言倍率与「1h 比 5m 贵」的方向。
func TestMirasimBillingCacheWrite1hChargedAtDoubleInput(t *testing.T) {
	// [[cov:BL:cache-write-1h]]
	svc := newMirasimBillingService(t)
	const n = 100_000

	for _, m := range mirasimBillingEnabledModels {
		t.Run(m.model, func(t *testing.T) {
			pricing, fresh := mirasimBillingBaseline(t, svc, m.model, n)

			require.Truef(t, pricing.SupportsCacheBreakdown,
				"%s: SupportsCacheBreakdown=false，1h TTL 明细会被 computeCacheCreationCost 忽略并按 5m 单价少收", m.model)

			write1h, err := svc.CalculateCost(m.model, UsageTokens{
				CacheCreationTokens:   n,
				CacheCreation1hTokens: n,
			}, 1.0)
			require.NoError(t, err)
			write5m, err := svc.CalculateCost(m.model, UsageTokens{
				CacheCreationTokens:   n,
				CacheCreation5mTokens: n,
			}, 1.0)
			require.NoError(t, err)

			// 方向门：1h 比 5m 贵，且两者都比新鲜输入贵
			require.Greaterf(t, write1h.CacheCreationCost, write5m.CacheCreationCost,
				"%s: 1h cache_creation (%.10f) 不比 5m (%.10f) 贵 —— TTL 档位算反或被合并",
				m.model, write1h.CacheCreationCost, write5m.CacheCreationCost)

			require.InEpsilonf(t, mirasimCacheWrite1hRatio*pricing.InputPricePerToken, pricing.CacheCreation1hPrice,
				mirasimBillingEpsilon, "%s: CacheCreation1hPrice 不是输入价的 2x", m.model)
			require.InEpsilonf(t, mirasimCacheWrite1hRatio*fresh.InputCost, write1h.CacheCreationCost,
				mirasimBillingEpsilon, "%s: 1h cache_creation 记账不是基准输入费用的 2x", m.model)
		})
	}
}

// TestMirasimBillingEveryEnabledModelHasExplicitPricing 钉 K2。
//
// 判据函数用 HasIdentifiedTokenPricing (billing_service.go:1211) 而不是
// GetModelPricing：后者会让任意含 "opus"/"haiku"/"claude" 的名字落到
// getFallbackPricing (billing_service.go:921) 的**按子串猜系列**兜底价上，
// 因此它永远返回一个价，用它做准入判断等于本门恒绿。
// HasIdentifiedTokenPricing 只认两种确定性来源：价卡目录里被确定性识别的条目，
// 或 fallbackPrices 里以该模型名为**精确 key** 的显式条目。
//
// 本测试自带仪器正对照：一组没配价、但名字长得像同族的模型必须被判 false，
// 且 GetModelPricing 对它们仍返回一个猜出来的价 —— 那正是本门要挡的形态
// （「加了新模型忘了配价，于是按默认价算，账对不上」）。
func TestMirasimBillingEveryEnabledModelHasExplicitPricing(t *testing.T) {
	// [[cov:BL:no-price-fallback]]
	svc := newMirasimBillingService(t)

	// 仪器正对照先跑：这些必须是 false，否则下面的 true 断言没有信息量。
	for _, unconfigured := range []string{"claude-opus-9", "claude-sonnet-9-9", "claude-haiku-9", "claude-fable-9"} {
		require.Falsef(t, svc.HasIdentifiedTokenPricing(unconfigured),
			"仪器失效：未配价的 %s 被判为有显式定价，本门将恒绿", unconfigured)
		guessed, err := svc.GetModelPricing(unconfigured)
		require.NoErrorf(t, err, "正对照前提：GetModelPricing 对 %s 应当猜出一个价", unconfigured)
		require.NotNilf(t, guessed, "正对照前提：%s 的猜测价卡不应为 nil", unconfigured)
		require.Positivef(t, guessed.InputPricePerToken,
			"正对照前提：%s 会被按默认价兜底计费（这正是 K2 要挡的形态）", unconfigured)
	}

	for _, m := range mirasimBillingEnabledModels {
		t.Run(m.model, func(t *testing.T) {
			require.Truef(t, svc.HasIdentifiedTokenPricing(m.model),
				"%s（%s 窗口，来源 %s）在价卡目录与 fallbackPrices 里都没有显式条目，"+
					"会被 getFallbackPricing 按子串猜成同族默认价 —— 账对不上",
				m.model, m.quotaFamily, m.source)
		})
	}
}

// TestMirasimBillingUnpricedModelGetsStablePriceCard
//
// **无 cov 标签** —— 这不是四条义务之一，是跑 BL:no-price-fallback 时挖出来的
// 相邻缺陷，放在这里是因为它正是 K2 必须是「门」而不是「观测」的理由。
//
// 缺陷：matchByModelFamily 的 Phase 3（pricing_service.go:1428-1433）用
//
//	for key, pricing := range s.pricingData { if strings.Contains(keyLower, pattern) { return pricing } }
//
// 在 Go map 上取「第一个子串命中」。map 迭代顺序是随机化的，所以**同一进程内、
// 同一个未配价模型名、连续两次查询会拿到不同的价卡条目**。
// pricing_service.go:1338-1340 的注释说这类 map 随机性已被修（families 改成有序
// 切片），但那只修了 Phase 2 的系列归类，Phase 3 的价卡选取仍在裸 map 上。
//
// 实测（本仓当前 resources/model-pricing 目录）：`claude-sonnet-5` 会在
// claude-sonnet-4-20250514 / claude-sonnet-4-5 / claude-sonnet-4-5-20250929 /
// claude-sonnet-4-5-20250929-v1:0 / claude-sonnet-4-6 五者之间随机落点。
// 前四者带 above_200k 长上下文阶梯（>200k 输入价 2x），claude-sonnet-4-6 不带 ——
// 于是一个 250k 上下文的请求，收 1x 还是 2x 取决于那一刻的 map 迭代顺序。
//
// 断言方向是**期望行为**（价卡必须稳定），所以修好之后这条会转绿，而不是反过来。
//
// 抽样为什么要跨多个新建的 PricingService：Go 的 `range` 随机化的是**起始桶**，
// 而桶布局由这张 map 实例的 hash 种子定死。某个种子下「命中 claude-sonnet-4-6」
// 的概率可能低到近似 0（实测单进程内少数派占比在 2/200 ~ 53/200 之间漂），
// 只在一张 map 上抽样有几率抽不到少数派 → 假绿。因此每轮重建目录（一次 ~2.5ms），
// 换一张 map 布局再抽，抽不到的那种运气不会连着 8 张都发生。
func TestMirasimBillingUnpricedModelGetsStablePriceCard(t *testing.T) {
	// 探针用合成名而非 claude-sonnet-5：缺陷与「哪个 sonnet 才是 mirasim 启用的」无关。
	const probe = "claude-sonnet-9-9"
	require.False(t, newMirasimBillingService(t).HasIdentifiedTokenPricing(probe),
		"探针前提：%s 必须是未配价模型，否则本测试测不到 fallback 路径", probe)

	// 每次 fallback 命中都会 LegacyPrintf 一行，静音掉以免淹没测试输出。
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	const (
		catalogRebuilds  = 8
		probesPerCatalog = 250
	)
	seen := make(map[string]int)
	for range catalogRebuilds {
		svc := newMirasimBillingService(t)
		for range probesPerCatalog {
			pricing, err := svc.GetModelPricing(probe)
			require.NoError(t, err)
			seen[fmt.Sprintf("in=%g out=%g cacheRead=%g cw5m=%g cw1h=%g longCtx=%d@%gx",
				pricing.InputPricePerToken, pricing.OutputPricePerToken, pricing.CacheReadPricePerToken,
				pricing.CacheCreation5mPrice, pricing.CacheCreation1hPrice,
				pricing.LongContextInputThreshold, pricing.LongContextInputMultiplier)]++
		}
	}

	require.Lenf(t, seen, 1,
		"对同一个未配价模型 %s 查 %d 次（跨 %d 张重建的价卡目录），拿到 %d 张**不同**的价卡：%v —— "+
			"计价结果依赖 Go map 迭代顺序，同一请求重放会得到不同金额",
		probe, catalogRebuilds*probesPerCatalog, catalogRebuilds, len(seen), seen)
}

// TODO(L1): ACCEPTANCE-mirasim.md:474-476 的模型白名单落地到 sub2api 后，
// mirasimBillingEnabledModels 应改为直接遍历那份生产白名单，本门才真正能抓住
// 「新加模型忘了配价」—— 现在白名单只在上游 ma-relay 里，加了新模型不会让本表变化。
