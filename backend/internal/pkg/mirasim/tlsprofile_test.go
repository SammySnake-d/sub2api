package mirasim

import (
	"crypto/md5"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

// TestTLSProfileReproducesMirasimJA3 pins the ClientHello to the fingerprint
// captured from Mirasim.app's own Electron runtime. It recomputes the JA3 string
// from the profile fields rather than asserting the fields one by one, so a
// reordering or a dropped cipher is caught by the hash.
//
// JA3 = SSLVersion,Ciphers,Extensions,EllipticCurves,ECPointFormats
func TestTLSProfileReproducesMirasimJA3(t *testing.T) {
	const wantJA3Hash = "71dc8c533dd919ae9f4963224a4ba8fd"

	join := func(vals []uint16) string {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = strconv.Itoa(int(v))
		}
		return strings.Join(parts, "-")
	}

	// 771 = TLS 1.2 legacy_version in the ClientHello record (TLS 1.3 is offered
	// through supported_versions, which is how the real client does it too).
	ja3 := strings.Join([]string{
		"771",
		join(TLSProfile.CipherSuites),
		join(TLSProfile.Extensions),
		join(TLSProfile.Curves),
		join(TLSProfile.PointFormats),
	}, ",")

	sum := md5.Sum([]byte(ja3)) //nolint:gosec // JA3 is defined as MD5; not a security primitive here
	if got := hex.EncodeToString(sum[:]); got != wantJA3Hash {
		t.Fatalf("JA3 drifted from Mirasim.app's captured ClientHello\n got  %s\n want %s\n ja3  %s", got, wantJA3Hash, ja3)
	}

	// ALPN must be absent: the real client negotiates no protocol and ends up on
	// HTTP/1.1. Advertising h2 here would be visible at the first byte.
	for _, id := range TLSProfile.Extensions {
		if id == 16 {
			t.Fatal("the ALPN extension must not be advertised")
		}
	}
	if len(TLSProfile.ALPNProtocols) != 0 {
		t.Fatal("ALPNProtocols must stay empty")
	}
	if TLSProfile.EnableGREASE {
		t.Fatal("the captured ClientHello carries no GREASE")
	}
}
