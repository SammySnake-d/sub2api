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
	require.Equal(t, 1000, cfg.MaxRoutableLatencyMs,
		"延迟天花板；超过即熔断，sticky 账号自动迁走")
	require.Equal(t, 1, cfg.MaxConsecutiveFailures,
		"一次失败即熔断。配合 resin 的单次请求内换节点重试，坏节点赔掉的那次 dial "+
			"由代理层自己吸收，不冒泡成 502 让上游网关误判成账号问题")
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
		"API key": `key=sk-0b6860ff99569c54b1d7e0b39a92201633991cf8a4e14bb71ebce962bf30f70f`,
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
