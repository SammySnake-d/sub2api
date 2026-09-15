package ccore

import (
	"crypto/rand"
	"strings"
	"testing"
)

func TestNilBody(t *testing.T) {
	seed := make([]byte, 32)
	rand.Read(seed)
	canon := strings.Join([]string{"GET", "/v1/limits", "1725270000000", "non", "dev", "0.0.271", "tok"}, "\x00")
	sig, err := Sign(seed, canon, "", nil)
	if err != nil || len(sig) != 64 {
		t.Fatalf("nil-body sign len=%d err=%v", len(sig), err)
	}
	t.Logf("nil-body sign ok len=%d", len(sig))
}
