package repository

// body 层身份收敛（metadata.user_id）的回归锁。
//
// 被钉住的不变量：**同一个 mirasim 账号发出的每一个请求，上游在 body 里看到的
// metadata.user_id 都必须是同一个账号级稳定值，且与头层声称的那台设备自洽。**
//
// 缺口原样（修之前）：头层已经被 mirasim.ApplyCanonicalIdentityHeaders 收成"同一台
// 设备"，body 里的 metadata.user_id 却是客户端原值透传 —— 内含客户端真实 device ID 与
// session ID。这个组合比两层都不收口更可疑：一台设备报出多个 device id。
// 详见 mirasim_identity_body.go 顶部的三层对照表。
//
// 这里的测试都从**真的装饰器**入口发请求，观察的是真的出站字节，不是对纯函数的自证。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// mirasimUserIDRequest 造一个带 metadata.user_id 的数据面请求。
// clientHeaders 里放"这个客户是谁"的那些头，用来验证 body 层身份与客户无关。
func mirasimUserIDRequest(t *testing.T, bodyJSON string, clientHeaders map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://relay.example.invalid/v1/messages?beta=true", bytes.NewReader([]byte(bodyJSON)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "sk-placeholder")
	for k, v := range clientHeaders {
		req.Header.Set(k, v)
	}
	return req
}

// mirasimBodyWithUserID 把 userID 作为 JSON 字符串安全地放进 metadata.user_id，
// 避免手工转义把测试自己写错。
func mirasimBodyWithUserID(t *testing.T, userID string) string {
	t.Helper()
	b, err := sjson.SetBytes([]byte(`{"model":"claude-opus-5","max_tokens":16,"metadata":{"user_id":""},"messages":[{"role":"user","content":"hi"}]}`),
		"metadata.user_id", userID)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mirasimClientJSONUserID 是新格式（>= 2.1.78）的客户端原值。
func mirasimClientJSONUserID(deviceID, accountUUID, sessionID string) string {
	b, _ := json.Marshal(map[string]string{
		"device_id":    deviceID,
		"account_uuid": accountUUID,
		"session_id":   sessionID,
	})
	return string(b)
}

// sentUserID 取最近一次出站请求 body 里的 metadata.user_id。
// 数据面请求一定是最后一个：device-session 铸造发生在 Prepare 里，早于本次数据请求。
func sentUserID(t *testing.T, cap *capturingUpstream) (string, []byte) {
	t.Helper()
	sent, body := cap.last()
	if sent.URL == nil || sent.URL.Path == "/v1/device/session" {
		t.Fatalf("仪器自检失败：最后一个出站请求是控制面调用（%s），不是数据面请求", sent.URL)
	}
	return gjson.GetBytes(body, "metadata.user_id").String(), body
}

// addMirasimAccount 往夹具的持久层里塞第二个 mirasim 账号（自己的设备根）。
func addMirasimAccount(t *testing.T, store *fakeAccountStore, id int64) {
	t.Helper()
	seed, err := mirasim.NewDeviceSeed()
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.accounts[id] = &service.Account{
		ID: id, Platform: domain.PlatformAnthropic, Type: domain.AccountTypeAPIKey,
		Credentials: map[string]any{
			mirasim.CredProvider:    mirasim.ProviderMirasim,
			mirasim.CredDeviceSeed:  seed,
			mirasim.CredAccessToken: testAccessToken(time.Now().Add(40 * time.Minute)),
			mirasim.CredExpiresAt:   time.Now().Add(40 * time.Minute).Format(time.RFC3339),
			"base_url":              "https://relay.example.invalid",
		},
	}
}

// TestMirasimBodyUserIDIsIdenticalAcrossDifferentClients 是主断言。
//
// 两个身份完全不同的客户打进**同一个**号：不同的 user_id 格式（legacy vs JSON）、
// 不同的 device id、不同的 session、不同的 UA、不同的 x-claude-code-session-id。
// 上游在 body 里看到的必须是同一个值。
func TestMirasimBodyUserIDIsIdenticalAcrossDifferentClients(t *testing.T) {
	next, _, up := newMirasimFixture(t)

	clientADevice := strings.Repeat("a1", 32) // 64 hex
	clientAUserID := "user_" + clientADevice + "_account_11111111-1111-4111-8111-111111111111_session_22222222-2222-4222-8222-222222222222"
	clientBDevice := strings.Repeat("b2", 32)
	clientBUserID := mirasimClientJSONUserID(clientBDevice, "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444")

	if clientAUserID == clientBUserID {
		t.Fatal("前提自检：两个客户的 user_id 相同，「跨客户端」没被测到")
	}

	mirasimSend(t, up, 1, mirasimUserIDRequest(t, mirasimBodyWithUserID(t, clientAUserID), map[string]string{
		"user-agent":               "claude-cli/2.1.100 (external, cli)",
		"x-claude-code-session-id": "22222222-2222-4222-8222-222222222222",
	}))
	gotA, bodyA := sentUserID(t, next)

	mirasimSend(t, up, 1, mirasimUserIDRequest(t, mirasimBodyWithUserID(t, clientBUserID), map[string]string{
		"user-agent":               "claude-cli/2.9.0 (external, sdk-cli)",
		"x-claude-code-session-id": "44444444-4444-4444-8444-444444444444",
	}))
	gotB, bodyB := sentUserID(t, next)

	if gotA == "" || gotB == "" {
		t.Fatal("出站 body 里根本没有 metadata.user_id —— 收敛把字段弄丢了")
	}
	if gotA != gotB {
		t.Fatalf("同一个账号、两个客户，上游看到两个 user_id：\n  A = %s\n  B = %s\n"+
			"—— 一台设备报出多个 device id，反关联当场失效", gotA, gotB)
	}
	if gotA == clientAUserID || gotB == clientBUserID {
		t.Fatal("客户端原值被原样透传了：收敛没生效")
	}

	// 逐字节找客户端真 device id：它绝不能以任何形式出现在出站 body 里。
	for name, leak := range map[string]string{"A": clientADevice, "B": clientBDevice} {
		for who, body := range map[string][]byte{"A": bodyA, "B": bodyB} {
			if bytes.Contains(body, []byte(leak)) {
				t.Fatalf("客户 %s 的真实 device id 出现在客户 %s 的出站 body 里", name, who)
			}
		}
	}
}

// TestMirasimBodyUserIDDiffersAcrossMirasimAccounts 是反向失效保护。
//
// 没有它，"把所有 mirasim 请求的 user_id 写成同一个常量"也能让上面那条全绿 ——
// 而那会把号池里 143 个账号全部关联成同一台设备，比原缺陷更糟。
func TestMirasimBodyUserIDDiffersAcrossMirasimAccounts(t *testing.T) {
	next, store, up := newMirasimFixture(t)
	addMirasimAccount(t, store, 3)

	clientUserID := mirasimClientJSONUserID(strings.Repeat("c3", 32),
		"55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666")
	body := mirasimBodyWithUserID(t, clientUserID)
	headers := map[string]string{"user-agent": "claude-cli/2.1.272 (external, sdk-cli)"}

	mirasimSend(t, up, 1, mirasimUserIDRequest(t, body, headers))
	got1, _ := sentUserID(t, next)

	mirasimSend(t, up, 3, mirasimUserIDRequest(t, body, headers))
	got3, _ := sentUserID(t, next)

	if got1 == "" || got3 == "" {
		t.Fatal("出站 body 里根本没有 metadata.user_id")
	}
	if got1 == got3 {
		t.Fatalf("两个不同的 mirasim 账号对上游报出同一个 user_id（%s）："+
			"这会把整个号池关联成一台设备", got1)
	}
	// 两个账号的差异必须来自**设备根**，而不是只有 session 段不同。
	p1, p3 := service.ParseMetadataUserID(got1), service.ParseMetadataUserID(got3)
	if p1 == nil || p3 == nil {
		t.Fatalf("收敛后的 user_id 连自家解析器都过不了：%q / %q", got1, got3)
	}
	if p1.DeviceID == p3.DeviceID {
		t.Fatal("两个账号的 device 段相同：上游仍能把它们认成同一台设备")
	}
}

// TestMirasimBodyUserIDMatchesTheSignedDeviceAndCanonicalUA 钉头层与 body 层自洽。
//
// 能在测试里看到"签名用的是哪台设备"的唯一窗口是 device-session 铸造请求的明文
// body（{publicKey, deviceId}）—— x-mirasim-device 被封进 x-mirasim-enc，测试解不开。
// 见 mirasim_upstream_test.go 里 deviceMints 的说明。
func TestMirasimBodyUserIDMatchesTheSignedDeviceAndCanonicalUA(t *testing.T) {
	next, store, up := newMirasimFixture(t)

	mirasimSend(t, up, 1, mirasimUserIDRequest(t,
		mirasimBodyWithUserID(t, mirasimClientJSONUserID(strings.Repeat("d4", 32), "", "77777777-7777-4777-8777-777777777777")),
		map[string]string{"user-agent": "claude-cli/2.0.1 (external, cli)"}))

	got, _ := sentUserID(t, next)
	parsed := service.ParseMetadataUserID(got)
	if parsed == nil {
		t.Fatalf("收敛后的 user_id 解析不了：%q", got)
	}

	// 1) device 段必须由**这次真的签了名的那颗设备身份**派生，不是某个常量。
	mint := next.lastDeviceMint(t)
	if want := mirasimMetaDeviceID(mint.DeviceID); parsed.DeviceID != want {
		t.Fatalf("body 的 device 段 = %s，但签名用的设备是 %s（应派生出 %s）——"+
			"头和 body 说的不是同一台设备", parsed.DeviceID, mint.DeviceID, want)
	}

	// 2) session 段必须由账号级持久真源 accounts.extra.mirasim_session_id 派生
	//    （就是 x-mirasim-session 在客户端没带会话 id 时回落的那个值）。
	store.mu.Lock()
	persisted, _ := store.accounts[1].Extra[mirasim.ExtraSessionID].(string)
	store.mu.Unlock()
	if persisted == "" {
		t.Fatal("仪器自检失败：账号级 session id 没有落到 accounts.extra，本条无从比对")
	}
	if want := mirasimMetaSessionUUID(persisted); parsed.SessionID != want {
		t.Fatalf("body 的 session 段 = %s，want %s（由 extra.%s 派生）",
			parsed.SessionID, want, mirasim.ExtraSessionID)
	}

	// 3) body 声称的**格式**必须与头层声称的版本一致：客户端自报 2.0.1（legacy 格式），
	//    头层覆盖成 canonical 版本（>= 2.1.78，JSON 格式）。body 若跟着客户端走，
	//    上游就会看到"UA 说 2.1.272、body 却是 2.1.78 之前的老拼接串"这种不可能组合。
	sent, _ := next.last()
	sentUA := sent.Header.Get("User-Agent")
	if want := mirasim.CanonicalIdentityHeaders()["User-Agent"]; sentUA != want {
		t.Fatalf("前提自检：出站 UA = %q，want canonical %q", sentUA, want)
	}
	wantNewFormat := service.IsNewMetadataFormatVersion(service.ExtractCLIVersion(sentUA))
	if parsed.IsNewFormat != wantNewFormat {
		t.Fatalf("body 的 user_id 格式（new=%v）与出站 UA %q 声称的版本（new=%v）不自洽",
			parsed.IsNewFormat, sentUA, wantNewFormat)
	}
}

// TestMirasimBodyUserIDLeavesNonMirasimAccountsAlone 是差分阴性。
//
// 没有它，"无条件重写所有账号的 metadata.user_id"也能让上面几条全绿 —— 而那会把
// 每一个普通 anthropic / OpenAI / Gemini 账号的客户端身份也一起改掉。
func TestMirasimBodyUserIDLeavesNonMirasimAccountsAlone(t *testing.T) {
	next, store, up := newMirasimFixture(t)
	if _, ok := store.accounts[2]; !ok {
		t.Skip("夹具里没有非 mirasim 账号可用作对照")
	}

	clientUserID := mirasimClientJSONUserID(strings.Repeat("e5", 32),
		"88888888-8888-4888-8888-888888888888", "99999999-9999-4999-8999-999999999999")
	body := mirasimBodyWithUserID(t, clientUserID)

	mirasimSend(t, up, 2, mirasimUserIDRequest(t, body, map[string]string{"user-agent": "claude-cli/2.1.100 (external, cli)"}))

	sent, sentBody := next.last()
	if !bytes.Equal(sentBody, []byte(body)) {
		t.Fatalf("非 mirasim 账号的 body 被改了：\n got %s\nwant %s", sentBody, body)
	}
	if got := gjson.GetBytes(sentBody, "metadata.user_id").String(); got != clientUserID {
		t.Fatalf("非 mirasim 账号的 metadata.user_id 被改成了 %q —— 收敛只能作用于 mirasim 这条 lane", got)
	}
	if sent.Header.Get("x-mirasim-enc") != "" {
		t.Fatal("前提自检：非 mirasim 账号被签名了，这条对照没有意义")
	}
}

// TestMirasimBodyRewritePreservesEveryOtherByte：除了 metadata.user_id 那一个 JSON
// 标记，body 的每一个字节都必须原样。
//
// 为什么必须逐字节：重新序列化会重排键并把 < > & HTML 转义，毁掉上游的 prompt-cache
// 前缀；thinking 块与 cache_control 对字节尤其敏感（identity_service.go 的注释警告过
// 同一件事，fingerprint.go 记着一个兄弟网关为此把 >90% 缓存命中打到 0）。
func TestMirasimBodyRewritePreservesEveryOtherByte(t *testing.T) {
	next, _, up := newMirasimFixture(t)

	// 键序非字母序、带 HTML 敏感字符、带 thinking 与 cache_control。
	// user_id 用 legacy 拼接串，这样手写 body 不需要任何转义。
	oldUserID := "user_" + strings.Repeat("f6", 32) + "_account_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa_session_bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	body := `{"model":"claude-opus-5","zeta":1,"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"metadata":{"user_id":"` + oldUserID + `","zzz":{"keep":"a < b && c > d"}},` +
		`"alpha":2,"system":[{"type":"text","text":"a < b && c > d","cache_control":{"type":"ephemeral"}}]}`

	// 老值在源字节里的位置：拿它把 body 切成 前缀 | user_id 标记 | 后缀。
	src := gjson.GetBytes([]byte(body), "metadata.user_id")
	if src.Index <= 0 {
		t.Fatal("仪器自检失败：gjson 没给出 metadata.user_id 的字节偏移，逐字节比对无从做起")
	}
	pre := []byte(body)[:src.Index]
	post := []byte(body)[src.Index+len(src.Raw):]

	mirasimSend(t, up, 1, mirasimUserIDRequest(t, body, map[string]string{"user-agent": "claude-cli/2.1.272 (external, sdk-cli)"}))
	got, sentBody := sentUserID(t, next)

	if got == oldUserID {
		t.Fatal("前提自检：user_id 没被改，本条测不到「改了之后其余字节没动」")
	}
	if !bytes.HasPrefix(sentBody, pre) {
		t.Fatalf("user_id 之前的字节变了：\n got %s\nwant 前缀 %s", sentBody, pre)
	}
	if !bytes.HasSuffix(sentBody, post) {
		t.Fatalf("user_id 之后的字节变了：\n got %s\nwant 后缀 %s", sentBody, post)
	}
	// 冗余但直指要害的两条：这两块是最容易被重新序列化毁掉的。
	for _, must := range []string{
		`"thinking":{"type":"enabled","budget_tokens":1024}`,
		`"cache_control":{"type":"ephemeral"}`,
		`a < b && c > d`,
	} {
		if !bytes.Contains(sentBody, []byte(must)) {
			t.Errorf("出站 body 里找不到原样的 %s —— body 被重新序列化过", must)
		}
	}
}

// TestMirasimBodyUserIDNotInjectedWhenClientDidNotSendOne：客户端没发就不该让它出现。
// 凭空注入一个 user_id 本身就是指纹。
func TestMirasimBodyUserIDNotInjectedWhenClientDidNotSendOne(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"没有 metadata", `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`},
		{"metadata 是空对象", `{"model":"claude-opus-5","metadata":{}}`},
		{"metadata 里只有别的字段", `{"model":"claude-opus-5","metadata":{"trace":"t-1"}}`},
		{"metadata 是 null", `{"model":"claude-opus-5","metadata":null}`},
		{"user_id 不是字符串", `{"model":"claude-opus-5","metadata":{"user_id":12345}}`},
		{"user_id 是空串", `{"model":"claude-opus-5","metadata":{"user_id":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, _, up := newMirasimFixture(t)
			mirasimSend(t, up, 1, mirasimUserIDRequest(t, tc.body, nil))
			_, sentBody := next.last()
			if !bytes.Equal(sentBody, []byte(tc.body)) {
				t.Fatalf("body 被动过：\n got %s\nwant %s", sentBody, tc.body)
			}
		})
	}
}

// TestMirasimRewrittenBodyIsWhatReachesTheWire 走**真的** httpUpstreamService
// transport 打到真 socket。
//
// 它守的是一个具体的失败模式：改写了 body 却忘了把字节装回 *http.Request
// （Body / ContentLength / GetBody 三处之一漏掉）。那时签名覆盖的是改写后的字节，
// 上线的却是改写前的，上游给 403，而任何只看进程内变量的测试都全绿。
func TestMirasimRewrittenBodyIsWhatReachesTheWire(t *testing.T) {
	echo := &mirasimEchoServer{}
	srv := httptest.NewServer(http.HandlerFunc(echo.handler))
	defer srv.Close()

	seed, err := mirasim.NewDeviceSeed()
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeAccountStore{accounts: map[int64]*service.Account{
		1: {
			ID: 1, Platform: domain.PlatformAnthropic, Type: domain.AccountTypeAPIKey,
			Credentials: map[string]any{
				mirasim.CredProvider:    mirasim.ProviderMirasim,
				mirasim.CredDeviceSeed:  seed,
				mirasim.CredAccessToken: testAccessToken(time.Now().Add(40 * time.Minute)),
				mirasim.CredExpiresAt:   time.Now().Add(40 * time.Minute).Format(time.RFC3339),
				"base_url":              srv.URL,
			},
		},
	}}
	up := NewMirasimUpstream(NewHTTPUpstream(nil), store)

	clientUserID := "user_" + strings.Repeat("07", 32) + "_account_cccccccc-cccc-4ccc-8ccc-cccccccccccc_session_dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	body := `{"model":"claude-opus-5","metadata":{"user_id":"` + clientUserID + `"},"messages":[{"role":"user","content":"a < b && c > d"}]}`

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/messages?beta=true", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")

	resp, err := up.DoWithTLS(req, "", 1, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if req.Header.Get("x-mirasim-enc") == "" {
		t.Fatal("前提自检：请求没有被签名/封装，本条测不到「签名覆盖的就是上线字节」")
	}

	echo.mu.Lock()
	defer echo.mu.Unlock()
	if echo.body == nil {
		t.Fatal("仪器自检失败：echo server 一个数据面请求都没收到")
	}
	wire := gjson.GetBytes(echo.body, "metadata.user_id").String()
	if wire == clientUserID {
		t.Fatal("到达 socket 的 body 里仍是客户端原值：改写没有装回 *http.Request")
	}
	if wire == "" {
		t.Fatalf("到达 socket 的 body 里没有 metadata.user_id：%s", echo.body)
	}
	// 到达 socket 的必须正是签名时那一份：从 req 的最终 GetBody 再读一次做交叉核对。
	rc, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	final := make([]byte, len(echo.body))
	if _, err := rc.Read(final); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	if !bytes.Equal(final, echo.body) {
		t.Fatalf("GetBody 与上线字节不一致（transport 重试会送出另一份）：\n GetBody %s\n wire    %s", final, echo.body)
	}
	if int64(len(echo.body)) != req.ContentLength {
		t.Fatalf("ContentLength = %d，上线 body 实际 %d 字节", req.ContentLength, len(echo.body))
	}
	if !bytes.Contains(echo.body, []byte(`a < b && c > d`)) {
		t.Error("上线 body 里的 < > & 被转义了 —— body 被重新序列化过")
	}
}
