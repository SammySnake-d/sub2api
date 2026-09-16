package ccore

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestReturnedBuffersDoNotGrowInstanceMemory(t *testing.T) {
	if err := ensure(); err != nil {
		t.Fatal(err)
	}
	in := acquire()
	defer in.release()
	pub, eph, nonce := make([]byte, 32), make([]byte, 32), make([]byte, 12)
	for _, b := range [][]byte{pub, eph, nonce} {
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
	}
	segs := [][]byte{pub, eph, nonce, bytes.Repeat([]byte("x"), 16<<10), []byte("mrs-seal-v1\nPOST\n/v1/messages")}
	var want []byte
	for i := 0; i < 8; i++ {
		out, err := in.call(in.seal, segs, "cc_seal")
		if err != nil {
			t.Fatal(err)
		}
		want = out
	}
	before := in.mem.Size()
	for i := 0; i < 512; i++ {
		out, err := in.call(in.seal, segs, "cc_seal")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, want) {
			t.Fatal("returned bytes changed while recycling memory")
		}
	}
	after := in.mem.Size()
	t.Logf("WASM memory before=%d after=%d growth=%d bytes", before, after, after-before)
	if after > before+(128<<10) {
		t.Fatalf("bounded calls retained %d bytes of WASM result memory", after-before)
	}
}
