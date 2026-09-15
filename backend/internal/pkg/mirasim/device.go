package mirasim

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim/ccore"
)

// DeviceSigner holds one mirasim account's persistent device identity: the
// ed25519 key derived from the stored device seed, the device id the upstream
// knows it by, and the SPKI public key the device-session mint registers.
//
// Ported verbatim from ma-relay internal/relay/device.go. Every gratuitous edit
// here is a place the signature can silently diverge and turn into a 403 on
// every request, so the canonical-string assembly below must stay byte-exact.
type DeviceSigner struct {
	private      ed25519.PrivateKey
	DeviceID     string
	PublicKeyB64 string
}

// NewDeviceSigner builds a signer from the base64 (raw, unpadded std alphabet)
// 32-byte ed25519 seed stored with the account.
func NewDeviceSigner(seedB64 string) (*DeviceSigner, error) {
	seed, err := base64.RawStdEncoding.DecodeString(seedB64)
	if err != nil {
		return nil, fmt.Errorf("decode device seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("device seed has %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	private := ed25519.NewKeyFromSeed(seed)
	public := private.Public().(ed25519.PublicKey)
	spki, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return nil, err
	}
	publicB64 := base64.StdEncoding.EncodeToString(spki)
	digest := sha256.Sum256([]byte(publicB64))
	deviceID := base64.RawURLEncoding.EncodeToString(digest[:])[:22]
	return &DeviceSigner{private: private, DeviceID: deviceID, PublicKeyB64: publicB64}, nil
}

// NewDeviceSeed mints a fresh base64 device seed for a new mirasim account.
func NewDeviceSeed() (string, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(seed), nil
}

// Seed returns the 32-byte ed25519 device seed, fed to crypto-core cc_sign.
func (d *DeviceSigner) Seed() []byte { return d.private.Seed() }

// nonceSize is the signature nonce length the client uses (32 random bytes,
// emitted base64url without padding).
const nonceSize = 32

// Headers builds the relay signature header set. The canonical string is
//
//	method \0 path \0 ts \0 nonce \0 deviceId \0 clientVersion \0 credential
//
// (NUL-joined). The signature is produced by the app's own crypto-core WASM over
// (seed, canonical, meta, body): the body is hashed inside the core, so it is no
// longer part of the canonical string, and credential is the account access
// token (or the device ticket minted from it). The returned headers are still
// plaintext — SealHeaders must wrap them into x-mirasim-enc before the request
// is sent.
//
// NOTE: path here is the URL path ONLY. The query string is deliberately not
// signed, matching the client.
func (d *DeviceSigner) Headers(method, path string, body []byte, credential, meta string, now time.Time) (map[string]string, error) {
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return d.headersWithNonce(method, path, body, credential, meta, now, nonce)
}

// headersWithNonce is Headers with the per-request nonce injected instead of
// drawn from crypto/rand. ma-relay has no such seam (its nonce is read straight
// from crypto/rand inside Headers), so this is the one deliberate structural
// addition of the port: without it the differential test against ma-relay could
// never compare an x-mirasim-sig byte for byte. Headers keeps ma-relay's exact
// behaviour; every caller outside tests goes through it.
func (d *DeviceSigner) headersWithNonce(method, path string, body []byte, credential, meta string, now time.Time, nonce []byte) (map[string]string, error) {
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)
	fields := []string{strings.ToUpper(method), path, ts, nonceB64, d.DeviceID, ClientVersion, credential}
	for _, f := range fields {
		if strings.ContainsRune(f, '\x00') {
			return nil, fmt.Errorf("signature field contains NUL")
		}
	}
	canonical := strings.Join(fields, "\x00")
	sig, err := ccore.Sign(d.private.Seed(), canonical, meta, body)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"x-mirasim-device": d.DeviceID,
		"x-mirasim-ts":     ts,
		"x-mirasim-nonce":  nonceB64,
		"x-mirasim-sig":    base64.RawURLEncoding.EncodeToString(sig),
		"x-mirasim-client": ClientVersion,
	}, nil
}
