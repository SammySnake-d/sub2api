package service

// 出口代理池（resin）部署配置的验收门。
//
// 被测对象是仓库里的部署真源 deploy/resin-runtime.json —— 不是线上那份活配置。
// 这一点是刻意的：线上值可以被任何人 curl 改掉，而「改了什么、为什么」留不下来。
// 把真源放进仓库、由 deploy/apply-resin-runtime.sh 推上去并回读校验，
// 这几条判决才有一个能被 review、能被测试、能被追溯的落点。
//
// 两条被钉住的判决都是踩过坑换来的，注释里写了坑本身，因为下一个人最可能
// 「顺手改回去」的正是它们。

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const resinRuntimeConfigPath = "../../../deploy/resin-runtime.json"

// resinPlatformConfigPath 是**平台级**配置的真源，与上面那份是两个不同的控制面端点：
//
//	resin-runtime.json   → PATCH /api/v1/system/config   （单例：延迟天花板、租约上限…）
//	resin-platforms.json → PATCH /api/v1/platforms/{id}  （每平台：订阅源白名单、地区过滤）
//
// 分两份不是为了整齐：平台是可增删的实体，系统配置是单例。混在一起会让
// 「加一个平台」变成改一个全局对象。
const resinPlatformConfigPath = "../../../deploy/resin-platforms.json"

// resinRuntimeConfig 只解出本门要判的那几个键。
// 不用 map[string]any 全量比较：那会让任何一次无关键的增删都变成红，
// 门就会因为太吵而被关掉。
type resinRuntimeConfig struct {
	LatencyTestURL          string   `json:"latency_test_url"`
	LatencyAuthorities      []string `json:"latency_authorities"`
	MaxLeasesPerIP          int      `json:"max_leases_per_ip"`
	MaxRoutableLatencyMs    int      `json:"max_routable_latency_ms"`
	MaxConsecutiveFailures  int      `json:"max_consecutive_failures"`
	ReverseProxyLogDetailOn bool     `json:"reverse_proxy_log_detail_enabled"`
}

func loadResinRuntimeConfig(t *testing.T) resinRuntimeConfig {
	t.Helper()
	raw, err := os.ReadFile(resinRuntimeConfigPath)
	require.NoError(t, err, "读不到部署真源 %s —— 本文件所有断言都失去意义", resinRuntimeConfigPath)
	var cfg resinRuntimeConfig
	require.NoError(t, json.Unmarshal(raw, &cfg), "%s 不是合法 JSON，apply 脚本会直接失败", resinRuntimeConfigPath)
	return cfg
}

// TestResinLatencyProbeTargetIsALightweight204 钉住「延迟靶子指哪」。
//
// 这条曾经被配错过，代价是实测一次请求 3s → 170s：上游域名 relay.mirasim.ai 被填进
// latency_test_url，而它对裸 GET 不返回可用响应，于是每个节点的延迟探测全部超时，
// 所有节点看起来都极慢，P2C 选点退化成随机。
//
// 正确的分工是两件事，不能混：
//
//	latency_test_url    = 主动打的**轻量 204 探针**（gstatic/generate_204）
//	latency_authorities = 哪些域名的延迟算权威（由真实流量被动采样）
func TestResinLatencyProbeTargetIsALightweight204(t *testing.T) {
	// [[cov:DP:latency-probe-target]]
	cfg := loadResinRuntimeConfig(t)

	require.Equal(t, "https://www.gstatic.com/generate_204", cfg.LatencyTestURL,
		"延迟探测靶子必须是轻量 204 探针。填成一个不能裸 GET 的域名时，"+
			"所有节点的探测一起超时，选点退化成随机 —— 实测代价 3s → 170s")

	// 上游域名只能出现在 authorities，不能出现在探测靶子里。
	// 这条差分断言是本门的鉴别力所在：只断言 latency_test_url 等于 gstatic，
	// 挡不住「有人把上游域名**同时**写进 test_url 的 query 或路径里」这种改法。
	require.NotContains(t, cfg.LatencyTestURL, "mirasim",
		"上游域名不得出现在主动探测靶子里 —— 它的落点是 latency_authorities")
	require.Contains(t, cfg.LatencyAuthorities, "relay.mirasim.ai",
		"上游域名必须在 latency_authorities 里，否则到上游的真实延迟不参与选点与熔断")
	require.Contains(t, cfg.LatencyAuthorities, "gstatic.com",
		"探针域名也要在 authorities 里，否则主动探到的延迟不算数")
}

// TestResinCapsAccountsPerEgressIP 钉住「一个出口 IP 上压几个号」。
//
// 运营者口径：2-3 个号一个 IP。设 0（不限）时 PREFER_LOW_LATENCY 会把 143 个账号
// 全堆到唯一最快的那个出口 IP 上 —— 上游看到的就是「一个设备在跑 143 个账号」，
// 这恰好是整套画像隔离要防的那件事。
func TestResinCapsAccountsPerEgressIP(t *testing.T) {
	// [[cov:DP:leases-per-ip-max-3]]
	cfg := loadResinRuntimeConfig(t)

	require.Equal(t, 3, cfg.MaxLeasesPerIP,
		"同一出口 IP 上的账号数上限必须是 3（运营者口径 2-3 个号一个 IP）")
	// 0 是「不限」的哨兵值，单独钉一遍：它和「配小了」是完全不同的失效模式 ——
	// 前者会静默地把所有账号堆到一个 IP 上，后者只是容量紧一点。
	require.NotEqual(t, 0, cfg.MaxLeasesPerIP,
		"0 表示不限：PREFER_LOW_LATENCY 会把所有账号堆到唯一最快的 IP 上")

	// 与之配套的两个健康判据一并钉住 —— 它们仨一起才构成「坏节点不会长期挂着账号」。
	require.Equal(t, 300, cfg.MaxRoutableLatencyMs,
		"延迟天花板；超过即熔断。300 是运营口径（每个出口 IP 的节点延迟要在 300ms 内），"+
			"拓扑换到海外 resin 之后才可行：可路由节点 56→4323，健康出口 IP 71→1888，p50 424ms→185ms")
	require.Equal(t, 1, cfg.MaxConsecutiveFailures,
		"一次失败即熔断。配合 resin 的单次请求内换节点重试，坏节点赔掉的那次 dial "+
			"由代理层自己吸收，不冒泡成 502 让上游网关误判成账号问题")
}

// TestResinLatencyCeilingDoesNotClaimToEvictExistingLeases 钉住一条**认知边界**，
// 而不是一个配置值。
//
// 上面那条把天花板钉在 300，很容易被读成「所以不会有超过 300ms 的节点挂着账号」。
// 实测（2026-09-16）恰好相反：143 个 sticky 租约里 18 个的参考延迟在 300 之上，
// p90=495ms，max=3042ms。原因是天花板只作用于**新选点**，而 resin 的 LeaseCleaner
// 只按 TTL（168h）回收租约，不看节点是否已熔断或已超天花板。一个 640ms、已熔断的
// 节点把某个账号钉了一周，那个账号的控制面探测要 97s；手工释放租约后 0.73s。
//
// 这条门守的是：部署真源的 rationale 里**必须**写着这个限制。把它删掉的人会让
// 下一个人相信「配了天花板就够了」，然后在同一个坑里再花一天。
func TestResinLatencyCeilingDoesNotClaimToEvictExistingLeases(t *testing.T) {
	raw, err := os.ReadFile(resinRuntimeConfigPath)
	require.NoError(t, err)
	text := string(raw)

	require.Contains(t, text, "只管新选点",
		"部署真源必须写明延迟天花板不回收已有 sticky 租约 —— 少了这句，"+
			"下一个人会以为配了天花板就不会有超标节点挂着账号")
	require.Contains(t, text, "sticky",
		"同上：这个限制的主语是 sticky 租约，真源里要留下这个词才检索得到")
}

// TestResinPlatformSourceOfTruthPinsTheSubscriptionWhitelist 钉住订阅源白名单。
//
// 这是本轮最有效的一刀，也是最容易被「顺手放宽」的一处：名单里少了哪个源，
// 容量看起来完全够（收紧后仍有 ~2931 个可路由节点），失效是静默的 ——
// 只有个别账号偶尔慢到 90 秒，而监控面板上一切正常。
func TestResinPlatformSourceOfTruthPinsTheSubscriptionWhitelist(t *testing.T) {
	raw, err := os.ReadFile(resinPlatformConfigPath)
	require.NoError(t, err, "读不到平台级部署真源 %s", resinPlatformConfigPath)

	var cfg struct {
		Platforms []struct {
			ID            string   `json:"id"`
			Name          string   `json:"_name"`
			RegexFilters  []string `json:"regex_filters"`
			RegionFilters []string `json:"region_filters"`
		} `json:"platforms"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg), "%s 不是合法 JSON，apply 脚本会直接失败", resinPlatformConfigPath)

	var paid *struct {
		ID            string   `json:"id"`
		Name          string   `json:"_name"`
		RegexFilters  []string `json:"regex_filters"`
		RegionFilters []string `json:"region_filters"`
	}
	for i := range cfg.Platforms {
		if cfg.Platforms[i].Name == "Paid" {
			paid = &cfg.Platforms[i]
			break
		}
	}
	require.NotNil(t, paid, "平台真源里必须有 Paid —— mirasim 的 143 个账号全走它")

	require.Len(t, paid.RegexFilters, 1,
		"订阅源白名单应该是一条 ANY 规则。拆成多条也能工作，但那会让「少了哪个源」更难看出来")
	rule := paid.RegexFilters[0]

	// 逐个点名，而不是断言整条字符串相等：后者会让「调整正则写法」和
	// 「悄悄去掉一个源」这两件事都变成同一个红，作者只会照着期望值改。
	for _, sub := range []string{"liangxin-paid", "radikal-fast", "au1rxx", "ldc兑换工艺"} {
		require.Contains(t, rule, sub,
			"订阅源白名单里缺了 %s。这是运营者按长期波动挑的四个源；"+
				"少一个不会让容量不够（收紧后仍有 ~2931 个可路由节点），"+
				"失效是静默的：个别账号偶尔慢到 90 秒，面板上一切正常", sub)
	}

	// 差分保护：规则必须锚在开头并以 / 收尾。
	// 少了锚点，`au1rxx` 会匹配到任何 tag 里含这个串的节点；少了 `/`，
	// 它会匹配到 `au1rxx-mirror` 这类同名前缀的其他源。
	require.True(t, strings.HasPrefix(rule, "^("),
		"订阅源规则必须锚在 ^ —— 否则它会匹配 tag 中间出现的源名，白名单形同虚设。当前：%q", rule)
	require.True(t, strings.HasSuffix(rule, ")/"),
		"订阅源规则必须以 )/ 收尾 —— tag 格式是 `<源名>/<节点名>`，"+
			"少了斜杠会连 `au1rxx-mirror` 这种同前缀的别的源一起放进来。当前：%q", rule)

	// 这条是本文件里唯一一条**跨文件**的不变量：它在别处已经被钉过一遍
	// （mirasim_egress_region_test 等），这里再钉是因为收紧 regex_filters 时
	// 最容易发生的事就是「重写整个 filters 块，顺手把 region 也一起重写了」。
	require.Equal(t, []string{"!cn", "!hk"}, paid.RegionFilters,
		"!cn/!hk 是**唯一**在阻止 CN/HK 出口被选中的机制。订阅源本身含 42 个 cn + 54 个 hk "+
			"健康节点，所以「订阅源里没有 CN 节点」是错误认知，不能依赖它")
}

// TestResinPlatformSourceOfTruthRecordsTheLeaseMigrationGap 是上一条的配套。
//
// 改 regex_filters 最危险的地方不是改错，而是**改对了却以为生效了**：实测
// 2026-09-16，收紧过滤器之后 196 个存量租约仍指向已被过滤掉的节点，一个都没迁移，
// 而 routable_node_count 从 4193 漂亮地降到 2931 —— 面板显示得非常成功。
func TestResinPlatformSourceOfTruthRecordsTheLeaseMigrationGap(t *testing.T) {
	raw, err := os.ReadFile(resinPlatformConfigPath)
	require.NoError(t, err)
	text := string(raw)

	// 判据是**语义关键词的组合**，不是某一句话的字面。
	//
	// 第一版钉的是短语 "不会自动迁移"，当场被自己的文案打红 —— 文案写的是
	// 「不会让已有的 sticky 租约迁走」，意思一模一样。钉字面只会逼着作者照着
	// 期望值改措辞，而那对读者毫无价值。改成要求这几个概念都在场：
	// 存量租约 / 不迁移 / 按 TTL 回收 / 怎么手工释放。
	for _, concept := range []struct{ word, why string }{
		{"sticky", "限制的主语是 sticky 租约，这个词要在，否则检索不到"},
		{"迁", "必须说明租约「不会迁走 / 不迁移」—— 这是整条限制的动词"},
		{"168h", "必须写出 TTL 的实际值，读者才知道「不迁移」要持续多久（7 天）"},
		{"leases", "必须给出手工释放的端点路径，否则读者知道了问题却不知道怎么办"},
	} {
		require.Contains(t, text, concept.word,
			"平台真源缺少关键信息「%s」：%s。\n"+
				"这份配置最危险的失效形态是**改对了却以为生效了** —— 收紧过滤器后 "+
				"routable_node_count 会从 4193 漂亮地降到 2931，而 196 个存量租约一个都没迁移。",
			concept.word, concept.why)
	}
}

// TestResinRuntimeConfigDoesNotShipDetailLogging 是上面两条的相邻保护。
//
// reverse_proxy_log_detail_enabled 会把请求/响应的头与体写进日志。它是排障时
// 临时打开的东西，一旦被写进部署真源就等于常开 —— 而经过这条链路的正是
// 带凭据的上游请求。
func TestResinRuntimeConfigDoesNotShipDetailLogging(t *testing.T) {
	cfg := loadResinRuntimeConfig(t)
	require.Equal(t, false, cfg.ReverseProxyLogDetailOn,
		"部署真源里不得常开详细日志：经过这条链路的是带凭据的上游请求")
}

// TestResinRuntimeConfigHasNoSecrets 防止有人把 token 顺手写进这份会进仓库的文件。
//
// 判据只认**值**，不认名字。第一版按 "resin_admin_token=" / "authorization:" 这种
// 名字判，当场被这份文件自己的说明文字命中（里面写了 `Bearer $T` 的用法示例）——
// 那不是凭据，是文档。按名字判会让作者为了让门变绿而删掉文档，这是把门用反了。
// 真正危险的是**一个足够长的、看起来像真值的串**跟在凭据名后面。
func TestResinRuntimeConfigHasNoSecrets(t *testing.T) {
	raw, err := os.ReadFile(resinRuntimeConfigPath)
	require.NoError(t, err)

	hits := resinScanForCredentialValues(string(raw))
	require.Len(t, hits, 0,
		"部署真源会进仓库，不得含任何凭据值；token 的落点是 /etc/resin.env。命中：%v", hits)
}

// resinCredentialValueRes 是「凭据名 + 一个像真值的串」的形态。
// 占位符（$VAR / ${VAR} / <...> / xxx / ***）刻意排除在外：它们出现在文档里是正常的。
var resinCredentialValueRes = []*regexp.Regexp{
	// RESIN_ADMIN_TOKEN=<16+ 位非占位符>
	regexp.MustCompile(`(?i)(resin_[a-z_]*token|admin_token|proxy_token)\s*[=:]\s*"?([A-Za-z0-9+/_-]{16,})"?`),
	// Bearer <16+ 位非占位符>
	regexp.MustCompile(`(?i)bearer\s+([A-Za-z0-9+/._-]{16,})`),
	// 裸的 64 位十六进制（jwt secret / device seed 的形态）
	regexp.MustCompile(`\b[0-9a-f]{64}\b`),
	// sk- 开头的 API key
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}\b`),
}

// resinScanForCredentialValues 返回命中的片段（已截断，不回显完整值）。
func resinScanForCredentialValues(text string) []string {
	var hits []string
	for _, re := range resinCredentialValueRes {
		for _, m := range re.FindAllString(text, -1) {
			// 占位符不算：$T / ${TOKEN} / <your-token> / *** / xxxx
			if strings.ContainsAny(m, "$<*") || strings.Contains(strings.ToLower(m), "xxxx") {
				continue
			}
			if len(m) > 24 {
				m = m[:24] + "…"
			}
			hits = append(hits, m)
		}
	}
	return hits
}

// TestResinSecretScannerActuallyCatchesSecrets 是上一条的**阳性标定**。
//
// 一个永远返回空的扫描器让上面那条门恒绿。这里喂进四种真实形态，逐一要求命中；
// 再喂进这份文件里真实存在的占位符文本，要求**不**命中（差分阴性）——
// 没有阴性那一半，把判据放宽成「只要出现 bearer 就算」也能让阳性全过，
// 代价是作者为了转绿去删文档。
func TestResinSecretScannerActuallyCatchesSecrets(t *testing.T) {
	positives := map[string]string{
		"环境文件形态":  `RESIN_ADMIN_TOKEN=Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5`,
		"请求头形态":   `Authorization: Bearer Zm9vYmFyYmF6cXV4MTIzNDU2`,
		"64位十六进制": `"secret": "5f4c48ffc3e93347631c03bc21ec41c1890595529a98f99ac2989ef0275d8d2a"`,
		"API key": `key=sk-0000000000000000000000000000000000000000000000000000000000000000`,
	}
	for name, sample := range positives {
		require.NotEmpty(t, resinScanForCredentialValues(sample),
			"%s 没被扫到 —— 扫描器漏了这一类，上面那条门对它恒绿", name)
	}

	negatives := map[string]string{
		"shell 占位符":  `curl -H "Authorization: Bearer $T" http://127.0.0.1:2260/`,
		"花括号占位符":     `RESIN_ADMIN_TOKEN=${RESIN_ADMIN_TOKEN}`,
		"尖括号占位符":     `Authorization: Bearer <your-admin-token-here>`,
		"文档里只提到名字":   `token 的落点是 /etc/resin.env 里的 RESIN_ADMIN_TOKEN`,
		"短值（不是凭据形态）": `Authorization: Bearer abc`,
	}
	for name, sample := range negatives {
		require.Len(t, resinScanForCredentialValues(sample), 0,
			"%s 被误判成凭据 —— 门会逼着作者删文档来转绿，这是把门用反了", name)
	}
}
