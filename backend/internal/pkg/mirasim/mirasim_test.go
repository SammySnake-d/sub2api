package mirasim

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testJWT(t *testing.T, sub string, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims, err := json.Marshal(map[string]any{"sub": sub, "exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// TestPrepareRefreshesExpiringTokenOnce is the concurrency-safety gate on token
// refresh: N simultaneous requests on one account must produce exactly ONE
// refresh call and ONE device-session mint, and all of them must end up signing
// with the same credential.
func TestPrepareRefreshesExpiringTokenOnce(t *testing.T) {
	var refreshCalls, ticketCalls int64
	newToken := testJWT(t, "usr_refreshed", time.Now().Add(50*time.Minute))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			atomic.AddInt64(&refreshCalls, 1)
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["refresh_token"] != "rt_original" {
				t.Errorf("refresh sent %q", in["refresh_token"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  newToken,
				"refresh_token": "rt_rotated",
			})
		case "/v1/device/session":
			atomic.AddInt64(&ticketCalls, 1)
			if got := r.Header.Get("authorization"); got != "Bearer "+newToken {
				t.Errorf("device session used %q, want the refreshed token", redact(got))
			}
			// The mint is signed in plaintext: no seal, and the signature headers
			// must be present and unsealed.
			if r.Header.Get("x-mirasim-enc") != "" {
				t.Error("device session mint must not be sealed")
			}
			for _, h := range []string{"x-mirasim-device", "x-mirasim-ts", "x-mirasim-nonce", "x-mirasim-sig", "x-mirasim-client"} {
				if r.Header.Get(h) == "" {
					t.Errorf("device session mint missing %s", h)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "tkt_minted", "expiresIn": 600})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var persistMu sync.Mutex
	persisted := map[string]any{}
	persist := func(_ context.Context, _ int64, updates map[string]any) error {
		persistMu.Lock()
		defer persistMu.Unlock()
		for k, v := range updates {
			persisted[k] = v
		}
		return nil
	}

	ident := Identity{
		DeviceSeed:   testSeed,
		AccessToken:  testJWT(t, "usr_stale", time.Now().Add(1*time.Minute)), // inside refreshBefore
		RefreshToken: "rt_original",
		ExpiresAt:    time.Now().Add(1 * time.Minute),
		AuthBase:     srv.URL,
		RelayBase:    srv.URL,
	}

	reg := NewRegistry()
	client := doerFunc(func(r *http.Request) (*http.Response, error) { return srv.Client().Do(r) })

	const n = 8
	var wg sync.WaitGroup
	results := make([]Prepared, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = reg.Prepare(context.Background(), 42, ident, client, persist, persist)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Prepare[%d]: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&refreshCalls); got != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", got)
	}
	if got := atomic.LoadInt64(&ticketCalls); got != 1 {
		t.Errorf("device session mints = %d, want exactly 1", got)
	}
	for i, r := range results {
		if r.Credential != "tkt_minted" {
			t.Errorf("result[%d] credential = %q, want the minted ticket", i, redact(r.Credential))
		}
		if r.AccountSub != "usr_refreshed" {
			t.Errorf("result[%d] account sub = %q", i, r.AccountSub)
		}
		if r.SessionID == "" || !strings.HasPrefix(r.SessionID, "session_") {
			t.Errorf("result[%d] session id = %q", i, r.SessionID)
		}
	}

	persistMu.Lock()
	defer persistMu.Unlock()
	if persisted[CredAccessToken] != newToken {
		t.Error("rotated access token was not persisted")
	}
	if persisted[CredRefreshToken] != "rt_rotated" {
		t.Error("rotated refresh token was not persisted")
	}
	if _, ok := persisted[CredExpiresAt].(string); !ok {
		t.Error("expiry was not persisted as a string")
	}
	if persisted[ExtraSessionID] == nil {
		t.Error("session id was not persisted")
	}
}

// TestPrepareFallsBackToAccessTokenWhenMintFails pins ma-relay's behaviour: a
// failed device-session mint is not an error, the access token is itself a valid
// credential — and crucially the SAME string must be both signed and sent.
func TestPrepareFallsBackToAccessTokenWhenMintFails(t *testing.T) {
	token := testJWT(t, "usr_nomint", time.Now().Add(40*time.Minute))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	reg := NewRegistry()
	got, err := reg.Prepare(context.Background(), 7, Identity{
		DeviceSeed:  testSeed,
		AccessToken: token,
		ExpiresAt:   time.Now().Add(40 * time.Minute),
		RelayBase:   srv.URL,
		AuthBase:    srv.URL,
	}, doerFunc(func(r *http.Request) (*http.Response, error) { return srv.Client().Do(r) }), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Credential != token {
		t.Fatalf("credential = %q, want the access token", redact(got.Credential))
	}
}

func TestRefreshRejectionIsClassified(t *testing.T) {
	for _, tc := range []struct {
		status int
		hard   bool
	}{{400, true}, {401, true}, {403, true}, {429, false}, {500, false}, {503, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		reg := NewRegistry()
		_, err := reg.Prepare(context.Background(), 1, Identity{
			DeviceSeed:   testSeed,
			AccessToken:  testJWT(t, "u", time.Now().Add(time.Minute)),
			RefreshToken: "rt",
			ExpiresAt:    time.Now().Add(time.Minute),
			AuthBase:     srv.URL,
			RelayBase:    srv.URL,
		}, doerFunc(func(r *http.Request) (*http.Response, error) { return srv.Client().Do(r) }), nil, nil)
		if err == nil {
			t.Fatalf("status %d: expected an error", tc.status)
		}
		if IsHardAuthRejection(err) != tc.hard {
			t.Errorf("status %d: IsHardAuthRejection = %v, want %v", tc.status, !tc.hard, tc.hard)
		}
		srv.Close()
	}
}

func TestApplyContextHeadersThenSealLeavesOnlyClientAndEnc(t *testing.T) {
	signer, err := NewDeviceSigner(testSeed)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set("content-type", "application/json")
	h.Set("user-agent", "claude-cli/2.1.100 (external, cli)")
	h.Set("x-stainless-package-version", "0.94.0")
	h.Set("anthropic-beta", "interleaved-thinking-2025-05-14,context-1m-2025-08-07")
	h.Set("originator", "codex_cli_rs")

	before := h.Clone()

	ApplyContextHeaders(h, ContextInput{
		Path:       "/v1/messages",
		SessionID:  "session_abc",
		AccountSub: "usr_x",
		Locale:     "en-US",
	})

	// Client-identity normalisation is out of scope for this batch: every caller
	// header must survive byte-for-byte. Only the x-mirasim-* namespace is added.
	for k, want := range before {
		if got := h[k]; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("caller header %s was rewritten: %v -> %v", k, want, got)
		}
	}
	for k := range h {
		lk := strings.ToLower(k)
		if _, existed := before[http.CanonicalHeaderKey(k)]; !existed && !strings.HasPrefix(lk, "x-mirasim-") {
			t.Errorf("decorator added a non-x-mirasim header: %s", lk)
		}
	}

	if err := SignAndSeal(h, signer, "POST", "/v1/messages", []byte(`{}`), "cred"); err != nil {
		t.Fatal(err)
	}
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-mirasim-") && lk != "x-mirasim-client" && lk != sealHeaderName {
			t.Errorf("header %s survived the seal in plaintext", lk)
		}
	}
	if h.Get(sealHeaderName) == "" {
		t.Error("x-mirasim-enc was not set")
	}
	if h.Get("x-mirasim-client") != ClientVersion {
		t.Error("x-mirasim-client must stay plaintext")
	}
	// The caller's beta set is still untouched after signing.
	if got := h.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14,context-1m-2025-08-07" {
		t.Errorf("anthropic-beta was rewritten: %q", got)
	}
}

func TestLocaleIsStablePerAccount(t *testing.T) {
	first := LocaleForAccount("usr_abc")
	for i := 0; i < 20; i++ {
		if got := LocaleForAccount("usr_abc"); got != first {
			t.Fatalf("locale drifted: %q then %q", first, got)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		seen[LocaleForAccount(string(rune('a'+i%26))+"_"+time.Unix(int64(i), 0).String())] = true
	}
	if len(seen) < 2 {
		t.Fatal("locale does not spread across the population")
	}
}

func TestSignatureChangesWithBody(t *testing.T) {
	signer, err := NewDeviceSigner(testSeed)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, nonceSize)
	now := time.UnixMilli(1786250000123)
	a, err := signer.headersWithNonce("POST", "/v1/messages", []byte(`{"a":1}`), "cred", "", now, nonce)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signer.headersWithNonce("POST", "/v1/messages", []byte(`{"a":2}`), "cred", "", now, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if a["x-mirasim-sig"] == b["x-mirasim-sig"] {
		t.Fatal("signature is not covering the body — a one-byte body change produced the same signature")
	}
	// ...and the path, and the credential.
	c, _ := signer.headersWithNonce("POST", "/v1/messages/count_tokens", []byte(`{"a":1}`), "cred", "", now, nonce)
	if a["x-mirasim-sig"] == c["x-mirasim-sig"] {
		t.Fatal("signature is not covering the path")
	}
	d, _ := signer.headersWithNonce("POST", "/v1/messages", []byte(`{"a":1}`), "other", "", now, nonce)
	if a["x-mirasim-sig"] == d["x-mirasim-sig"] {
		t.Fatal("signature is not covering the credential")
	}
	e, _ := signer.headersWithNonce("POST", "/v1/messages", []byte(`{"a":1}`), "cred", "x-mirasim-agent\x00claude", now, nonce)
	if a["x-mirasim-sig"] == e["x-mirasim-sig"] {
		t.Fatal("signature is not covering the meta (context headers)")
	}
}

func TestNULInCanonicalFieldIsRejected(t *testing.T) {
	signer, err := NewDeviceSigner(testSeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Headers("POST", "/v1/messages", nil, "bad\x00cred", "", time.Now()); err == nil {
		t.Fatal("a NUL in a canonical field must be rejected: the \\x00 join separator could otherwise be spoofed")
	}
}

// redact keeps secrets out of test output the same way production logging must.
func redact(s string) string {
	if len(s) <= 6 {
		return "***"
	}
	return s[:3] + "...(" + itoa(len(s)) + " chars)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
