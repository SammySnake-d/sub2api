package mirasim

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim/ccore"
)

const (
	sealHeaderName = "x-mirasim-enc"
	sealSchemeLine = "mrs-seal-v1"
	// sealPubKeyB64 is the fixed relay seal key shipped with the upstream client.
	sealPubKeyB64 = "HlyNMMeGXryasYLJuYQ/9ksCD4AYVVy1zXKAtJdpJn4="
)

var sealPubKey = func() []byte { b, _ := base64.StdEncoding.DecodeString(sealPubKeyB64); return b }()

const (
	sealEphSize   = 32
	sealNonceSize = 12
)

// SealHeaders reproduces the client's header-sealing step: every plaintext
// x-mirasim-* header (except x-mirasim-client and x-mirasim-enc itself) is
// collected into a JSON object keyed by the lowercase header name, sealed to the
// fixed relay key via crypto-core (x25519 + AEAD, AAD =
// "mrs-seal-v1\nMETHOD\npath"), placed in x-mirasim-enc, and the originals are
// deleted. Nothing sensitive (device id, ts, nonce, signature, session, agent,
// ...) is sent in the clear. No-op if no such headers.
//
// Ported verbatim from ma-relay internal/relay/seal.go.
func SealHeaders(h http.Header, method, path string) error {
	eph := make([]byte, sealEphSize)
	if _, err := rand.Read(eph); err != nil {
		return err
	}
	nonce := make([]byte, sealNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	return sealHeadersWith(h, method, path, eph, nonce)
}

// sealHeadersWith is SealHeaders with the ephemeral X25519 seed and AEAD nonce
// injected. ma-relay draws both from crypto/rand inside SealHeaders and exposes
// no seam, so this split is the port's second deliberate structural addition: it
// is what makes the sealed blob reproducible for the differential test. The
// plaintext assembly, key set, AAD and ccore call below are unchanged.
func sealHeadersWith(h http.Header, method, path string, eph, nonce []byte) error {
	plain, names := sealablePlaintext(h)
	if len(plain) == 0 {
		return nil
	}
	pt, err := json.Marshal(plain)
	if err != nil {
		return err
	}
	aad := sealAAD(method, path)
	blob, err := ccore.Seal(sealPubKey, eph, nonce, pt, aad)
	if err != nil {
		return err
	}
	for _, n := range names {
		h.Del(n)
	}
	h.Set(sealHeaderName, base64.RawURLEncoding.EncodeToString(blob))
	return nil
}

// sealablePlaintext collects the header set that goes inside the seal: every
// x-mirasim-* EXCEPT x-mirasim-client (must stay plaintext so the server can
// read the version before/without unsealing) and the enc header itself.
func sealablePlaintext(h http.Header) (map[string]string, []string) {
	plain := map[string]string{}
	var names []string
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-mirasim-") && lk != "x-mirasim-client" && lk != sealHeaderName {
			if v := h.Get(k); v != "" {
				plain[lk] = v
				names = append(names, k)
			}
		}
	}
	return plain, names
}

func sealAAD(method, path string) []byte {
	return []byte(sealSchemeLine + "\n" + strings.ToUpper(method) + "\n" + path)
}

// metaFromHeaders reproduces the meta string the server reconstructs to verify a
// request: over the sealed header set it keeps every x-mirasim-* header except
// the four signature headers (device/ts/nonce/sig), the client version and enc,
// so it retains the context headers (session/agent/call/locale/account/probe/...).
// It is then serialised as k\0v\0k\0v... Keys are sorted so the serialisation
// matches the server iterating our (json.Marshal-sorted) seal JSON. Empty values
// are skipped.
//
// Ported verbatim from ma-relay internal/relay/seal.go.
func metaFromHeaders(h http.Header) string {
	m := map[string]string{}
	for k := range h {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-mirasim-") {
			continue
		}
		switch lk {
		case "x-mirasim-device", "x-mirasim-ts", "x-mirasim-nonce", "x-mirasim-sig", "x-mirasim-client", sealHeaderName:
			continue
		}
		if v := h.Get(k); v != "" {
			m[lk] = v
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(0)
		}
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(m[k])
	}
	return b.String()
}

// SignAndSeal applies the full relay protocol to an outbound request: derive
// meta from the already-set context headers, ed25519-sign the canonical + meta +
// body via crypto-core, attach the signature headers, then seal every
// x-mirasim-* header into x-mirasim-enc. Any context headers (session, agent,
// call, locale, account, probe) must be set on h BEFORE calling this — the seal
// has to cover the final header set.
//
// path must be the URL path only; the query string is not signed.
//
// Ported verbatim from ma-relay internal/relay/seal.go.
func SignAndSeal(h http.Header, signer *DeviceSigner, method, path string, body []byte, credential string) error {
	if signer == nil {
		return nil
	}
	meta := metaFromHeaders(h)
	signed, err := signer.Headers(method, path, body, credential, meta, time.Now())
	if err != nil {
		return err
	}
	for k, v := range signed {
		h.Set(k, v)
	}
	return SealHeaders(h, method, path)
}

// signAndSealWith is SignAndSeal with every source of non-determinism injected:
// the signing timestamp, the signature nonce, the seal ephemeral seed and the
// seal AEAD nonce. Tests only.
func signAndSealWith(h http.Header, signer *DeviceSigner, method, path string, body []byte, credential string, now time.Time, sigNonce, eph, sealNonce []byte) error {
	if signer == nil {
		return nil
	}
	meta := metaFromHeaders(h)
	signed, err := signer.headersWithNonce(method, path, body, credential, meta, now, sigNonce)
	if err != nil {
		return err
	}
	for k, v := range signed {
		h.Set(k, v)
	}
	return sealHeadersWith(h, method, path, eph, sealNonce)
}
