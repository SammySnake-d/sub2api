package ccore

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestSmoke(t *testing.T) {
	seed := make([]byte, 32)
	rand.Read(seed)
	canon := strings.Join([]string{"POST", "/v1/messages", "1725270000000", "nonceB64url", "devid123", "0.0.271", "sk-accesstoken"}, "\x00")
	sig, err := Sign(seed, canon, "", []byte(`{"model":"claude-sonnet-5"}`))
	if err != nil || len(sig) == 0 {
		t.Fatalf("SIGN failed len=%d err=%v", len(sig), err)
	}
	t.Logf("SIGN ok len=%d head=%s", len(sig), base64.RawURLEncoding.EncodeToString(sig)[:16])
	pub, _ := base64.StdEncoding.DecodeString("HlyNMMeGXryasYLJuYQ/9ksCD4AYVVy1zXKAtJdpJn4=")
	eph := make([]byte, 32)
	rand.Read(eph)
	aad := []byte("mrs-seal-v1\nPOST\n/v1/messages")
	pt := []byte(`{"x-mirasim-device":"devid123","x-mirasim-ts":"1725270000000"}`)
	for _, nl := range []int{12, 24} {
		non := make([]byte, nl)
		rand.Read(non)
		blob, err := Seal(pub, eph, non, pt, aad)
		t.Logf("SEAL nonce=%d: len=%d err=%v", nl, len(blob), err)
	}
}
