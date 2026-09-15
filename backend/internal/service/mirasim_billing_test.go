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
	return NewBillingService(&config.Config{}, newMirasimPricingCatalog(t))
}

// newMirasimPricingCatalog 用**真实**价卡文件 + 生产解析路径构造价卡目录。
// 每次调用都重建一份（含新的 map 实例），供需要跨 map 布局抽样的确定性测试使用。
func newMirasimPricingCatalog(t *testing.T) *PricingService {
	t.Helper()
	body, err := os.ReadFile(mirasimBillingCatalogPath)
	require.NoError(t, err, "真实价卡文件必须可读；读不到则本文件全部断言失去意义")
	return newStubPricingServiceFromJSON(t, string(body))
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

// sonnet5FallbackCatalogJSON 是一份**不含 claude-sonnet-5 条目**的最小目录，
// 用来固定住"目录里没有该型号"这个状态 —— 修复前的 bundled 目录就是这个状态，
// 任何还在用旧 price-mirror 的部署也是。条目内容照抄真实目录里的
// claude-sonnet-4-20250514（含 above_200k 阶梯字段），走生产解析路径折算成
// 阈值 200k + 2x 倍率。不用真实目录文件，是因为那份文件随 price-mirror 刷新而变，
// 本测试要钉的恰恰是"目录缺该型号时"的行为。
const sonnet5FallbackCatalogJSON = `{
	"claude-sonnet-4-20250514": {"litellm_provider": "anthropic", "mode": "chat",
		"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
		"cache_creation_input_token_cost": 3.75e-06,
		"cache_creation_input_token_cost_above_1hr": 6e-06,
		"cache_read_input_token_cost": 3e-07,
		"input_cost_per_token_above_200k_tokens": 6e-06,
		"output_cost_per_token_above_200k_tokens": 2.25e-05,
		"cache_read_input_token_cost_above_200k_tokens": 6e-07,
		"cache_creation_input_token_cost_above_200k_tokens": 7.5e-06}
}`

// TestMirasimBillingSonnet5FallsBackToOwnCardNotSonnet4Ladder
//
// **无 cov 标签** —— 它是 BL:no-price-fallback 判红后修复的回归锁，不是第五条义务。
//
// 缺陷形态：claude-sonnet-5 在 fallbackPrices 里没有条目时，matchByModelFamily 的
// Phase 2 关键字兜底把它归到 "sonnet-4" 系列，于是借用 claude-sonnet-4-20250514 的价卡。
// 那张卡的基础单价是 $3/$15（sonnet-5 实际是 $2/$10，已经多收 50%），更隐蔽的是它还带
// above_200k 长上下文阶梯（阈值 200k、倍率 2x），Sonnet 5 没有 —— 超过 200k 上下文的
// sonnet-5 请求被整次会话再翻一倍，不报错、不告警。
//
// 显式价卡数字来源：Anthropic 官方价目表
// https://platform.claude.com/docs/en/about-claude/pricing（2026-09-16 核对）
// "Claude Sonnet 5" 行 = $2 / $2.50(5m) / $4(1h) / $0.20(read) / $15→$10(output)，
// 即 $2/$10 已是 standard price，2026-09-01 的涨价计划被官方撤销。
// 见 billing_service.go initFallbackPricing 中 claude-sonnet-5 条目的注释。
//
// 仪器正对照：先断言 claude-sonnet-4-20250514 这张"被借用的"价卡在本目录里确实带
// 2x 阶梯、且确实会把 250k 请求收成两倍。正对照不成立时，下面 sonnet-5 的
// "没有阶梯"断言没有信息量。
func TestMirasimBillingSonnet5FallsBackToOwnCardNotSonnet4Ladder(t *testing.T) {
	svc := NewBillingService(&config.Config{}, newStubPricingServiceFromJSON(t, sonnet5FallbackCatalogJSON))
	const (
		sonnet5      = "claude-sonnet-5"
		borrowedCard = "claude-sonnet-4-20250514" // 修复前 sonnet-5 会落到的那张卡
		longCtx      = 250_000                    // > 200k 阈值
	)

	// —— 仪器正对照：被借用的那张卡确有 2x 长上下文阶梯 ——
	borrowed, err := svc.GetModelPricing(borrowedCard)
	require.NoError(t, err)
	require.EqualValues(t, 200_000, borrowed.LongContextInputThreshold,
		"正对照前提：%s 应带 200k 长上下文阈值，否则本测试无法区分两张卡", borrowedCard)
	require.EqualValues(t, 2, borrowed.LongContextInputMultiplier,
		"正对照前提：%s 的长上下文倍率应为 2x", borrowedCard)
	borrowedCost, err := svc.CalculateCost(borrowedCard, UsageTokens{InputTokens: longCtx}, 1.0)
	require.NoError(t, err)
	require.InEpsilon(t, float64(longCtx)*borrowed.InputPricePerToken*2, borrowedCost.InputCost, mirasimBillingEpsilon,
		"正对照前提：%s 的 %d token 请求应被整次会话按 2x 计价", borrowedCard, longCtx)

	// —— 被测：目录缺该型号时，sonnet-5 走自己的显式价卡 ——
	require.True(t, svc.HasIdentifiedTokenPricing(sonnet5),
		"%s 必须有显式定价条目，否则又会按子串猜成同族默认价", sonnet5)

	// 带日期/变体后缀的 id 也必须落到同一张卡，而不是滑回 sonnet-4 系列。
	//
	// 价格出处：Anthropic 官方定价页（docs.anthropic.com/en/docs/about-claude/pricing，
	// 2026-09-16 核对）。Sonnet 5 是 $2/$10，**不是** sonnet-4.5/4.6 那档的 $3/$15。
	// 官方在同一页明确写了这一点，防止有人按"同族应该同价"改回去：
	//
	//   "The $2/$10 per million input/output token pricing for Claude Sonnet 5,
	//    announced at launch as introductory pricing through August 31, 2026,
	//    is now the standard price. The previously scheduled increase to $3/$15
	//    per million input/output tokens on September 1, 2026 will not occur."
	//
	// 这正是这条测试要钉住的事：sonnet-5 比 sonnet-4 系列**便宜**，按同族默认价
	// 猜会多收 50%。
	for _, id := range []string{sonnet5, "claude-sonnet-5-20260701", "claude-sonnet-5-thinking"} {
		pricing, err := svc.GetModelPricing(id)
		require.NoErrorf(t, err, "%s 取不到价卡", id)
		require.InEpsilonf(t, 2e-6, pricing.InputPricePerToken, mirasimBillingEpsilon, "%s 输入价应为 $2/MTok", id)
		require.InEpsilonf(t, 10e-6, pricing.OutputPricePerToken, mirasimBillingEpsilon, "%s 输出价应为 $10/MTok", id)
		require.InEpsilonf(t, 0.2e-6, pricing.CacheReadPricePerToken, mirasimBillingEpsilon, "%s cache read 应为 $0.20/MTok", id)
		require.InEpsilonf(t, 2.5e-6, pricing.CacheCreation5mPrice, mirasimBillingEpsilon, "%s 5m cache write 应为 $2.50/MTok", id)
		require.InEpsilonf(t, 4e-6, pricing.CacheCreation1hPrice, mirasimBillingEpsilon, "%s 1h cache write 应为 $4/MTok", id)
		require.Truef(t, pricing.SupportsCacheBreakdown,
			"%s 未开 5m/1h 分档，1h 写入会被 computeCacheCreationCost 按 5m 单价少收", id)

		// 关键差异：sonnet-5 不得继承 sonnet-4 的长上下文阶梯。
		require.Zerof(t, pricing.LongContextInputThreshold,
			"%s 借到了长上下文阶梯（阈值 %d），250k 请求会被多收一倍", id, pricing.LongContextInputThreshold)
	}

	cost, err := svc.CalculateCost(sonnet5, UsageTokens{InputTokens: longCtx}, 1.0)
	require.NoError(t, err)
	require.InEpsilon(t, float64(longCtx)*2e-6, cost.InputCost, mirasimBillingEpsilon,
		"%d token 的 sonnet-5 请求记了 %.10f，应为 %.10f（无长上下文溢价）",
		longCtx, cost.InputCost, float64(longCtx)*2e-6)
	require.Less(t, cost.InputCost, borrowedCost.InputCost,
		"sonnet-5 与被借用的 sonnet-4 卡在 %d token 上记了同样的钱 —— 说明仍在共用那张带阶梯的卡", longCtx)
}

// TestMirasimBillingSonnet5BilledAtCurrentlyPublishedRate
//
// 断言的是端到端实际计价单价（无论价卡来自目录还是 fallbackPrices），对照
// Anthropic 官方价目表 https://platform.claude.com/docs/en/about-claude/pricing
// （2026-09-16 核对）现行行：
//
//	Claude Sonnet 5   $2 / $2.50 / $4 / $0.20 / $10   （input / 5m write / 1h write / cache read / output，每 MTok）
//
// **曾经写在这里的 "2026-09-01 起涨到 $3/$15" 是一条已被官方撤销的公告**，官方在
// 同一页写死了这一点：
//
//	"The $2/$10 per million input/output token pricing for Claude Sonnet 5,
//	 announced at launch as introductory pricing through August 31, 2026,
//	 is now the standard price. The previously scheduled increase to $3/$15
//	 per million input/output tokens on September 1, 2026 will not occur."
//
// 所以 $2/$10 就是现行价，目录条目与 fallbackPrices 都按这一档配；本测试防的是
// 有人按"同族应该同价"把 sonnet-5 改成 sonnet-4.5/4.6 那档的 $3/$15，凭空多收 50%。
//
// 若将来官方真的调价，改的是这里的期望值 + billing_service.go 的 fallbackPrices，
// 不是把断言放宽成"等于目录里写的任何值"（那样这条门就永远恒绿）。
func TestMirasimBillingSonnet5BilledAtCurrentlyPublishedRate(t *testing.T) {
	svc := newMirasimBillingService(t)
	const sonnet5 = "claude-sonnet-5"

	pricing, err := svc.GetModelPricing(sonnet5)
	require.NoError(t, err)

	require.InEpsilon(t, 2e-6, pricing.InputPricePerToken, mirasimBillingEpsilon,
		"sonnet-5 输入价按 %.12g 计，官方现行价为 2e-06（$2/MTok）", pricing.InputPricePerToken)
	require.InEpsilon(t, 10e-6, pricing.OutputPricePerToken, mirasimBillingEpsilon,
		"sonnet-5 输出价按 %.12g 计，官方现行价为 1e-05（$10/MTok）", pricing.OutputPricePerToken)
	require.InEpsilon(t, 0.2e-6, pricing.CacheReadPricePerToken, mirasimBillingEpsilon,
		"sonnet-5 cache read 按 %.12g 计，官方现行价为 2e-07（$0.20/MTok）", pricing.CacheReadPricePerToken)
	require.InEpsilon(t, 2.5e-6, pricing.CacheCreation5mPrice, mirasimBillingEpsilon,
		"sonnet-5 5m cache write 按 %.12g 计，官方现行价为 2.5e-06（$2.50/MTok）", pricing.CacheCreation5mPrice)
	require.InEpsilon(t, 4e-6, pricing.CacheCreation1hPrice, mirasimBillingEpsilon,
		"sonnet-5 1h cache write 按 %.12g 计，官方现行价为 4e-06（$4/MTok）", pricing.CacheCreation1hPrice)
}

// TestMirasimBillingIdentifiedPriceCardIsStableAcrossLookups
//
// **无 cov 标签** —— 与上面那条 map 随机性一样，是修 K2 时发现的相邻缺陷的回归锁。
//
// 上面那条钉的是 matchByModelFamily 的"猜系列"路径；这条钉的是它的兄弟：
// lookupIdentifiedModelPricingLocked（pricing_service.go）第 3 步按 extractBaseName
// 匹配时同样 `range` 裸 map。同一个 baseName 在本仓价卡目录里对应多条：
// claude-sonnet-4-5 / claude-sonnet-4-5-20250929 / claude-sonnet-4-5-20250929-v1:0，
// 它们的 token 单价相同但 litellm_provider 不同（anthropic vs bedrock），
// 而 provider 会进入 ModelPricing（如 LongContextThresholdInclusive 的 xAI 判定）。
// 一个价卡目录尚未收录的新日期版本（claude-sonnet-4-5-<新日期>）就会随机落到其中一张。
//
// 抽样跨 8 张重建的目录，理由同 TestMirasimBillingUnpricedModelGetsStablePriceCard：
// Go 的 range 随机化起始桶，单张 map 的桶布局可能让少数派概率极低 → 假绿。
func TestMirasimBillingIdentifiedPriceCardIsStableAcrossLookups(t *testing.T) {
	// 未收录的日期版本：精确键与 -4.5- 拼写变体都查不到，必然走第 3 步 baseName 匹配。
	const probe = "claude-sonnet-4-5-20991231"

	const (
		catalogRebuilds  = 8
		probesPerCatalog = 250
	)
	seen := make(map[string]int)
	for range catalogRebuilds {
		catalog := newMirasimPricingCatalog(t)
		_, exact := catalog.pricingData[probe]
		require.Falsef(t, exact, "探针前提：%s 不能是目录里的精确条目，否则测不到 baseName 匹配", probe)

		for range probesPerCatalog {
			pricing := catalog.GetIdentifiedModelPricing(probe)
			require.NotNilf(t, pricing, "探针前提：%s 应能按 baseName 命中同族条目", probe)
			seen[fmt.Sprintf("provider=%s in=%g out=%g cacheRead=%g cw1h=%g",
				pricing.LiteLLMProvider, pricing.InputCostPerToken, pricing.OutputCostPerToken,
				pricing.CacheReadInputTokenCost, pricing.CacheCreationInputTokenCostAbove1hr)]++
		}
	}

	require.Lenf(t, seen, 1,
		"对同一个模型名 %s 查 %d 次（跨 %d 张重建的价卡目录），拿到 %d 张**不同**的价卡：%v —— "+
			"确定性识别路径同样依赖 Go map 迭代顺序",
		probe, catalogRebuilds*probesPerCatalog, catalogRebuilds, len(seen), seen)
}

// TODO(L1): ACCEPTANCE-mirasim.md:474-476 的模型白名单落地到 sub2api 后，
// mirasimBillingEnabledModels 应改为直接遍历那份生产白名单，本门才真正能抓住
// 「新加模型忘了配价」—— 现在白名单只在上游 ma-relay 里，加了新模型不会让本表变化。
