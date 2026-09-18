//go:build unit

package service

// mirasim 探活拦截的回归锁。
//
// 判据来自 2026-09-17 在法国生产实例上跑的三臂实测（见被测文件头），
// 这里把那三臂逐条钉住，外加两条阴性对照。

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/stretchr/testify/require"
)

func probeTestMirasimAccount() *Account {
	return &Account{
		ID:          9101,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{mirasim.CredProvider: mirasim.ProviderMirasim},
	}
}

func probeTestPlainAccount() *Account {
	return &Account{
		ID:          9102,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{},
	}
}

// 三臂实测的逐条回归。第二臂是最重要的一条：它钉住「补 session 救不活」这个
// 反直觉的事实，防止下一个人读到上游文案里的 "carries no session" 之后，
// 把拦截改成「先补 metadata 再放行」。
func TestMirasimAvailabilityProbeMatchesMeasuredUpstreamRule(t *testing.T) {
	acct := probeTestMirasimAccount()

	t.Run("max_tokens=1 无 metadata —— 上游实测 400", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
		require.NotNil(t, mirasimAvailabilityProbeBlock(acct, body))
	})

	t.Run("max_tokens=1 带合法 metadata —— 上游实测仍然 400", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-5","max_tokens":1,` +
			`"messages":[{"role":"user","content":"hi"}],` +
			`"metadata":{"user_id":"user_` +
			`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa` +
			`_account_11111111-2222-3333-4444-555555555555` +
			`_session_66666666-7777-8888-9999-aaaaaaaaaaaa"}}`)
		require.NotNil(t, mirasimAvailabilityProbeBlock(acct, body),
			"带 session 也救不活（实测），所以拦截判据不能把 metadata 当成放行条件")
	})

	t.Run("max_tokens=2 —— 上游实测 200，必须放行", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-5","max_tokens":2,"messages":[{"role":"user","content":"hi"}]}`)
		require.Nil(t, mirasimAvailabilityProbeBlock(acct, body),
			"门槛是 ≤1；拦到 2 就是误伤本来能成功的请求")
	})
}

// 阴性对照一：非 mirasim 账号完全不受影响。
// 没有这条，把判据写成「只看 max_tokens」也能让上面三条全绿，
// 而那会让所有渠道的 max_tokens=1 请求都被拦掉。
func TestMirasimAvailabilityProbeLeavesOtherProvidersAlone(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	require.Nil(t, mirasimAvailabilityProbeBlock(probeTestPlainAccount(), body),
		"这条规则是 mirasim 上游特有的，不能外溢到别的渠道")
	require.Nil(t, mirasimAvailabilityProbeBlock(nil, body))
}

// 阴性对照二：判据必须保守。读不出 max_tokens 就放行 ——
// 宁可漏拦（代价是一次注定失败的往返，也就是今天的现状），
// 也不能误拦一条本来能成功的请求。
func TestMirasimAvailabilityProbeFailsOpenOnUnreadableBody(t *testing.T) {
	acct := probeTestMirasimAccount()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"空 body", ``},
		{"坏 JSON", `{"model":"x","max_tokens":`},
		{"没有 max_tokens 字段", `{"model":"claude-sonnet-5","messages":[]}`},
		{"max_tokens 是字符串", `{"model":"x","max_tokens":"1"}`},
		{"max_tokens 是 null", `{"model":"x","max_tokens":null}`},
		{"max_tokens 为负", `{"model":"x","max_tokens":-1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Nil(t, mirasimAvailabilityProbeBlock(acct, []byte(tc.body)),
				"读不准就必须放行，让上游自己判")
		})
	}
	// 0 是真的「不要输出」，同样会被上游判成探活，必须拦。
	require.NotNil(t, mirasimAvailabilityProbeBlock(acct, []byte(`{"model":"x","max_tokens":0}`)))
}

// 这条测试原来钉的是「文案必须与上游逐字一致」，理由是客户端看到的东西不该因为
// 我们提前拦了而变形。2026-09-18 那条不变量被推翻并替换：上游原文里带 "upstream"
// 和 "GET /v1/limits"，前者暴露我们是代理，后者是我们不对外暴露的端点，属于上游
// 指纹。泄露的代价高于形态一致的收益。
//
// 换成的新不变量：文案必须**指出是哪个字段、以及怎么改**——这正是本地拦截相对
// 直接透传的全部价值（上游从不指字段）。不泄露那一半由
// TestClientVisibleMessagesDoNotNameTheUpstream 统一扫，这里不重复。
func TestMirasimAvailabilityProbeMessageNamesTheFieldAndTheFix(t *testing.T) {
	err := mirasimAvailabilityProbeBlock(probeTestMirasimAccount(),
		[]byte(`{"model":"x","max_tokens":1}`))
	require.NotNil(t, err)
	msg := err.Error()
	require.Contains(t, msg, "max_tokens", "文案必须指出是哪个字段——上游从不指，这是本地拦的全部价值")
	require.Contains(t, msg, "at least 2", "文案必须给出可执行的修法，不能只说「被拒了」")
	require.NotContains(t, strings.ToLower(msg), "upstream",
		"客户端文案不许暴露我们是个代理")
	require.NotContains(t, msg, "/v1/limits",
		"不许引用我们不对外暴露的上游端点——那是上游指纹")
}

func TestMirasimSingleTokenCompatibilityIsExplicitAndScoped(t *testing.T) {
	svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MirasimSingleTokenCompatibility: true}}}
	for _, tc := range []struct {
		input   string
		want    int
		changed bool
	}{
		{`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"Write a Python division function"}]}`, 2, true},
		{`{"max_tokens":0}`, 0, false}, {`{"max_tokens":-1}`, -1, false}, {`{"max_tokens":1.5}`, 1, false}, {`{"max_tokens":256}`, 256, false}, {`{"max_tokens":"1"}`, 1, false},
	} {
		body := []byte(tc.input)
		out, err := svc.normalizeMirasimSingleToken(context.Background(), probeTestMirasimAccount(), body)
		require.NoError(t, err)
		if tc.changed {
			require.Equal(t, int64(tc.want), gjson.GetBytes(out, "max_tokens").Int())
			require.Equal(t, gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(out, "messages").Raw)
			require.Nil(t, mirasimAvailabilityProbeBlock(probeTestMirasimAccount(), out))
		} else {
			require.Equal(t, body, out)
		}
		plain, err := svc.normalizeMirasimSingleToken(context.Background(), probeTestPlainAccount(), body)
		require.NoError(t, err)
		require.Equal(t, body, plain)
	}
	svc.cfg.Gateway.MirasimSingleTokenCompatibility = false
	body := []byte(`{"max_tokens":1}`)
	out, err := svc.normalizeMirasimSingleToken(context.Background(), probeTestMirasimAccount(), body)
	require.NoError(t, err)
	require.Equal(t, body, out)
	require.Nil(t, mirasimAvailabilityProbeBlock(probeTestMirasimAccount(), []byte(`{"max_tokens":1.5}`)))
}
