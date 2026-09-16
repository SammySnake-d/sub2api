package service

// 部署/出口（DP 组）里两条能在进程内机器判的义务。
//
// 这一组的大部分是 human_only（出口 IP 的国别、节点熔断后的迁移），因为判据在
// 机器之外。但其中两条的**决定点在 sub2api 自己的代码里**，可以不依赖线上环境：
//
//   DP:proxy-auth-header —— 代理认证写进哪个头。ma-relay 踩过并已修：SetBasicAuth
//     设的是 Authorization，而代理认证要 Proxy-Authorization，CONNECT 直接 407，
//     整个代理集成被挡死。这不是配置问题，是一行代码的问题，所以门开在代码上。
//
//   DP:sticky-per-account —— 一个账号一个出口身份。运营口径是「同一账号在连续
//     10 次请求内始终是同一个出口 IP」。出口 IP 由 resin 分配，但**账号身份**是
//     sub2api 给的：账号绑定的 proxy URL 形如
//     http://<Platform>.<Account>:<PROXY_TOKEN>@127.0.0.1:2260，
//     resin 从 proxy-auth 用户名读出是谁，再按它保持租约。身份不稳或两个账号撞成
//     同一个，出口 IP 立刻跟着串 —— 这一半完全在进程内，可以机器判。
//
// 两条都打在真实生产路径上：
//
//	(*Proxy).URL()                          internal/service/proxy.go:42
//	  ← account.ProxyID != nil && account.Proxy != nil 时取用
//	    internal/service/gateway_anthropic_passthrough.go:71-73（mirasim 数据面）
//	proxyurl.Parse                          internal/pkg/proxyurl/parse.go:36
//	  ← internal/repository/http_upstream.go:1240 normalizeProxyURL
//	proxyutil.ConfigureTransportProxy       internal/pkg/proxyutil/dialer.go:56
//	  ← internal/repository/http_upstream.go:1368 buildUpstreamTransport
//	tlsfingerprint.HTTPProxyDialer          internal/pkg/tlsfingerprint/dialer.go:184-221
//	  ← internal/repository/http_upstream.go:1438，mirasim 请求恒走这条
//	    （mirasim_upstream.go:113/126 强制 tlsfingerprint.MirasimProfile()）

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

const (
	// 生产形态：用户名 = <Platform>.<Account>，resin 据此保持 sticky 租约。
	mirasimProxyUser     = "Default.mirasim-77"
	mirasimProxyPassword = "proxy-token-fixture-value"
)

// mirasimProxyRecorder 记录到达"代理"的那一个请求的鉴权头。
// 只记头，不记值以外的东西；断言在测试函数里做，避免把判决藏进夹具。
type mirasimProxyRecorder struct {
	mu        sync.Mutex
	method    string
	proxyAuth string
	auth      string
	requests  int
}

func (r *mirasimProxyRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	r.method = req.Method
	r.proxyAuth = req.Header.Get("Proxy-Authorization")
	r.auth = req.Header.Get("Authorization")
}

func (r *mirasimProxyRecorder) snapshot() (method, proxyAuth, auth string, requests int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.method, r.proxyAuth, r.auth, r.requests
}

// mirasimFakeProxy 起一个像真代理一样行事的 HTTP 服务：
// 没有 Proxy-Authorization 就 407。407 不是测试造出来的效果，
// 它正是真代理对"认证写错头"的回答 —— 判据必须能重现那个后果，
// 否则本门只是在断言"我们写了某个字符串"。
func mirasimFakeProxy(t *testing.T, rec *mirasimProxyRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.Header.Get("Proxy-Authorization") == "" {
			w.Header().Set("Proxy-Authenticate", `Basic realm="resin"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mirasimProxyBasic(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// TestMirasimDeployProxyAuthUsesProxyAuthorizationHeader 钉住「代理认证写哪个头」。
//
// 三条腿，缺一条判据就漏掉一类失效：
//
//	A. 自研 CONNECT（mirasim 恒走这条）真的把凭据写进 Proxy-Authorization，
//	   且**没有**顺手也写进 Authorization（写进去等于把上游凭据位置泄给代理）。
//	B. 差分阴性：同一条代码路径，只摘掉 proxy URL 里的 userinfo 一个开关，
//	   结论必须从"通过隧道"反转成 407。
//	C. 走 net/http 的那条路径（buildUpstreamTransport）同样成立，且把凭据放进
//	   Authorization（ma-relay 踩过的那个 bug 的形状）真的会拿到 407。
//
// 只做 A 的话，判据退化成"某个字符串被设置过"：把 Proxy-Authorization 写成常量
// 空串也能让 A 通过（代理侧不检查），B/C 才让"写错头 = 407"这个因果留在门里。
func TestMirasimDeployProxyAuthUsesProxyAuthorizationHeader(t *testing.T) {
	// [[cov:DP:proxy-auth-header]]
	wantAuth := mirasimProxyBasic(mirasimProxyUser, mirasimProxyPassword)

	// ── A. 自研 CONNECT 隧道（tlsfingerprint.HTTPProxyDialer，mirasim 数据面）──
	recA := &mirasimProxyRecorder{}
	proxyA := mirasimFakeProxy(t, recA)
	proxyURLA, err := url.Parse(proxyA.URL)
	require.NoError(t, err, "httptest 代理地址解析失败，后面全是空转")
	proxyURLA.User = url.UserPassword(mirasimProxyUser, mirasimProxyPassword)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialer := tlsfingerprint.NewHTTPProxyDialer(tlsfingerprint.MirasimProfile(), proxyURLA)
	_, errWithCreds := dialer.DialTLSContext(ctx, "tcp", "relay.mirasim.test:443")

	methodA, proxyAuthA, authA, requestsA := recA.snapshot()
	require.Equal(t, 1, requestsA, "代理没收到请求 —— 断言的是一个从未发生的握手")
	require.Equal(t, http.MethodConnect, methodA,
		"HTTPS 经代理必须是 CONNECT 隧道；不是 CONNECT 就说明走了另一条路，本条判的不是生产路径")
	require.Equal(t, wantAuth, proxyAuthA,
		"代理认证必须写进 Proxy-Authorization（tlsfingerprint/dialer.go:221）；写错头 = 407 挡死整个代理集成")
	require.Equal(t, "", authA,
		"Authorization 必须留空：那一格属于上游账号凭据，代理拿不到也不该拿到")
	// 握手会失败（假代理不会真的建 TLS），但失败原因不能是 407 —— 那是"认证没过"。
	require.NotNil(t, errWithCreds, "假代理不可能完成 utls 握手，这里必须有错误，否则夹具没按预期跑")
	require.NotContains(t, errWithCreds.Error(), "407",
		"带着 Proxy-Authorization 还拿到 407，说明凭据没被代理层认到")

	// ── B. 差分阴性：只摘掉 userinfo，结论必须反转成 407 ────────────────
	recB := &mirasimProxyRecorder{}
	proxyB := mirasimFakeProxy(t, recB)
	proxyURLB, err := url.Parse(proxyB.URL)
	require.NoError(t, err)
	// 唯一改动：不带 userinfo。其余（profile、目标、超时）逐字相同。
	dialerB := tlsfingerprint.NewHTTPProxyDialer(tlsfingerprint.MirasimProfile(), proxyURLB)
	_, errNoCreds := dialerB.DialTLSContext(ctx, "tcp", "relay.mirasim.test:443")

	_, proxyAuthB, _, requestsB := recB.snapshot()
	require.Equal(t, 1, requestsB, "差分臂的代理没收到请求，这条阴性是空的")
	require.Equal(t, "", proxyAuthB, "没有 userinfo 就不该凭空造出 Proxy-Authorization")
	require.NotNil(t, errNoCreds, "没有代理凭据时隧道必须建不起来")
	require.Contains(t, errNoCreds.Error(), "407",
		"缺 Proxy-Authorization 必须以 407 告终；不反转说明这个夹具根本没在检查认证，A 臂的结论也就不可信")

	// ── C. net/http 路径（proxyurl.Parse + proxyutil.ConfigureTransportProxy）──
	recC := &mirasimProxyRecorder{}
	proxyC := mirasimFakeProxy(t, recC)
	hostPort := strings.TrimPrefix(proxyC.URL, "http://")
	accountProxy := &Proxy{
		ID:       77,
		Name:     "resin-egress",
		Protocol: "http",
		Host:     strings.Split(hostPort, ":")[0],
		Port:     mirasimPortOf(t, hostPort),
		Username: mirasimProxyUser,
		Password: mirasimProxyPassword,
	}
	trimmed, parsed, err := proxyurl.Parse(accountProxy.URL())
	require.NoError(t, err, "(*Proxy).URL() 产出的地址过不了生产的 proxyurl.Parse，这条链在生产里也是断的")
	require.Equal(t, accountProxy.URL(), trimmed,
		"proxyurl.Parse 改写了 (*Proxy).URL() 的输出；两者不一致时账号实际走的地址与配置不是一个东西")
	require.Equal(t, mirasimProxyUser, parsed.User.Username(),
		"账号身份必须活在 proxy-auth 用户名里 —— resin 就是从这里认出是哪个账号的")

	transport := &http.Transport{}
	require.NoError(t, proxyutil.ConfigureTransportProxy(transport, parsed),
		"生产的 Transport 代理配置函数拒绝了这个地址")
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, err := client.Get("http://relay.mirasim.test/v1/messages")
	require.NoError(t, err, "经代理的请求没发出去")
	defer func() { _ = resp.Body.Close() }()

	_, proxyAuthC, authC, requestsC := recC.snapshot()
	require.Equal(t, 1, requestsC, "代理没收到请求")
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"带 userinfo 的代理请求必须过认证；拿到 %d 说明凭据没进 Proxy-Authorization", resp.StatusCode)
	require.Equal(t, wantAuth, proxyAuthC,
		"net/http 路径同样必须把代理凭据放进 Proxy-Authorization")
	require.Equal(t, "", authC, "代理凭据不得同时出现在 Authorization")

	// ── D. 反例保护：把凭据放进 Authorization（ma-relay 踩过的写法）→ 407 ──
	recD := &mirasimProxyRecorder{}
	proxyD := mirasimFakeProxy(t, recD)
	_, parsedD, err := proxyurl.Parse(proxyD.URL) // 唯一改动：代理地址不带 userinfo
	require.NoError(t, err)
	transportD := &http.Transport{}
	require.NoError(t, proxyutil.ConfigureTransportProxy(transportD, parsedD))
	reqD, err := http.NewRequest(http.MethodGet, "http://relay.mirasim.test/v1/messages", nil)
	require.NoError(t, err)
	reqD.SetBasicAuth(mirasimProxyUser, mirasimProxyPassword) // SetBasicAuth 写的是 Authorization
	respD, err := (&http.Client{Transport: transportD, Timeout: 5 * time.Second}).Do(reqD)
	require.NoError(t, err)
	defer func() { _ = respD.Body.Close() }()

	_, proxyAuthD, authD, _ := recD.snapshot()
	require.Equal(t, http.StatusProxyAuthRequired, respD.StatusCode,
		"把代理凭据写进 Authorization 必须拿到 407 —— 这正是 ma-relay 踩过的那次故障形状")
	require.Equal(t, "", proxyAuthD, "Authorization 不会被代理当成代理认证")
	require.Equal(t, mirasimProxyBasic(mirasimProxyUser, mirasimProxyPassword), authD,
		"差分臂必须真的把凭据发出去了（发的是错的那一格），否则 407 只说明请求是空的")
}

func mirasimPortOf(t *testing.T, hostPort string) int {
	t.Helper()
	parts := strings.Split(hostPort, ":")
	require.Len(t, parts, 2, "httptest 地址 %q 不是 host:port 形态", hostPort)
	port := 0
	_, err := fmt.Sscanf(parts[1], "%d", &port)
	require.NoError(t, err, "端口解析失败")
	return port
}

// mirasimStickyAccount 造一个 mirasim 账号的出口形态：账号绑一个代理行，
// 代理行的用户名就是它在出口池里的身份（<Platform>.<Account>）。
func mirasimStickyAccount(n int64) *Account {
	proxyID := n
	return &Account{
		ID:       n,
		Name:     fmt.Sprintf("mirasim-%d", n),
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		ProxyID:  &proxyID,
		Proxy: &Proxy{
			ID:       n,
			Name:     fmt.Sprintf("resin-%d", n),
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     2260,
			Username: fmt.Sprintf("Default.mirasim-%d", n),
			Password: mirasimProxyPassword,
		},
	}
}

// mirasimResolveStickyIdentity 逐字镜像生产的解析链：
//
//	gateway_anthropic_passthrough.go:71-73   if account.ProxyID != nil && account.Proxy != nil { account.Proxy.URL() }
//	http_upstream.go:1240 normalizeProxyURL  proxyurl.Parse(raw)
//
// 返回出口池看到的账号身份（proxy-auth 用户名）。
func mirasimResolveStickyIdentity(t *testing.T, account *Account) string {
	t.Helper()
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	_, parsed, err := proxyurl.Parse(proxyURL)
	require.NoError(t, err, "账号 %d 的代理地址过不了生产的 proxyurl.Parse", account.ID)
	if parsed == nil || parsed.User == nil {
		return ""
	}
	return parsed.User.Username()
}

// TestMirasimStickyProxyIdentityIsStablePerAccount 钉住「一号一身份」。
//
// 运营判据是出口 IP：同一账号连续 10 次请求出口 IP 不变，不同账号不撞在一个 IP 上
// （每 IP ≤ 3 个号）。IP 由 resin 分配，但它凭什么知道"这是同一个账号"——
// 凭 proxy-auth 用户名。所以在 sub2api 这一侧，这条义务的可判部分是：
//
//	同一账号连续解析出的身份完全相同；不同账号的身份互不相同。
//
// 判据若放宽成「解析不报错」，漏掉的正是最危险的两种：身份为空（整池共用出口池
// 默认身份 = 所有账号从同一个 IP 出去）与身份相撞（两个号被 resin 当成一个）。
// 这两种都不会让任何功能测试变红：请求照样通，只是上游看到的设备画像全错。
func TestMirasimStickyProxyIdentityIsStablePerAccount(t *testing.T) {
	// [[cov:DP:sticky-per-account]]
	accountA := mirasimStickyAccount(7)
	accountB := mirasimStickyAccount(8)

	// ── 正例 1：同一账号连续 10 次（对齐 N2 的「连续 10 次请求」口径）────
	identities := make([]string, 0, 10)
	unique := make(map[string]struct{})
	for i := 0; i < 10; i++ {
		id := mirasimResolveStickyIdentity(t, accountA)
		identities = append(identities, id)
		unique[id] = struct{}{}
	}
	require.Len(t, identities, 10, "循环没跑满 10 次，这条正例没覆盖运营口径")
	require.Len(t, unique, 1,
		"同一账号连续 10 次解析出了 %d 个不同身份：%v；身份一变，resin 的租约就换节点，出口 IP 跟着换",
		len(unique), identities)
	require.Equal(t, "Default.mirasim-7", identities[0],
		"身份必须是 <Platform>.<Account> 形态，resin 按它保持 sticky 租约")
	require.Equal(t, identities[0], identities[9], "第 1 次与第 10 次必须逐字相同")

	// 身份确实由 (*Proxy).URL() 承载，而不是测试自己算出来的。
	require.Contains(t, accountA.Proxy.URL(), "Default.mirasim-7",
		"(*Proxy).URL()（internal/service/proxy.go:42）必须把用户名写进 userinfo，否则出口池收不到身份")

	// ── 正例 2：不同账号互不相同，且只差在身份上 ──────────────────────
	identityB := mirasimResolveStickyIdentity(t, accountB)
	require.Equal(t, "Default.mirasim-8", identityB, "账号 8 的身份形态不对")
	require.NotEqual(t, identities[0], identityB,
		"两个账号解析出同一个身份：resin 会把它们当成一个租约，出口 IP 必然共用，「一号一 IP」当场失效")

	hostA, hostB := mirasimProxyHostOf(t, accountA), mirasimProxyHostOf(t, accountB)
	require.Equal(t, hostA, hostB,
		"两个账号打的是同一个 resin 入口（同 host:port）—— 身份才是唯一的区分维度")
	require.NotEqual(t, accountA.Proxy.URL(), accountB.Proxy.URL(),
		"同入口、同口令、不同账号，代理地址必须不同；相同就说明身份没进地址")

	// ── 差分阴性 1：两个账号共用同一行代理（唯一开关：Proxy 指向同一个对象）──
	shared := &Proxy{
		ID: 99, Name: "resin-shared", Protocol: "http", Host: "127.0.0.1", Port: 2260,
		Username: "Default.mirasim-shared", Password: mirasimProxyPassword,
	}
	sharedID := shared.ID
	sharedA := *accountA
	sharedA.Proxy, sharedA.ProxyID = shared, &sharedID
	sharedB := *accountB
	sharedB.Proxy, sharedB.ProxyID = shared, &sharedID
	require.Equal(t, mirasimResolveStickyIdentity(t, &sharedA), mirasimResolveStickyIdentity(t, &sharedB),
		"差分阴性：两个账号共用一行代理时身份必然相撞 —— 这正是上面那条 NotEqual 要挡的配置错误，"+
			"它必须在这里真的复现，否则 NotEqual 可能只是因为用了两个不同的 fixture 而恒真")

	// ── 差分阴性 2：账号没绑代理（唯一开关：ProxyID = nil）────────────
	// 生产的取值条件是 account.ProxyID != nil && account.Proxy != nil；
	// 任一为空就直连裸奔 —— 出口 IP 变成 VPS 自己的公网 IP（N1 的反例保护正是这条）。
	naked := *accountA
	naked.ProxyID = nil
	require.Equal(t, "", mirasimResolveStickyIdentity(t, &naked),
		"ProxyID 为空时不应解析出任何身份：那种情况下请求直连出去，功能测试全绿而出口是错的")

	// ── "有用户名无口令"是静默降级，必须在更早的地方被拒 ──────────────────
	// service.(*Proxy).URL() 只在 Username 与 Password **同时**非空时才写 userinfo，
	// 那是上游刻意的兼容选择（有一条 username_only_keeps_no_auth_for_compatibility
	// 钉着），本 fork 不动它。这里把**后果**钉住，让它不至于变成一个没人知道的行为：
	// 这样的代理行会整段丢掉身份 —— 地址仍然合法、请求仍然发得出去、零报错，
	// 只是该账号从出口池的默认身份出去，一号一出口 IP 的隔离当场失效。
	//
	// 因为它在这一层无法被察觉，真正的拦截点放在导入侧
	//（handler/admin.parseMirasimProxyURL 对这种代理行直接报错），
	// 由 TestParseMirasimProxyURLRejectsUsernameWithoutPassword 覆盖。
	passwordless := &Proxy{
		ID: 100, Name: "resin-nopass", Protocol: "http", Host: "127.0.0.1", Port: 2260,
		Username: "Default.mirasim-7", Password: "",
	}
	passwordlessID := passwordless.ID
	degraded := *accountA
	degraded.Proxy, degraded.ProxyID = passwordless, &passwordlessID
	require.Equal(t, "", mirasimResolveStickyIdentity(t, &degraded),
		"口令为空时 userinfo 整段被丢：这就是为什么这种代理行必须在导入时就被拒掉")

	// 差分阴性：只有口令、没有用户名时同样解析不出身份（那不是身份，是半条凭据）。
	onlyPassword := &Proxy{
		ID: 101, Name: "resin-nouser", Protocol: "http", Host: "127.0.0.1", Port: 2260,
		Username: "", Password: "some-token",
	}
	onlyPasswordID := onlyPassword.ID
	halfCred := *accountA
	halfCred.Proxy, halfCred.ProxyID = onlyPassword, &onlyPasswordID
	require.Equal(t, "", mirasimResolveStickyIdentity(t, &halfCred),
		"只有口令没有用户名时解析不出身份：粘性身份就住在用户名里")
}

func mirasimProxyHostOf(t *testing.T, account *Account) string {
	t.Helper()
	_, parsed, err := proxyurl.Parse(account.Proxy.URL())
	require.NoError(t, err)
	return parsed.Host
}
