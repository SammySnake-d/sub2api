//go:build unit

package service

// mirasim 通道的 Claude Code 身份改造 —— 回归锁。
//
// 守的不变量是一条**非对称**的：改造必须在该发生时发生，更必须在**不该发生时一个字节都不动**。
// 两个方向的代价不对称，所以差分阴性比阳性重要：
//
//	该改没改 → 上游拒收，请求直接失败，症状明显、一眼能发现
//	不该改却改了 → 真 Claude Code 客户端的 system 被重写，长 prompt 换成短 prompt，
//	              掉到 Anthropic 1024 token 最低缓存门槛以下 → 缓存整个失效 →
//	              每轮全量重建 → 首字延迟涨到分钟级 → 客户端取消 → 取消丢弃 in-flight
//	              缓存写入 → 重试从零开始。这是一条**静默**的、会自我放大的活锁，
//	              生产上表现为连续 ~59s 的取消，没有任何错误日志。
//
// 所以本文件里"原样返回"的断言一律用**字节比较**（require.Equal 比 []byte），
// 不用语义比较：一次 json.Unmarshal→Marshal 往返就会重排 key、把 < > & 转义成
// < 形态，语义比较看不出来，而那已经改了缓存前缀所在的字节。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 非 CC 第三方客户端的形态：有自己的 system，没有 billing attribution block。
const thirdPartyBody = `{"model":"claude-sonnet-4-5","max_tokens":1024,` +
	`"system":[{"type":"text","text":"You are a helpful coding assistant.","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":"hello world from a third party client"}]}`

func mimicryParse(t *testing.T, raw string) *ParsedRequest {
	t.Helper()
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(raw)), PlatformAnthropic)
	require.NoError(t, err)
	return parsed
}

func mirasimAccountForMimicry() *Account {
	return &Account{
		ID:       1,
		Type:     AccountTypeAPIKey,
		Platform: PlatformAnthropic,
		Credentials: map[string]any{
			"provider": "mirasim",
		},
	}
}

// 阳性：非 CC 的第三方请求被改造成 Claude Code 形态。
func TestMirasimIdentityInjectsForThirdPartyClient(t *testing.T) {
	svc := &GatewayService{}
	parsed := mimicryParse(t, thirdPartyBody)
	in := []byte(thirdPartyBody)

	out := svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, mirasimAccountForMimicry(), parsed, in)

	require.NotEqual(t, string(in), string(out), "非 CC 请求必须被改造，否则 mirasim 会拒收")
	system := gjson.GetBytes(out, "system")
	require.True(t, system.IsArray(), "system 必须是块数组，绝不能被拍平成字符串 —— 拍平会丢 cache_control")
	require.Greater(t, len(system.Array()), 1, "应注入多个 Claude Code 形态的 system block")
	require.Contains(t, string(out), claudeCodeSystemPrompt,
		"必须带 Claude Code 身份提示词，那是 mirasim 的收货判据")
}

// ★ 差分阴性 1：真 Claude Code 客户端的请求，一个字节都不许动。
//
// 判据走的是 body 里的 billing attribution block —— 真实 CC 每个请求都带它，
// 而经 new-api 之类上游网关转发进来的真 CC，UA 已被改写成 Go-http-client，
// 只剩这个 block 能证明它的身份。少了这条保护，被代理的真 CC 会被当成第三方重写。
func TestMirasimIdentityLeavesRealClaudeCodeByteIdentical(t *testing.T) {
	svc := &GatewayService{}
	// 先用改造结果当作"真 CC 请求"的样本：它带了完整的 billing block + 身份块。
	seed := mimicryParse(t, thirdPartyBody)
	ccLike := svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, mirasimAccountForMimicry(), seed, []byte(thirdPartyBody))
	require.True(t, systemHasBillingAttributionBlock(ccLike), "样本前提：它应当带 billing attribution block")

	parsed := mimicryParse(t, string(ccLike))
	parsed.MetadataUserID = "user_abc_account_def_session_ghi" // 被代理的真 CC 带 metadata.user_id

	out := svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, mirasimAccountForMimicry(), parsed, ccLike)

	require.Equal(t, ccLike, out,
		"真 Claude Code 请求必须字节级原样返回。一次 JSON 往返就会重排 key、"+
			"HTML 转义 < > &，而那些字节正在缓存前缀里")
}

// 差分阴性 2：非 mirasim 账号不受影响。
// 这条改造是为 mirasim 的收货规则做的，波及别的 anthropic 账号就是越界。
func TestMirasimIdentitySkipsNonMirasimAccount(t *testing.T) {
	svc := &GatewayService{}
	parsed := mimicryParse(t, thirdPartyBody)
	in := []byte(thirdPartyBody)

	for name, acc := range map[string]*Account{
		"oauth 账号": {ID: 2, Type: AccountTypeOAuth, Platform: PlatformAnthropic},
		"没有 provider 标记的 apikey": {
			ID: 3, Type: AccountTypeAPIKey, Platform: PlatformAnthropic,
			Credentials: map[string]any{"api_key": "sk-x"},
		},
		"provider 是别的值": {
			ID: 4, Type: AccountTypeAPIKey, Platform: PlatformAnthropic,
			Credentials: map[string]any{"provider": "not-mirasim"},
		},
	} {
		out := svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, acc, parsed, in)
		require.Equal(t, in, out, "%s 不该被改造", name)
	}
}

// 差分阴性 3:退化输入不许 panic，一律原样返回。
func TestMirasimIdentityDegenerateInputsAreUntouched(t *testing.T) {
	svc := &GatewayService{}
	acc := mirasimAccountForMimicry()
	parsed := mimicryParse(t, thirdPartyBody)

	require.Nil(t, svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, acc, parsed, nil))
	require.Equal(t, []byte{}, svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, acc, parsed, []byte{}))
	require.Nil(t, svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, acc, nil, nil))
	require.Nil(t, svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, nil, parsed, nil))
}

// ★ 确定性：同一输入调用两次，输出**字节完全相同**。
//
// 这是整套注入的缓存安全所系。Anthropic prompt cache 是 tools → system → messages 的
// 线性前缀匹配，从第一个不同的字节起后面全失效。注入内容里只要混进任何 per-request
// 变量（时间戳、随机 id、session id），前缀每次都不同，缓存就**永不命中** ——
// 而这不会报任何错，只会让首字延迟和成本一起涨上去。
func TestMirasimIdentityInjectionIsDeterministic(t *testing.T) {
	svc := &GatewayService{}
	acc := mirasimAccountForMimicry()
	in := []byte(thirdPartyBody)

	first := svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, acc, mimicryParse(t, thirdPartyBody), in)
	second := svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, acc, mimicryParse(t, thirdPartyBody), in)

	require.Equal(t, string(first), string(second),
		"注入必须是确定性的；任何 per-request 变量进了 system 都会让缓存永不命中")
}

// ★ cache_control 断点不得越过 Anthropic 上限。
//
// 注入**会**新增一个断点 —— 3-block 形态里的"扩充提示词"块自带一个，作为稳定的缓存
// 锚点，那是设计意图。危险的不是 +1，而是这条路径上原本**没有**上限守卫：
// OAuth 通道在 gateway_forward.go:258 统一跑 enforceCacheControlLimit，而直通路径在
// :113 就提前 return 了，走不到它。客户端本来就发满 4 个断点时，注入会让它变成 5 个、
// 被上游 400 拒绝。
//
// 这条测试第一次跑就是红的（1 → 2 且无人收口），据此才把守卫补进
// applyMirasimClaudeCodeIdentity。上限用常量而不是写死的 4：常量变了断言跟着变，
// 写死的数字会在别人调整上限时静默失效。
func TestMirasimIdentityKeepsCacheControlWithinLimit(t *testing.T) {
	svc := &GatewayService{}
	acc := mirasimAccountForMimicry()

	t.Run("客户端已发满上限", func(t *testing.T) {
		raw := bodyWithNCacheBreakpoints(maxCacheControlBlocks)
		before := countCacheControlForMimicryTest([]byte(raw))
		require.Equal(t, maxCacheControlBlocks, before, "前提：样本确实带了上限个断点")

		out := svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, acc, mimicryParse(t, raw), []byte(raw))

		require.LessOrEqual(t, countCacheControlForMimicryTest(out), maxCacheControlBlocks,
			"注入后断点数越过上限 %d，上游会 400 拒绝整个请求", maxCacheControlBlocks)
	})

	t.Run("普通请求", func(t *testing.T) {
		out := svc.applyMirasimClaudeCodeIdentity(context.Background(), nil, acc, mimicryParse(t, thirdPartyBody), []byte(thirdPartyBody))
		require.LessOrEqual(t, countCacheControlForMimicryTest(out), maxCacheControlBlocks)
	})
}

// bodyWithNCacheBreakpoints 造一个带 n 个 cache_control 断点的第三方请求。
//
// 断点必须**分散**在 tools / messages / system 三处，不能全塞进 system。
// 这不是排版偏好：rewriteSystemForNonClaudeCodeWithPromptBlocks 会把客户端 system 里的
// 多个块合并成**一个**文本块搬进 messages，所以全放 system 的断点会被塌缩成 1 个，
// 永远撞不到上限 —— 第一版 fixture 就是这么写的，变异验证(拿掉守卫)时测试照样绿，
// 空转了。tools 与 messages 上的断点不会被塌缩，那才是真正会越限的形态。
func bodyWithNCacheBreakpoints(n int) string {
	if n < 3 {
		n = 3
	}
	ephemeral := `,"cache_control":{"type":"ephemeral"}`
	// 1 个在 tools（Claude Code 真实发包里断点就常落在末位工具上）
	tools := `"tools":[{"name":"Read","description":"read a file",` +
		`"input_schema":{"type":"object","properties":{"p":{"type":"string"}}}` + ephemeral + `}],`
	// 1 个在 system
	system := `"system":[{"type":"text","text":"You are a helpful assistant."` + ephemeral + `}],`
	// 其余全部落在 messages —— 这些既不会被塌缩，也不会被搬位
	msgs := ""
	for i := 0; i < n-2; i++ {
		if i > 0 {
			msgs += ","
		}
		msgs += `{"role":"user","content":[{"type":"text","text":"turn ` +
			string(rune('A'+i)) + `"` + ephemeral + `}]}`
	}
	return `{"model":"claude-sonnet-4-5","max_tokens":1024,` + tools + system +
		`"messages":[` + msgs + `]}`
}

func countCacheControlForMimicryTest(body []byte) int {
	n := 0
	var walk func(gjson.Result)
	walk = func(r gjson.Result) {
		if r.IsObject() {
			if r.Get("cache_control").Exists() {
				n++
			}
			r.ForEach(func(_, v gjson.Result) bool { walk(v); return true })
			return
		}
		if r.IsArray() {
			r.ForEach(func(_, v gjson.Result) bool { walk(v); return true })
		}
	}
	walk(gjson.ParseBytes(body))
	return n
}
