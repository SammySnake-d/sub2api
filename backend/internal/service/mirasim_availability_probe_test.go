//go:build unit

package service

// mirasim 探活拦截的回归锁。
//
// 判据来自 2026-09-17 在法国生产实例上跑的三臂实测（见被测文件头），
// 这里把那三臂逐条钉住，外加两条阴性对照。

import (
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

// 错误文案必须与上游逐字一致：客户端看到的东西不能因为「我们提前拦了」而变形。
// ma-relay 的 bad_request_reason.go 就是按这条原文匹配来给 400 归类的。
func TestMirasimAvailabilityProbeMessageIsUpstreamVerbatim(t *testing.T) {
	const upstream = "this request asks for at most one token of output and carries no session, " +
		"so it is read as an availability probe rather than work. Use GET /v1/limits to check availability; " +
		"it costs no upstream call and is not rate limited per model"
	err := mirasimAvailabilityProbeBlock(probeTestMirasimAccount(),
		[]byte(`{"model":"x","max_tokens":1}`))
	require.NotNil(t, err)
	require.Equal(t, upstream, err.Error())
}
