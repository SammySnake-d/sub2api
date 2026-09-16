package repository

// 画像一致性的收口测试。
//
// 被钉住的不变量：**属于同一个 mirasim 账号的每一个出站请求，不管从哪条路径发起，
// 上游看到的都必须是同一台设备。**
//
// 这条在生产上真的破过（2026-09-16，线上 usage_logs 实录）：
//
//	客户流量      claude-cli/2.1.272 (external, sdk-cli)  MacOS  0.112.1  v26.3.0
//	账号健康检查   claude-cli/2.1.272 (external, cli)     Linux  0.94.0   v24.3.0
//
// 上游看到的是一台 Mac 和一台 Linux 交替在用同一个账号，SDK 版本还差了一大截。
// 原因是 canonical 身份头当时只挂在**两条网关路径**上，而健康检查、计划探测、
// 额度探测都不走网关，于是它们拿到了 service.defaultFingerprint 的陈旧值。
//
// 这个缺陷是**静默**的：健康检查照样 200，没有任何信号。所以它只能靠测试守。
// 现在唯一落点在 mirasimUpstream.sign，本文件从**不同入口**发请求来验证收口是否真的收住了。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// assertCanonicalIdentity 逐个头比对 canonical 值。
// 不用「包含某个子串」之类的宽松判据：跑偏的正是**某一个**头，
// 而宽松判据恰好会放过"只有 x-stainless-os 不对"这种形态。
func assertCanonicalIdentity(t *testing.T, h http.Header, where string) {
	t.Helper()
	for k, want := range mirasim.CanonicalIdentityHeaders() {
		if got := h.Get(k); got != want {
			t.Fatalf("%s: %s = %q，应为 %q —— 同一账号在上游露出了第二张脸", where, k, got, want)
		}
	}
}

// TestMirasimIdentityIsAppliedOnTheSignedPath 是基线：走签名的普通请求。
func TestMirasimIdentityIsAppliedOnTheSignedPath(t *testing.T) {
	cap, _, up := newMirasimFixture(t)

	req, err := http.NewRequest(http.MethodPost, "https://relay.mirasim.ai/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Do(req, "", 1, 1); err != nil {
		t.Fatal(err)
	}

	sent, _ := cap.last()
	assertCanonicalIdentity(t, sent.Header, "签名路径")
}

// TestMirasimIdentityOverridesACallerSuppliedProfile 是本文件的核心。
//
// 模拟账号健康检查的形态：调用方自己带了一整套**别的**设备画像
// （Linux / 0.94.0 / v24.3.0 / "(external, cli)"，即 service.defaultFingerprint 的值）。
// 收口必须把它们全部覆盖掉，而不是"缺了才补"。
func TestMirasimIdentityOverridesACallerSuppliedProfile(t *testing.T) {
	cap, _, up := newMirasimFixture(t)

	req, err := http.NewRequest(http.MethodPost, "https://relay.mirasim.ai/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	// 线上实录的那一套陈旧画像，逐字填进去。
	stale := map[string]string{
		"User-Agent":                  "claude-cli/2.1.272 (external, cli)",
		"X-Stainless-Package-Version": "0.94.0",
		"X-Stainless-Runtime-Version": "v24.3.0",
		"X-Stainless-OS":              "Linux",
		"X-Stainless-Arch":            "arm64",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Runtime":         "node",
	}
	for k, v := range stale {
		req.Header.Set(k, v)
	}

	if _, err := up.Do(req, "", 1, 1); err != nil {
		t.Fatal(err)
	}

	sent, _ := cap.last()
	assertCanonicalIdentity(t, sent.Header, "调用方自带陈旧画像")

	// 单独再钉一遍这两个：它们是线上真的跑偏了的那两个，
	// 上面的循环挂了也要让报错直接指向它们。
	if got := sent.Header.Get("X-Stainless-OS"); got == "Linux" {
		t.Fatal("x-stainless-os 仍是 Linux —— 覆盖没生效，账号在上游看起来换了台机器")
	}
	if got := sent.Header.Get("X-Stainless-Package-Version"); got == "0.94.0" {
		t.Fatal("x-stainless-package-version 仍是 0.94.0 —— SDK 版本与客户流量对不上")
	}
}

// TestMirasimIdentityAppliedOnUnsignedControlPlaneCalls 覆盖不签名的那条路。
//
// /auth/refresh 与 /auth/referral 走 MirasimSigningDisabled，不签名（签名会覆盖掉
// 它们自己的 bearer）。但它们**同样是这个账号发出去的请求**，身份不该在那里露出第二张脸，
// 所以身份头的应用刻意放在了 signing-disabled 检查之前。
func TestMirasimIdentityAppliedOnUnsignedControlPlaneCalls(t *testing.T) {
	cap, _, up := newMirasimFixture(t)

	req, err := http.NewRequest(http.MethodGet, "https://auth.example/auth/referral", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer caller-supplied-token")
	req.Header.Set("User-Agent", "some-other-client/1.0")
	req = req.WithContext(service.WithMirasimSigningDisabled(req.Context()))

	if _, err := up.Do(req, "", 1, 1); err != nil {
		t.Fatal(err)
	}

	sent, _ := cap.last()
	assertCanonicalIdentity(t, sent.Header, "不签名的控制面调用")

	// 差分保护：不签名这条路**必须**保留调用方自己的 bearer。
	// 如果为了统一身份而把它也签了，auth 服务器会拒掉一个它从没签发过的 device ticket。
	if got := sent.Header.Get("Authorization"); got != "Bearer caller-supplied-token" {
		t.Fatalf("控制面调用的 bearer 被改成了 %q —— 身份统一不该动鉴权", got)
	}
}

// TestMirasimIdentityLeavesNonMirasimAccountsAlone 是差分阴性。
//
// 没有它，把身份头改成"无条件覆盖所有账号"也能让上面三条全绿 ——
// 而那会把每一个普通 anthropic 账号的客户端画像也抹成 mirasim 的样子。
func TestMirasimIdentityLeavesNonMirasimAccountsAlone(t *testing.T) {
	cap, store, up := newMirasimFixture(t)

	// 2 号账号在夹具里不是 mirasim 账号。
	if _, ok := store.accounts[2]; !ok {
		t.Skip("夹具里没有非 mirasim 账号可用作对照")
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "claude-cli/9.9.9 (external, cli)")

	if _, err := up.Do(req, "", 2, 1); err != nil {
		t.Fatal(err)
	}

	sent, _ := cap.last()
	if got := sent.Header.Get("User-Agent"); got != "claude-cli/9.9.9 (external, cli)" {
		t.Fatalf("非 mirasim 账号的 UA 被改成了 %q —— 身份统一只能作用于 mirasim 这条 lane", got)
	}
}

// TestMirasimOutboundVersionIsIndependentOfTheClient 用**两个不同的客户端**做差分。
//
// 这条原本在 internal/service/mirasim_identity_test.go，打在
// GatewayService.buildUpstreamRequest 上。头层收口从网关搬到 mirasimUpstream.sign
// 之后，那两条（TestMirasimIdentityOutboundVersionIndependentOfClient /
// ...VersionSetIsCoherent）就成了指着旧位置的孤儿，实测失败值正是客户端自报的
// claude-cli/2.9.0 —— 网关确实不再写这些头了。搬过来是那次收口的收尾。
//
// **搬过来而不是删掉，是因为它的差分设计不该丢。** 上面那条
// TestMirasimIdentityOverridesACallerSuppliedProfile 用的是一套固定的陈旧画像，
// 而这条用两个**都不等于 canonical** 的版本各打一次：于是「出站 UA 两次相同」
// 就不可能是碰巧撞上其中之一，也不可能是"原样透传"——透传会让两次不同。
func TestMirasimOutboundVersionIsIndependentOfTheClient(t *testing.T) {
	cap, _, up := newMirasimFixture(t)

	canonical := mirasim.CanonicalIdentityHeaders()["User-Agent"]
	const clientOld = "claude-cli/2.1.100 (external, cli)"
	const clientNew = "claude-cli/2.9.0 (external, cli)"
	// 前提自检：两个客户端版本都必须与 canonical 不同，否则这条测试退化成恒真。
	if clientOld == canonical || clientNew == canonical {
		t.Fatalf("测试前提失效：客户端版本撞上了 canonical(%q)，"+
			"「出站等于 canonical」就可能只是碰巧透传了其中之一", canonical)
	}

	send := func(ua, pkgVer, runtimeVer string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "https://relay.mirasim.ai/v1/messages",
			strings.NewReader(`{"model":"claude-haiku-4-5"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("X-Stainless-Package-Version", pkgVer)
		req.Header.Set("X-Stainless-Runtime-Version", runtimeVer)
		if _, err := up.Do(req, "", 1, 1); err != nil {
			t.Fatal(err)
		}
		sent, _ := cap.last()
		return sent.Header.Get("User-Agent")
	}

	uaOld := send(clientOld, "0.91.1", "v22.14.0")
	uaNew := send(clientNew, "0.112.1", "v26.3.0")

	if uaOld != uaNew {
		t.Fatalf("同一个号（= 同一台设备）因为换了客户就改了口径。\n"+
			"旧客户出站 UA: %q\n新客户出站 UA: %q", uaOld, uaNew)
	}
	if uaOld != canonical {
		t.Fatalf("出站 UA 是 %q，不是 canonical 的 %q —— 两次相同但都不是 canonical，"+
			"说明被归一到了某个别的值", uaOld, canonical)
	}
	if uaOld == clientOld || uaOld == clientNew {
		t.Fatalf("出站 UA 等于某个客户端自报的值(%q) —— 那是透传，不是归一", uaOld)
	}
}

// TestMirasimOutboundVersionSetIsCoherent 是上一条的配套：光是"稳定"不够，
// 那一族头还必须**互相自洽**。
//
// 线上破过的正是自洽性而非稳定性：UA 报 claude-cli/2.1.272，而
// x-stainless-package-version 报 0.94.0、runtime 报 v24.3.0、os 报 Linux ——
// 每个头单独看都稳定，合起来却是一台不存在的机器。
func TestMirasimOutboundVersionSetIsCoherent(t *testing.T) {
	cap, _, up := newMirasimFixture(t)

	req, err := http.NewRequest(http.MethodPost, "https://relay.mirasim.ai/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	// 刻意喂进一整套自洽但**过时**的画像：单独看每个头都合理。
	for k, v := range map[string]string{
		"User-Agent":                  "claude-cli/2.1.272 (external, cli)",
		"X-Stainless-Package-Version": "0.94.0",
		"X-Stainless-Runtime-Version": "v24.3.0",
		"X-Stainless-OS":              "Linux",
	} {
		req.Header.Set(k, v)
	}
	if _, err := up.Do(req, "", 1, 1); err != nil {
		t.Fatal(err)
	}

	sent, _ := cap.last()
	// 整族逐个比 canonical。宽松判据（"只要 UA 对了就行"）恰好会放过
	// 「只有 x-stainless-os 没跟上」这种形态，而那正是线上真实发生过的那一种。
	assertCanonicalIdentity(t, sent.Header, "自洽但过时的整套画像")
}
