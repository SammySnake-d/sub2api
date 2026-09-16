package repository

// Mirasim signing decorator.
//
// mirasim is an Anthropic-protocol upstream that additionally requires a
// per-request device signature: five x-mirasim-* headers, of which only
// x-mirasim-client travels in the clear. Everything else is ed25519-signed over
// (METHOD, path, ts, nonce, deviceId, clientVersion, credential) plus the body,
// then sealed into x-mirasim-enc.
//
// Rather than becoming sub2api's ninth parallel provider path, mirasim reuses
// the existing platform=anthropic forwarding code unchanged and injects the
// signature at the single seam where a fully-formed *http.Request (headers AND
// body) meets its account id: service.HTTPUpstream. Non-mirasim accounts are
// delegated byte-for-byte untouched.
//
// WHY THIS LAYER AND NOT EARLIER: the signature covers the final header set and
// the final body, so it must be applied after EVERY rewrite. Inside
// buildUpstreamRequest the last step is account.ApplyHeaderOverrides, which runs
// after all other header logic; HTTPUpstream is strictly later than that.
// HTTPUpstream.Do/DoWithTLS perform no rewriting of anthropic requests, and the
// retry/failover paths rebuild a brand-new request (and therefore re-sign)
// rather than replaying a signed one.
//
// This decorator writes ONLY: the Authorization header, the x-mirasim-*
// namespace, the canonical client-identity headers, and (on the /v1/limits
// probe) accept-encoding — plus exactly ONE body field: metadata.user_id, via a
// surgical sjson edit (see mirasim_identity_body.go for why that one field has
// to be converged here and nowhere else). Nothing here ever round-trips the body
// through map[string]any: that would reorder keys and HTML-escape < > &,
// destroying the upstream prompt-cache prefix.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// mirasimAccountStore is the narrow slice of the account repository the
// decorator needs. It is declared here rather than added to
// service.AccountRepository so the shared interface (and every test fake that
// implements it) stays untouched; *accountRepository already satisfies it.
type mirasimAccountStore interface {
	GetByID(ctx context.Context, id int64) (*service.Account, error)
	UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error
	UpdateExtra(ctx context.Context, id int64, updates map[string]any) error
}

// mirasimBindingTTL bounds how long a resolved account identity is reused
// without re-reading the database. Token freshness does NOT depend on this:
// rotated tokens live in the in-memory registry and a stale snapshot is only
// ever adopted when it is strictly newer (see mirasim.Registry.Prepare).
const mirasimBindingTTL = 30 * time.Second

// errMirasimProxyInURL refuses to sign a request whose URL carries the proxy
// query parameter that GatewayService.buildCustomRelayURL appends, because that
// parameter contains the proxy's credentials in cleartext and would hand them to
// the upstream. That code path is only reachable for non-apikey accounts with
// custom_base_url enabled — and a mirasim account is an apikey account, so it is
// structurally unreachable today. The guard is fail-closed insurance against a
// future refactor making it reachable.
var errMirasimProxyInURL = errors.New("mirasim: refusing to sign a request whose URL carries a proxy= parameter (proxy credentials would leak upstream)")

type mirasimBinding struct {
	enabled  bool
	identity mirasim.Identity
	expires  time.Time
}

type mirasimUpstream struct {
	next  service.HTTPUpstream
	store mirasimAccountStore
	reg   *mirasim.Registry

	mu       sync.RWMutex
	bindings map[int64]mirasimBinding
}

// NewMirasimUpstream wraps an HTTPUpstream so that requests belonging to a
// mirasim account are signed and sealed before they go out. A nil store (or a
// repository that cannot read/write account state) degrades to plain delegation.
func NewMirasimUpstream(next service.HTTPUpstream, store mirasimAccountStore) service.HTTPUpstream {
	if next == nil || store == nil {
		return next
	}
	return &mirasimUpstream{
		next:     next,
		store:    store,
		reg:      mirasim.NewRegistry(),
		bindings: map[int64]mirasimBinding{},
	}
}

func (m *mirasimUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	signed, err := m.sign(req, proxyURL, accountID, accountConcurrency)
	if err != nil {
		return nil, err
	}
	if signed {
		// A mirasim request goes out with Mirasim.app's own TLS fingerprint
		// (tlsfingerprint.MirasimProfile) even when the caller asked for the
		// plain Do path: the handshake is the first packet, so the upstream
		// classifies the client before it reads a single header.
		return m.next.DoWithTLS(req, proxyURL, accountID, accountConcurrency, tlsfingerprint.MirasimProfile())
	}
	return m.next.Do(req, proxyURL, accountID, accountConcurrency)
}

func (m *mirasimUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	signed, err := m.sign(req, proxyURL, accountID, accountConcurrency)
	if err != nil {
		return nil, err
	}
	if signed {
		// Override whatever profile the caller resolved: sub2api's default is the
		// Claude Code CLI fingerprint, and a mirasim account is an Electron app.
		profile = tlsfingerprint.MirasimProfile()
	}
	return m.next.DoWithTLS(req, proxyURL, accountID, accountConcurrency, profile)
}

// sign returns true when the request was signed (i.e. it belongs to a mirasim
// account). It is a no-op returning false for every other account.
func (m *mirasimUpstream) sign(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (bool, error) {
	if req == nil || accountID <= 0 {
		return false, nil
	}
	ctx := req.Context()

	// 账号归属先查：它同时决定「要不要签名」和「要不要统一身份头」，而后者对
	// **不签名**的控制面调用也必须生效。binding 带 TTL 缓存（见 binding()），
	// 把它提到前面不会给控制面调用多加一次存储访问。
	binding, ok := m.binding(ctx, accountID)
	if !ok || !binding.enabled {
		return false, nil
	}

	// 画像一致性的**唯一**落点。放在这里而不是放在网关，是因为网关只覆盖客户流量：
	// 账号健康检查、计划探测、额度探测都不走网关，此前它们拿的是
	// service.defaultFingerprint 那份陈旧值（Linux / 0.94.0 / v24.3.0 / "(external, cli)"），
	// 与客户流量的 MacOS / 0.112.1 / v26.3.0 / "(external, sdk-cli)" 不是同一台设备。
	// 上游看到的就是一个账号在两台机器之间跳。详见 mirasim.ApplyCanonicalIdentityHeaders。
	//
	// 刻意放在 MirasimSigningDisabled 检查**之前**：/auth/refresh 与 /auth/referral
	// 不签名，但它们同样是这个账号发出去的请求，身份不该在那里露出第二张脸。
	mirasim.ApplyCanonicalIdentityHeaders(req.Header)

	// An explicitly marked control-plane call (auth server: /auth/refresh,
	// /auth/referral) authenticates with the plain access-token bearer its caller
	// set. Signing would overwrite that bearer with the relay-issued device
	// ticket, which the auth server never issued. The caller still picks the
	// proxy and TLS profile, so the egress is unchanged.
	if service.MirasimSigningDisabled(ctx) {
		return false, nil
	}

	if req.URL != nil && req.URL.Query().Has("proxy") {
		return false, errMirasimProxyInURL
	}

	// Go follows redirects by default, and a redirect changes the path — which
	// the signature covers — so any hop would arrive unsigned-for-that-path and
	// be rejected. Mark the request so the shared upstream client refuses to
	// follow. Mutating through the pointer keeps the caller's *http.Request.
	*req = *req.WithContext(service.WithHTTPUpstreamRedirectsDisabled(ctx))
	ctx = req.Context()

	// The signature covers the body, so it has to be materialised here. Anthropic
	// request bodies are already fully buffered by the forwarding layer (response
	// streaming is what streams), so this does not defeat anything.
	body, err := drainRequestBody(req)
	if err != nil {
		return false, err
	}

	doer := &mirasimDoer{next: m.next, proxyURL: proxyURL, accountID: accountID, concurrency: accountConcurrency}
	prepared, err := m.reg.Prepare(ctx, accountID, binding.identity, doer, m.persistCredentials(), m.persistExtra())
	if err != nil {
		return false, err
	}

	// body 层身份收敛（metadata.user_id）。**必须在 SignAndSeal 之前**：签名覆盖 body
	// （ccore.Sign 把 body 一起哈希），签完再改就是签名与上线字节不一致 → 403。
	// 与上面的 ApplyCanonicalIdentityHeaders 同在一个函数体，所以"头和 body 说的是
	// 同一台设备"是结构上成立的，不是两处各自维护。改写后的字节同时装回了 req
	// （Body/ContentLength/GetBody），下面签的就是这一份。
	// 为什么不复用网关里已有的那套 user_id 重写、以及它为什么对 mirasim 从未执行过：
	// 见 mirasim_identity_body.go 顶部。
	body = applyMirasimCanonicalUserID(req, body, prepared)

	// Upstream auth is the device ticket (or the access token when no ticket
	// could be minted) — and it MUST be the same string that goes into the
	// canonical signing tuple.
	req.Header.Del("x-api-key")
	req.Header.Set("Authorization", "Bearer "+prepared.Credential)

	sessionID := firstNonEmpty(
		req.Header.Get("x-claude-code-session-id"),
		req.Header.Get("x-mirasim-session"),
		prepared.SessionID,
	)
	signaturePath := req.URL.Path
	if signaturePath == "" {
		signaturePath = "/"
	}
	mirasim.ApplyContextHeaders(req.Header, mirasim.ContextInput{
		Path:       signaturePath,
		SessionID:  sessionID,
		AccountSub: prepared.AccountSub,
		Locale:     prepared.Locale,
	})

	// The query string is deliberately NOT signed (the client signs the path
	// only), so ?beta=true and friends pass through untouched.
	if err := mirasim.SignAndSeal(req.Header, prepared.Signer, req.Method, signaturePath, body, prepared.Credential); err != nil {
		return false, err
	}
	return true, nil
}

// persistCredentials writes rotated tokens back. Credentials (not extra) because
// that column is redacted by the admin DTO layer.
func (m *mirasimUpstream) persistCredentials() mirasim.Persister {
	return func(ctx context.Context, accountID int64, updates map[string]any) error {
		acc, err := m.store.GetByID(ctx, accountID)
		if err != nil {
			return err
		}
		merged := map[string]any{}
		for k, v := range acc.Credentials {
			merged[k] = v
		}
		for k, v := range updates {
			merged[k] = v
		}
		if err := m.store.UpdateCredentials(ctx, accountID, merged); err != nil {
			return err
		}
		m.invalidate(accountID)
		return nil
	}
}

// persistExtra writes the non-secret stable session id. It goes to extra rather
// than credentials for two reasons: it is not a secret, and every write to
// credentials on an apikey account drops extra.upstream_billing_probe
// (account_repo.go UpdateCredentials), which a per-account one-time write should
// not be causing.
func (m *mirasimUpstream) persistExtra() mirasim.Persister {
	return func(ctx context.Context, accountID int64, updates map[string]any) error {
		if err := m.store.UpdateExtra(ctx, accountID, updates); err != nil {
			return err
		}
		m.invalidate(accountID)
		return nil
	}
}

func (m *mirasimUpstream) invalidate(accountID int64) {
	m.mu.Lock()
	delete(m.bindings, accountID)
	m.mu.Unlock()
}

func (m *mirasimUpstream) binding(ctx context.Context, accountID int64) (mirasimBinding, bool) {
	now := time.Now()
	m.mu.RLock()
	b, ok := m.bindings[accountID]
	m.mu.RUnlock()
	if ok && now.Before(b.expires) {
		return b, true
	}

	acc, err := m.store.GetByID(ctx, accountID)
	if err != nil || acc == nil {
		// A lookup failure must not break non-mirasim traffic: treat it as "not a
		// mirasim account" and let the request through unsigned. A mirasim account
		// would then fail upstream with a clear auth error rather than silently
		// here.
		return mirasimBinding{}, false
	}
	b = mirasimBinding{expires: now.Add(mirasimBindingTTL)}
	if isMirasimAccount(acc) {
		b.enabled = true
		b.identity = mirasim.Identity{
			DeviceSeed:   acc.GetCredential(mirasim.CredDeviceSeed),
			AccessToken:  acc.GetCredential(mirasim.CredAccessToken),
			RefreshToken: acc.GetCredential(mirasim.CredRefreshToken),
			AuthBase:     acc.GetCredential(mirasim.CredAuthBase),
			RelayBase:    strings.TrimSpace(acc.GetCredential("base_url")),
			SessionID:    extraString(acc, mirasim.ExtraSessionID),
		}
		if t := acc.GetCredentialAsTime(mirasim.CredExpiresAt); t != nil {
			b.identity.ExpiresAt = *t
		}
		if b.identity.DeviceSeed == "" {
			logger.LegacyPrintf("repository.mirasim", "[mirasim] account=%d is marked provider=mirasim but has no device seed; requests will fail", accountID)
		}
	}
	m.mu.Lock()
	m.bindings[accountID] = b
	m.mu.Unlock()
	return b, true
}

func extraString(acc *service.Account, key string) string {
	if acc == nil || acc.Extra == nil {
		return ""
	}
	s, _ := acc.Extra[key].(string)
	return strings.TrimSpace(s)
}

// isMirasimAccount keys off one explicit credential marker. It intentionally
// does NOT sniff base_url: an operator must opt an account in, and the same
// marker is what the admin surface sets.
func isMirasimAccount(acc *service.Account) bool {
	if acc == nil || acc.Platform != domain.PlatformAnthropic {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(acc.GetCredential(mirasim.CredProvider)), mirasim.ProviderMirasim)
}

// mirasimDoer routes mirasim's control-plane calls (token refresh, device
// session mint) through the SAME upstream client — hence the same proxy, the
// same egress IP, the same TLS fingerprint and the same connection pool — as the
// account's data plane. It calls the wrapped implementation directly, so these
// requests are never re-entered by the decorator.
type mirasimDoer struct {
	next        service.HTTPUpstream
	proxyURL    string
	accountID   int64
	concurrency int
}

func (d *mirasimDoer) Do(req *http.Request) (*http.Response, error) {
	*req = *req.WithContext(service.WithHTTPUpstreamRedirectsDisabled(req.Context()))
	return d.next.DoWithTLS(req, d.proxyURL, d.accountID, d.concurrency, tlsfingerprint.MirasimProfile())
}

// drainRequestBody reads the request body into memory and installs a replayable
// copy, so the signature covers exactly the bytes that go on the wire and the
// transport (including its own retries via GetBody) still sees a readable body.
func drainRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	if req.GetBody != nil {
		rc, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		body, err := io.ReadAll(rc)
		if err != nil {
			return nil, err
		}
		return body, nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return body, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
