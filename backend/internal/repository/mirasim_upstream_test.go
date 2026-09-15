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
}

func (f *fakeAccountStore) GetByID(_ context.Context, id int64) (*service.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	next := &capturingUpstream{}
	// The device-session mint goes through next too; answer it with a failure so
	// the credential falls back to the access token (no network in unit tests).
	next.handler = func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(``)), Header: http.Header{}}, nil
	}
	return next, store, NewMirasimUpstream(next, store)
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

	// What the decorator left on the request object...
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
	if !bytes.Equal(echo.body, body) {
		t.Fatalf("body changed between signing and the wire:\n got %s\nwant %s", echo.body, body)
	}
	if got := echo.header.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14,context-1m-2025-08-07" {
		t.Errorf("anthropic-beta was rewritten to %q — this batch must not touch it", got)
	}
	if got := echo.header.Get("user-agent"); got != "claude-cli/2.1.250 (external, sdk-cli)" {
		t.Errorf("user-agent was rewritten to %q — client-identity normalisation is out of scope", got)
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
