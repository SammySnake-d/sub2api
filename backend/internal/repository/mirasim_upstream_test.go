package repository

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type fakeAccountStore struct {
	mu           sync.Mutex
	accounts     map[int64]*service.Account
	updates      []map[string]any
	extraUpdates []map[string]any
	reads        int
}

func (f *fakeAccountStore) GetByID(_ context.Context, id int64) (*service.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Counted: this is the ONLY door to the durable row, so the counter is what
	// distinguishes "the decorator went back to the store" from "the decorator
	// answered out of its own process cache".
	f.reads++
	acc, ok := f.accounts[id]
	if !ok {
		return nil, context.Canceled
	}
	clone := *acc
	clone.Credentials = map[string]any{}
	for k, v := range acc.Credentials {
		clone.Credentials[k] = v
	}
	clone.Extra = map[string]any{}
	for k, v := range acc.Extra {
		clone.Extra[k] = v
	}
	return &clone, nil
}

func (f *fakeAccountStore) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	acc := f.accounts[id]
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	for k, v := range updates {
		acc.Extra[k] = v
	}
	f.extraUpdates = append(f.extraUpdates, updates)
	return nil
}

func (f *fakeAccountStore) UpdateCredentials(_ context.Context, id int64, creds map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[id].Credentials = creds
	f.updates = append(f.updates, creds)
	return nil
}

func (f *fakeAccountStore) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeAccountStore) credential(id int64, key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, _ := f.accounts[id].Credentials[key].(string)
	return s
}

// dropCredential removes a field from the durable row, which is what a lost /
// never-written column looks like to the decorator.
func (f *fakeAccountStore) dropCredential(id int64, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.accounts[id].Credentials, key)
}

type capturingUpstream struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	handler  func(*http.Request) (*http.Response, error)
}

func (c *capturingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	c.mu.Lock()
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	c.requests = append(c.requests, req.Clone(req.Context()))
	c.bodies = append(c.bodies, body)
	h := c.handler
	c.mu.Unlock()
	if h != nil {
		return h(req)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
}

func (c *capturingUpstream) DoWithTLS(req *http.Request, p string, id int64, cc int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return c.Do(req, p, id, cc)
}

func (c *capturingUpstream) last() (*http.Request, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests[len(c.requests)-1], c.bodies[len(c.bodies)-1]
}

func testAccessToken(exp time.Time) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims, _ := json.Marshal(map[string]any{"sub": "usr_decorator", "exp": exp.Unix()})
	return head + "." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func newMirasimFixture(t *testing.T) (*capturingUpstream, *fakeAccountStore, service.HTTPUpstream) {
	t.Helper()
	seed, err := mirasim.NewDeviceSeed()
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeAccountStore{accounts: map[int64]*service.Account{
		1: {
			ID:       1,
			Platform: domain.PlatformAnthropic,
			Type:     domain.AccountTypeAPIKey,
			Credentials: map[string]any{
				mirasim.CredProvider:    mirasim.ProviderMirasim,
				mirasim.CredDeviceSeed:  seed,
				mirasim.CredAccessToken: testAccessToken(time.Now().Add(40 * time.Minute)),
				mirasim.CredExpiresAt:   time.Now().Add(40 * time.Minute).Format(time.RFC3339),
				"base_url":              "https://relay.example.invalid",
			},
		},
		2: {
			ID:          2,
			Platform:    domain.PlatformAnthropic,
			Type:        domain.AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-plain"},
		},
	}}
	next := newMirasimCapturingUpstream()
	return next, store, NewMirasimUpstream(next, store)
}

// newMirasimCapturingUpstream is one "process's" view of the network: it records
// everything the decorator sends. The device-session mint goes through it too;
// answer it with a failure so the credential falls back to the access token (no
// network in unit tests) — and so a mint is re-attempted on every request, which
// is what makes the device identity observable (see deviceMints).
func newMirasimCapturingUpstream() *capturingUpstream {
	return &capturingUpstream{handler: func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(``)), Header: http.Header{}}, nil
	}}
}

func newSignedRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://relay.example.invalid/v1/messages?beta=true", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "sk-placeholder")
	req.Header.Set("x-claude-code-session-id", "session_from_client")
	return req
}

func TestMirasimDecoratorSignsMirasimAccount(t *testing.T) {
	next, _, up := newMirasimFixture(t)
	body := `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	resp, err := up.DoWithTLS(newSignedRequest(t, body), "", 1, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	sent, sentBody := next.last()

	if got := string(sentBody); got != body {
		t.Fatalf("body was mutated:\n got %s\nwant %s", got, body)
	}
	if sent.Header.Get("x-api-key") != "" {
		t.Error("x-api-key must be removed on the mirasim path")
	}
	if !strings.HasPrefix(sent.Header.Get("Authorization"), "Bearer ") {
		t.Error("Authorization bearer was not set")
	}
	if sent.Header.Get(strings.ToLower("x-mirasim-enc")) == "" {
		t.Fatal("x-mirasim-enc was not set — the request went out unsealed")
	}
	if sent.Header.Get("x-mirasim-client") == "" {
		t.Error("x-mirasim-client must stay in the clear")
	}
	for _, h := range []string{"x-mirasim-device", "x-mirasim-ts", "x-mirasim-nonce", "x-mirasim-sig", "x-mirasim-session", "x-mirasim-agent"} {
		if sent.Header.Get(h) != "" {
			t.Errorf("%s leaked in plaintext", h)
		}
	}
	// The query must survive untouched even though it is not signed.
	if sent.URL.RawQuery != "beta=true" {
		t.Errorf("query = %q, want beta=true", sent.URL.RawQuery)
	}
}

func TestMirasimDecoratorLeavesOtherAccountsAlone(t *testing.T) {
	next, _, up := newMirasimFixture(t)
	req := newSignedRequest(t, `{"model":"claude-opus-5"}`)

	resp, err := up.Do(req, "", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	sent, _ := next.last()
	if sent.Header.Get("x-api-key") != "sk-placeholder" {
		t.Error("a non-mirasim account had its auth header rewritten")
	}
	if sent.Header.Get("x-mirasim-enc") != "" {
		t.Error("a non-mirasim account was signed")
	}
	if sent.Header.Get("x-claude-code-session-id") != "session_from_client" {
		t.Error("a non-mirasim account had headers stripped")
	}
}

func TestMirasimDecoratorUsesClientSessionID(t *testing.T) {
	_, store, up := newMirasimFixture(t)
	req := newSignedRequest(t, `{"model":"claude-opus-5"}`)
	resp, err := up.Do(req, "", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// A stable per-account session id must have been minted and persisted even
	// though this request carried its own.
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.extraUpdates) == 0 {
		t.Fatal("the stable session id was never persisted")
	}
	if _, ok := store.accounts[1].Extra[mirasim.ExtraSessionID].(string); !ok {
		t.Fatal("session id missing from account.extra")
	}
	if len(store.updates) != 0 {
		t.Fatal("minting a session id must not write the credentials column")
	}
}

func TestMirasimDecoratorRequiresDeviceSeed(t *testing.T) {
	_, store, up := newMirasimFixture(t)
	store.accounts[1].Credentials[mirasim.CredDeviceSeed] = ""

	_, err := up.Do(newSignedRequest(t, `{}`), "", 1, 4)
	if err == nil {
		t.Fatal("a mirasim account with no device seed must fail loudly, not silently send an unsigned request")
	}
	if !strings.Contains(err.Error(), "device seed") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// --- ACCEPTANCE A4: nothing may change between signing and the wire ---------

// mirasimEchoServer records exactly what arrived at the socket.
type mirasimEchoServer struct {
	mu       sync.Mutex
	hits     int
	header   http.Header
	body     []byte
	url      string
	redirect bool
}

func (e *mirasimEchoServer) handler(w http.ResponseWriter, r *http.Request) {
	// The device-session mint runs before the data request and would otherwise
	// consume the hit counter. Refuse it: the credential then falls back to the
	// access token, which is a supported path (see
	// TestPrepareFallsBackToAccessTokenWhenMintFails).
	if r.URL.Path == "/v1/device/session" {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	e.mu.Lock()
	e.hits++
	if e.redirect && e.hits == 1 {
		e.mu.Unlock()
		http.Redirect(w, r, "/v1/messages/elsewhere", http.StatusTemporaryRedirect)
		return
	}
	e.header = r.Header.Clone()
	e.body, _ = io.ReadAll(r.Body)
	e.url = r.URL.String()
	e.mu.Unlock()
	w.Header().Set("content-type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// TestMirasimSignedRequestReachesTheWireUnchanged is acceptance gate A4: the
// header set and body the decorator signed must be byte-identical to what the
// socket receives. It runs through the REAL httpUpstreamService transport (not a
// fake), so it also covers the transport's own header handling.
func TestMirasimSignedRequestReachesTheWireUnchanged(t *testing.T) {
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

	// A body with characters Go's encoding/json would HTML-escape and keys in an
	// order alphabetical marshalling would destroy — so a silent round-trip
	// anywhere downstream shows up as a diff.
	body := []byte(`{"model":"claude-opus-5","zeta":1,"alpha":2,"messages":[{"role":"user","content":"a < b && c > d"}]}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/messages?beta=true", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14,context-1m-2025-08-07")
	req.Header.Set("user-agent", "claude-cli/2.1.250 (external, sdk-cli)")

	resp, err := up.DoWithTLS(req, "", 1, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// [[cov:SG:no-post-sign-mutation]] The sealed header and the body captured at
	// the moment mirasim.SignAndSeal returned must be exactly what the socket
	// receives. This runs through the real httpUpstreamService transport, so it
	// covers the transport's own header handling too, and the body is compared
	// bytewise rather than by digest.
	signedEnc := req.Header.Get("x-mirasim-enc")
	signedAuth := req.Header.Get("Authorization")
	if signedEnc == "" {
		t.Fatal("request was not sealed")
	}

	echo.mu.Lock()
	defer echo.mu.Unlock()

	// ...must be exactly what the socket saw.
	if got := echo.header.Get("x-mirasim-enc"); got != signedEnc {
		t.Error("x-mirasim-enc changed between signing and the wire")
	}
	if got := echo.header.Get("Authorization"); got != signedAuth {
		t.Error("Authorization changed between signing and the wire")
	}
	if got := echo.header.Get("x-mirasim-client"); got != mirasim.ClientVersion {
		t.Errorf("x-mirasim-client on the wire = %q, want mirasim.ClientVersion %q", got, mirasim.ClientVersion)
	}
	if !bytes.Equal(echo.body, body) {
		t.Fatalf("body changed between signing and the wire:\n got %s\nwant %s", echo.body, body)
	}
	if got := echo.header.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14,context-1m-2025-08-07" {
		t.Errorf("anthropic-beta was rewritten to %q — this batch must not touch it", got)
	}
	// 身份头**是**这一层的职责，而且必须是覆盖式的。
	//
	// 这条断言原先反过来写（"client-identity normalisation is out of scope"），
	// 编码的是"传输层只签名、归一放在网关"那套设计。那套设计在生产上漏了：
	// 网关只覆盖客户流量，账号健康检查/计划探测/额度探测都不走网关，于是它们
	// 带着 service.defaultFingerprint 的陈旧画像（Linux / 0.94.0 / v24.3.0）出门，
	// 同一个账号在上游看来是两台设备（2026-09-16 线上 usage_logs 实录）。
	// 现在唯一落点收到了 mirasimUpstream.sign，所以调用方自报的 2.1.250 必须被覆盖。
	// 收口本身的正/反对照见 mirasim_identity_chokepoint_test.go。
	for k, want := range mirasim.CanonicalIdentityHeaders() {
		if got := echo.header.Get(k); got != want {
			t.Errorf("%s on the wire = %q, want canonical %q", k, got, want)
		}
	}
	if echo.url != "/v1/messages?beta=true" {
		t.Errorf("url on the wire = %q, want the signed path plus the unsigned query", echo.url)
	}
	// Whatever else travels, only x-mirasim-client and x-mirasim-enc may be in
	// the clear in that namespace.
	for k := range echo.header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-mirasim-") && lk != "x-mirasim-client" && lk != "x-mirasim-enc" {
			t.Errorf("%s reached the wire in plaintext", lk)
		}
	}
}

// TestMirasimRefusesToFollowRedirects: a redirect changes the path, and the path
// is inside the signature, so the hop could only ever arrive unsigned-for-it.
func TestMirasimRefusesToFollowRedirects(t *testing.T) {
	echo := &mirasimEchoServer{redirect: true}
	srv := httptest.NewServer(http.HandlerFunc(echo.handler))
	defer srv.Close()

	seed, _ := mirasim.NewDeviceSeed()
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

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader([]byte(`{}`)))
	resp, err := up.DoWithTLS(req, "", 1, 4, nil)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The mechanism, asserted directly: the decorator must have marked the
	// request so the shared upstream client refuses to follow.
	if !service.HTTPUpstreamRedirectsDisabled(req.Context()) {
		t.Fatal("service.HTTPUpstreamRedirectsDisabled is false: the decorator did not mark the request")
	}

	// [[cov:SG:no-redirect]] mirasim.SignAndSeal covers the URL path, so a
	// followed redirect would arrive carrying a signature for the previous path.
	// The decorator marks the request with
	// service.WithHTTPUpstreamRedirectsDisabled; the 307 must surface to the
	// caller instead of being followed.
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the 307 surfaced rather than followed", resp.StatusCode)
	}
	echo.mu.Lock()
	defer echo.mu.Unlock()
	if echo.hits != 1 {
		t.Fatalf("server saw %d requests: the redirect was followed with a signature for the old path", echo.hits)
	}
}

// TestMirasimRefusesProxyInURL: buildCustomRelayURL appends the proxy URL —
// credentials included — as a query parameter. That path is unreachable for an
// apikey account today; the guard is fail-closed insurance.
func TestMirasimRefusesProxyInURL(t *testing.T) {
	_, _, up := newMirasimFixture(t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://relay.example.invalid/v1/messages?beta=true&proxy=http%3A%2F%2Fuser%3Apass%40host%3A1080", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.DoWithTLS(req, "", 1, 4, nil); err == nil {
		t.Fatal("signing a URL carrying proxy credentials must be refused")
	} else if !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// --- IDENTITY: 设备身份的真源是 accounts 行，不是任何进程内缓存 --------------
//
// ID:deviceseed-stable / ID:deviceseed-db-backed 这两条必须打在**真的装饰器**上。
// 生产里的「缓存」是两个具体的东西：mirasimUpstream.bindings（TTL =
// mirasimBindingTTL）与 mirasim.Registry 的进程内 Credential；生产里的「持久层」
// 是 accounts 行，这里由 fakeAccountStore 扮演 —— 它是装饰器读取账号的唯一入口，
// 并且每次读都计数。用两个测试自己定义的 map 分别扮演「DB」和「缓存」证明不了
// 任何事：那只是测试刚写进去的东西读得出来。
//
// 测试怎么在装饰器外面看到「这次用的是哪颗 seed」：数据面请求的
// x-mirasim-device 被 SealHeaders 封进 x-mirasim-enc（x25519 封给 relay 的固定
// 公钥，测试解不开）。但 runtime.go authorizationLocked 手里没有可用 ticket 时会
// 发一个**明文**的 POST /v1/device/session，body 正是
// {"publicKey":<SPKI>,"deviceId":<id>}，且经 mirasimDoer 走同一个 next。本文件的
// fixture 让铸造一律 503（单元测试没有网络），于是每个请求都会重铸一次 ——
// 每个请求都在 next 上留下一份可读的设备身份快照。

type mirasimMintedDevice struct {
	PublicKey string `json:"publicKey"`
	DeviceID  string `json:"deviceId"`
}

// deviceMints 解出至今为止捕获到的每一次 device-session 铸造请求。
func (c *capturingUpstream) deviceMints(t *testing.T) []mirasimMintedDevice {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []mirasimMintedDevice
	for i, req := range c.requests {
		if req.URL == nil || req.URL.Path != "/v1/device/session" {
			continue
		}
		var m mirasimMintedDevice
		if err := json.Unmarshal(c.bodies[i], &m); err != nil {
			t.Fatalf("device-session mint body is not the plaintext {publicKey, deviceId} JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func (c *capturingUpstream) lastDeviceMint(t *testing.T) mirasimMintedDevice {
	t.Helper()
	mints := c.deviceMints(t)
	if len(mints) == 0 {
		t.Fatal("仪器自检失败：一次 device-session 铸造都没捕获到，本测试对设备身份是瞎的")
	}
	return mints[len(mints)-1]
}

// mirasimSend 发一个普通数据面请求，语义 = 「一个客户打进这个号」。
func mirasimSend(t *testing.T, up service.HTTPUpstream, accountID int64, req *http.Request) {
	t.Helper()
	resp, err := up.DoWithTLS(req, "", accountID, 4, nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
}

// mirasimStaleBinding 模拟 mirasimBindingTTL 到期（不 sleep 30 秒）：把生产结构体
// mirasimUpstream.bindings 里那一条的 expires 推到过去，同时把**缓存里那份**
// identity 的 seed 换成 cachedSeed。
//
// cachedSeed 是区分度的来源：过期之后，如果装饰器仍然拿缓存那份用，出站设备 id
// 就是 cachedSeed 派生的；只有真的回持久层读，设备 id 才会是账号行里那颗派生的。
func mirasimStaleBinding(t *testing.T, m *mirasimUpstream, accountID int64, cachedSeed string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bindings[accountID]
	if !ok {
		t.Fatalf("仪器自检失败：account=%d 根本不在进程内 binding 缓存里，"+
			"「缓存 vs 持久层」无从区分", accountID)
	}
	if !b.enabled {
		t.Fatalf("仪器自检失败：account=%d 的 binding 没有启用 mirasim 签名", accountID)
	}
	b.identity.DeviceSeed = cachedSeed
	b.expires = time.Now().Add(-mirasimBindingTTL)
	m.bindings[accountID] = b
}

func mirasimDecorator(t *testing.T, up service.HTTPUpstream) *mirasimUpstream {
	t.Helper()
	dec, ok := up.(*mirasimUpstream)
	if !ok {
		t.Fatalf("NewMirasimUpstream 没有返回 *mirasimUpstream（%T），本测试要检查的进程内缓存不在这里", up)
	}
	return dec
}

func TestMirasimDeviceSeedIsReadFromTheDurableStoreNotTheProcessCache(t *testing.T) {
	// [[cov:ID:deviceseed-db-backed]] 进程内缓存失效后 DeviceSeed 仍不变：真源是
	// accounts.credentials 里的 mirasim_device_seed，不是 mirasimUpstream.bindings。
	next, store, up := newMirasimFixture(t)
	dec := mirasimDecorator(t, up)

	seed := store.credential(1, mirasim.CredDeviceSeed)
	want, err := mirasim.NewDeviceSigner(seed)
	if err != nil {
		t.Fatal(err)
	}

	// 1) 冷启动的第一个请求：出站设备身份必须就是账号行里那颗 seed 派生的。
	mirasimSend(t, up, 1, newSignedRequest(t, `{"model":"claude-opus-5"}`))
	if got := next.lastDeviceMint(t); got.DeviceID != want.DeviceID || got.PublicKey != want.PublicKeyB64 {
		t.Fatalf("设备身份不是账号行那颗 seed 派生的：deviceId=%s want=%s (publicKey match=%v)",
			got.DeviceID, want.DeviceID, got.PublicKey == want.PublicKeyB64)
	}

	// 2) 仪器自检：进程内缓存确实存在，而且确实在挡住持久层读取。没有这一条，
	//    第 3 步的「过期后真的回读了一次」就可能只是「它每次都读」的平凡结论。
	mirasimSend(t, up, 1, newSignedRequest(t, `{"model":"claude-opus-5"}`))
	warm := store.readCount()
	mirasimSend(t, up, 1, newSignedRequest(t, `{"model":"claude-opus-5"}`))
	if got := store.readCount(); got != warm {
		t.Fatalf("仪器自检失败：binding 缓存没有挡住任何一次持久层读取（reads %d -> %d），"+
			"「来自 DB 而不是缓存」这条就没有区分度", warm, got)
	}

	// 3) 缓存过期，且过期那一刻缓存里放着另一颗 seed（诱饵）。
	decoySeed, err := mirasim.NewDeviceSeed()
	if err != nil {
		t.Fatal(err)
	}
	decoy, err := mirasim.NewDeviceSigner(decoySeed)
	if err != nil {
		t.Fatal(err)
	}
	if decoy.DeviceID == want.DeviceID {
		t.Fatal("前提自检：诱饵 seed 与账号行的 seed 派生出同一个设备 id，这一步分辨不了任何东西")
	}
	mirasimStaleBinding(t, dec, 1, decoySeed)

	before := store.readCount()
	mirasimSend(t, up, 1, newSignedRequest(t, `{"model":"claude-opus-5"}`))
	if got := store.readCount(); got != before+1 {
		t.Fatalf("binding 过期后没有回持久层读（reads %d -> %d）：这次签名用的是缓存里的身份", before, got)
	}
	switch got := next.lastDeviceMint(t); got.DeviceID {
	case want.DeviceID:
	case decoy.DeviceID:
		t.Fatalf("设备 id 来自进程内缓存里的那颗 seed（%s），不是 accounts.credentials.%s（%s）",
			decoy.DeviceID, mirasim.CredDeviceSeed, want.DeviceID)
	default:
		t.Fatalf("设备 id 既不是账号行也不是缓存里的：%s", got.DeviceID)
	}

	// 4) 反向对照：持久层的 seed 没了，而缓存里还留着**好的**那颗。必须立刻失败，
	//    不能悄悄拿缓存值顶上去 —— 后者意味着身份根本不是由持久层决定的。
	store.dropCredential(1, mirasim.CredDeviceSeed)
	mirasimStaleBinding(t, dec, 1, seed)
	if _, err := up.DoWithTLS(newSignedRequest(t, `{"model":"claude-opus-5"}`), "", 1, 4, nil); err == nil {
		t.Fatal("持久层的 device seed 没了却还签得出来：身份实际来自缓存层，不是 accounts 行")
	} else if !strings.Contains(err.Error(), "device seed") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestMirasimDeviceIDStableAcrossClientsAndProcessRestart(t *testing.T) {
	// [[cov:ID:deviceseed-stable]] 同一账号的设备身份跨不同客户端请求、跨进程重启不变。
	next, store, up := newMirasimFixture(t)

	seed := store.credential(1, mirasim.CredDeviceSeed)
	want, err := mirasim.NewDeviceSigner(seed)
	if err != nil {
		t.Fatal(err)
	}

	// 两个不同的客户打进同一个号：不同的会话 id、不同的 user-agent、不同的 body。
	// 差异在**装饰器入口**就存在，所以「设备身份与客户无关」是被真的问了一遍，
	// 而不是把同一个对象读两次。
	clientA := newSignedRequest(t, `{"model":"claude-opus-5","messages":[{"role":"user","content":"a"}]}`)
	clientA.Header.Set("x-claude-code-session-id", "session_from_client_a")
	clientA.Header.Set("user-agent", "claude-cli/2.1.100 (external, cli)")
	clientB := newSignedRequest(t, `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"bbbb"}]}`)
	clientB.Header.Set("x-claude-code-session-id", "session_from_client_b")
	clientB.Header.Set("user-agent", "claude-cli/2.9.0 (external, sdk-cli)")
	if clientA.Header.Get("x-claude-code-session-id") == clientB.Header.Get("x-claude-code-session-id") {
		t.Fatal("前提自检：两个客户的会话 id 相同，「跨客户端」没被测到")
	}

	mirasimSend(t, up, 1, clientA)
	mirasimSend(t, up, 1, clientB)

	mints := next.deviceMints(t)
	if len(mints) != 2 {
		t.Fatalf("两个客户请求应留下两次设备身份快照，实际 %d 次", len(mints))
	}
	for i, m := range mints {
		if m.DeviceID != want.DeviceID {
			t.Fatalf("客户 %d 看到的设备 id = %s，want %s：同一个号在上游看来换了一台设备", i+1, m.DeviceID, want.DeviceID)
		}
		if m.PublicKey != want.PublicKeyB64 {
			t.Fatalf("客户 %d 注册的 SPKI 公钥与 seed 派生值不一致：device-session 会绑到另一把钥匙上", i+1)
		}
	}

	// 重启：全新的 mirasimUpstream = 全新的 mirasim.Registry + 空的 binding 缓存。
	// 持久层是同一个 store，这正是迁移/重启后剩下的全部东西。
	restartedNext := newMirasimCapturingUpstream()
	restarted := NewMirasimUpstream(restartedNext, store)
	beforeRestart := store.readCount()
	mirasimSend(t, restarted, 1, newSignedRequest(t, `{"model":"claude-opus-5"}`))
	if store.readCount() == beforeRestart {
		t.Fatal("仪器自检失败：重启后的实例没有回持久层读过账号，它并不是一个冷进程")
	}

	if got := restartedNext.lastDeviceMint(t); got.DeviceID != want.DeviceID {
		t.Fatalf("重启后设备 id = %s，want %s：上游会看到这个号换了一台新设备", got.DeviceID, want.DeviceID)
	}
	if got := store.credential(1, mirasim.CredDeviceSeed); got != seed {
		t.Fatal("持久层里的 device seed 被某次普通请求重铸了（一次重铸就是一次换设备）")
	}
	store.mu.Lock()
	credWrites := len(store.updates)
	store.mu.Unlock()
	if credWrites != 0 {
		t.Fatalf("正常请求路径往 credentials 写了 %d 次：seed 所在的这一列不该被请求路径碰", credWrites)
	}
}
