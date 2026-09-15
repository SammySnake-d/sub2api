package mirasim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Credential field names inside sub2api's accounts.credentials JSONB column.
//
// Why credentials and not new columns: accounts.credentials is already the
// per-account secret/knob bag (api_key, access_token, refresh_token, expires_at
// for OAuth accounts; pool_mode, custom_error_codes, temp_unschedulable_rules
// for behaviour knobs) and it already has a repository write path
// (AccountRepository.UpdateCredentials) that the token refresh needs. A mirasim
// account adds exactly one field that has no existing analogue — the device seed
// — so a migration would buy nothing.
const (
	// CredProvider marks an anthropic-platform account as a mirasim account.
	// Value must be ProviderMirasim. This is the ONLY thing the upstream
	// decorator keys off: no new platform enum, no new account type.
	CredProvider = "provider"
	// CredDeviceSeed is the base64 (raw std, unpadded) 32-byte ed25519 device
	// seed. Persistent per account: the upstream binds the derived device id to
	// the account, so rotating it looks like a new device every restart.
	CredDeviceSeed = "mirasim_device_seed"
	// CredAccessToken / CredRefreshToken / CredExpiresAt mirror the field names
	// sub2api already uses for OAuth accounts, so the existing admin surfaces
	// keep working.
	CredAccessToken  = "access_token"
	CredRefreshToken = "refresh_token"
	CredExpiresAt    = "expires_at"
	// CredAuthBase is the account's own auth server (token refresh lives there,
	// not on the relay). Falls back to DefaultAuthBase.
	CredAuthBase = "mirasim_auth_base"
)

// ExtraSessionID is the account's stable x-mirasim-session fallback, minted on
// first use and persisted in accounts.extra (NOT credentials: it is not a
// secret, and every credentials write on an apikey account drops
// extra.upstream_billing_probe).
//
// It MUST be durable. Without persistence every account mints a fresh session id
// on every process start, so the whole pool rotates its session ids in lockstep
// on each restart — a signal no set of independently installed real clients
// would ever produce.
const ExtraSessionID = "mirasim_session_id"

// ProviderMirasim is the CredProvider value that turns on mirasim signing.
const ProviderMirasim = "mirasim"

// DefaultAuthBase is where token refresh goes when the account stores none.
// mirasim's auth server is a separate host from the relay.
const DefaultAuthBase = "https://auth.mirasim.ai"

const (
	refreshBefore       = 5 * time.Minute
	defaultTokenLife    = 50 * time.Minute
	ticketRefreshBefore = 2 * time.Minute
	refreshTimeout      = 30 * time.Second
	ticketTimeout       = 10 * time.Second
	maxControlBody      = 1 << 20
)

// Identity is one mirasim account's persisted state, read out of
// accounts.credentials. Tokens in here are secrets: never log an Identity.
type Identity struct {
	DeviceSeed   string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	AuthBase     string
	RelayBase    string
	SessionID    string
}

// Doer is the minimal HTTP client the control-plane calls (token refresh, device
// session mint) need. The caller supplies one that egresses exactly like the
// account's data-plane traffic, so an account's IP stays consistent.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Persister writes credential updates back to durable storage. Called with only
// the changed fields.
type Persister func(ctx context.Context, accountID int64, updates map[string]any) error

// RefreshRejectedError signals that mirasim's OWN auth server answered the
// refresh call with a non-2xx status, as opposed to the request never arriving
// (network/transport failure). Callers can distinguish "this refresh token is
// dead" from "the auth server is unhealthy".
type RefreshRejectedError struct{ Status int }

func (e *RefreshRejectedError) Error() string {
	return fmt.Sprintf("mirasim refresh returned HTTP %d", e.Status)
}

// IsHardAuthRejection reports whether err is a confirmed rejection of the
// refresh token (400/401/403) rather than a transport failure or a 5xx, which
// say nothing about this credential.
func IsHardAuthRejection(err error) bool {
	var e *RefreshRejectedError
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == 400 || e.Status == 401 || e.Status == 403
}

// Credential is the live per-account runtime state: the device signer, the
// current access token, and the short-lived device ticket that is what actually
// gets signed into requests. All mutation is under mu, so concurrent requests on
// one account share a single refresh and a single ticket mint.
type Credential struct {
	accountID int64

	mu               sync.Mutex
	signer           *DeviceSigner
	seed             string
	accessToken      string
	refreshToken     string
	expiresAt        time.Time
	authBase         string
	relayBase        string
	sessionID        string
	ticket           string
	ticketExpiresAt  time.Time
	ticketIssuerHash string
}

// Registry holds one Credential per account id.
type Registry struct {
	mu    sync.Mutex
	creds map[int64]*Credential
}

func NewRegistry() *Registry { return &Registry{creds: map[int64]*Credential{}} }

func (r *Registry) get(accountID int64) *Credential {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.creds[accountID]
	if !ok {
		c = &Credential{accountID: accountID}
		r.creds[accountID] = c
	}
	return c
}

// Forget drops an account's cached runtime state (used when an account is
// deleted or its credentials are rewritten out of band).
func (r *Registry) Forget(accountID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.creds, accountID)
}

// Prepared is everything the request assembly needs for one outbound call.
type Prepared struct {
	// Credential is the value signed into the canonical string AND sent as the
	// Authorization bearer: the device ticket when one could be minted, else the
	// raw access token. The two must be the same string.
	Credential string
	Signer     *DeviceSigner
	// AccountSub is the access token's JWT "sub" (x-mirasim-account telemetry).
	AccountSub string
	// SessionID is the account's stable session fallback.
	SessionID string
	// Locale is the stable per-account locale.
	Locale string
}

// Prepare brings an account's credential up to date and returns everything
// needed to sign one request: it syncs the freshly-read persisted identity,
// refreshes the access token if it is within refreshBefore of expiry, and mints
// or reuses the device ticket. Concurrency-safe per account: the whole sequence
// runs under the account's mutex, so N concurrent requests trigger one refresh
// and one ticket mint, not N.
func (r *Registry) Prepare(ctx context.Context, accountID int64, ident Identity, client Doer, persistCredentials, persistExtra Persister) (Prepared, error) {
	c := r.get(accountID)
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.syncLocked(ident); err != nil {
		return Prepared{}, err
	}
	if err := c.ensureFreshLocked(ctx, client, persistCredentials); err != nil {
		return Prepared{}, err
	}
	c.ensureSessionIDLocked(ctx, persistExtra)
	cred := c.authorizationLocked(ctx, client)
	sub := jwtClaimString(c.accessToken, "sub")
	// Locale is keyed on the upstream account identity when known, falling back
	// to the local account id so distinct accounts still spread rather than all
	// collapsing to en-US.
	localeKey := sub
	if localeKey == "" {
		localeKey = strconv.FormatInt(accountID, 10)
	}
	return Prepared{
		Credential: cred,
		Signer:     c.signer,
		AccountSub: sub,
		SessionID:  c.sessionID,
		Locale:     LocaleForAccount(localeKey),
	}, nil
}

// syncLocked folds the persisted identity into the live state. A device seed
// change rebuilds the signer; an access token changed out of band (another
// process refreshed it) invalidates the ticket, which is bound to its issuer.
func (c *Credential) syncLocked(ident Identity) error {
	if strings.TrimSpace(ident.DeviceSeed) == "" {
		return errors.New("mirasim account has no device seed")
	}
	if c.signer == nil || c.seed != ident.DeviceSeed {
		signer, err := NewDeviceSigner(ident.DeviceSeed)
		if err != nil {
			return err
		}
		c.signer = signer
		c.seed = ident.DeviceSeed
		c.ticket = ""
		c.ticketExpiresAt = time.Time{}
		c.ticketIssuerHash = ""
	}
	// Only adopt a persisted token that is NOT older than what we hold: our own
	// in-memory token may already be a rotation this process performed but whose
	// read-back has not landed yet.
	if ident.AccessToken != "" && ident.AccessToken != c.accessToken {
		persistedExp := tokenExpiry(ident.AccessToken)
		if c.accessToken == "" || persistedExp.After(c.expiresAt) {
			c.accessToken = ident.AccessToken
			c.expiresAt = ident.ExpiresAt
			if !persistedExp.IsZero() {
				c.expiresAt = persistedExp
			}
			c.ticket = ""
			c.ticketExpiresAt = time.Time{}
			c.ticketIssuerHash = ""
		}
	} else if ident.AccessToken != "" && c.expiresAt.IsZero() {
		c.expiresAt = ident.ExpiresAt
	}
	if ident.RefreshToken != "" {
		c.refreshToken = ident.RefreshToken
	}
	c.authBase = strings.TrimSpace(ident.AuthBase)
	if c.authBase == "" {
		c.authBase = DefaultAuthBase
	}
	c.relayBase = strings.TrimRight(strings.TrimSpace(ident.RelayBase), "/")
	if c.relayBase == "" {
		c.relayBase = DefaultRelayBase
	}
	if s := strings.TrimSpace(ident.SessionID); s != "" {
		c.sessionID = s
	}
	return nil
}

func (c *Credential) ensureSessionIDLocked(ctx context.Context, persist Persister) {
	if c.sessionID != "" {
		return
	}
	c.sessionID = randomPrefixedID("session_")
	if persist != nil {
		// Best effort: a lost write only costs a new session id next restart.
		_ = persist(ctx, c.accountID, map[string]any{ExtraSessionID: c.sessionID})
	}
}

// ensureFreshLocked refreshes the access token when it is inside refreshBefore
// of expiry, then persists the rotation.
//
// Ported from ma-relay internal/relay/pool.go EnsureFresh.
func (c *Credential) ensureFreshLocked(ctx context.Context, client Doer, persist Persister) error {
	if !c.expiresAt.IsZero() && time.Until(c.expiresAt) > refreshBefore {
		return nil
	}
	if c.expiresAt.IsZero() && c.accessToken != "" {
		// No expiry known and a token in hand: trust it until upstream says no.
		return nil
	}
	if c.refreshToken == "" {
		if c.accessToken != "" && (c.expiresAt.IsZero() || time.Now().Before(c.expiresAt)) {
			return nil
		}
		return errors.New("mirasim access token expired and no refresh token is stored")
	}
	body, err := json.Marshal(map[string]string{"refresh_token": c.refreshToken})
	if err != nil {
		return err
	}
	controlCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	authURL := strings.TrimRight(c.authBase, "/") + "/auth/refresh"
	req, err := http.NewRequestWithContext(controlCtx, http.MethodPost, authURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mirasim refresh transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxControlBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &RefreshRejectedError{Status: resp.StatusCode}
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		ExpiresAt    int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("decode mirasim refresh response: %w", err)
	}
	if result.AccessToken == "" {
		return errors.New("mirasim refresh response has no access_token")
	}
	expires := tokenExpiry(result.AccessToken)
	if expires.IsZero() && result.ExpiresAt > 0 {
		expires = time.Unix(result.ExpiresAt, 0).UTC()
	}
	if expires.IsZero() && result.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UTC()
	}
	if expires.IsZero() {
		expires = time.Now().Add(defaultTokenLife).UTC()
	}
	refresh := result.RefreshToken
	if refresh == "" {
		refresh = c.refreshToken
	}
	if persist != nil {
		if err := persist(ctx, c.accountID, map[string]any{
			CredAccessToken:  result.AccessToken,
			CredRefreshToken: refresh,
			CredExpiresAt:    expires.Format(time.RFC3339),
		}); err != nil {
			return fmt.Errorf("persist rotated mirasim credential: %w", err)
		}
	}
	c.accessToken = result.AccessToken
	c.refreshToken = refresh
	c.expiresAt = expires
	c.ticket = ""
	c.ticketExpiresAt = time.Time{}
	c.ticketIssuerHash = ""
	return nil
}

// authorizationLocked returns the credential string to sign with and send as the
// bearer: a device ticket minted from the access token when the relay issues
// one, otherwise the access token itself. A failed mint is NOT an error — the
// access token remains a valid credential.
//
// Ported from ma-relay internal/relay/pool.go Authorization.
func (c *Credential) authorizationLocked(ctx context.Context, client Doer) string {
	issuerHash := shortHash(c.accessToken)
	if c.ticket != "" && c.ticketIssuerHash == issuerHash && time.Until(c.ticketExpiresAt) > ticketRefreshBefore {
		return c.ticket
	}
	requestBody, err := json.Marshal(struct {
		PublicKey string `json:"publicKey"`
		DeviceID  string `json:"deviceId"`
	}{PublicKey: c.signer.PublicKeyB64, DeviceID: c.signer.DeviceID})
	if err != nil {
		return c.accessToken
	}
	controlCtx, cancel := context.WithTimeout(ctx, ticketTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(controlCtx, http.MethodPost, c.relayBase+"/v1/device/session", bytes.NewReader(requestBody))
	if err != nil {
		return c.accessToken
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.accessToken)
	// The device-session mint is signed PLAINTEXT: device/ts/nonce/sig + client,
	// empty meta, NO seal and NO context headers — exactly like the app, which
	// builds this one request outside the seal pipeline.
	if hdrs, herr := c.signer.Headers(http.MethodPost, "/v1/device/session", requestBody, c.accessToken, "", time.Now()); herr == nil {
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return c.accessToken
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxControlBody))
	if readErr != nil {
		return c.accessToken
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.accessToken
	}
	var result struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int64  `json:"expiresIn"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Ticket == "" {
		return c.accessToken
	}
	expires := time.Now().Add(10 * time.Minute)
	if result.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	} else if result.ExpiresAt > 0 {
		expires = time.Unix(result.ExpiresAt, 0)
	}
	c.ticket = result.Ticket
	c.ticketExpiresAt = expires
	c.ticketIssuerHash = issuerHash
	return c.ticket
}

// ClearTicket forces the next request on this account to re-mint a device
// ticket. Used when upstream rejects the current one.
func (r *Registry) ClearTicket(accountID int64) {
	c := r.get(accountID)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ticket = ""
	c.ticketExpiresAt = time.Time{}
	c.ticketIssuerHash = ""
}

func jwtClaimString(token, claim string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var p map[string]any
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	s, _ := p[claim].(string)
	return s
}

func tokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Expires int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Expires <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Expires, 0).UTC()
}

// shortHash is a non-reversible tag for a secret, safe to hold in memory and to
// compare. It is never logged.
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:12]
}
