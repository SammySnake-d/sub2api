// Package ccore black-box-hosts upstream's crypto-core WASM (cc_sign / cc_seal),
// extracted verbatim from Mirasim.app v0.0.271 (Resources/server.cjs). Running
// the app's own module guarantees byte-identical relay signatures without
// re-deriving the ed25519/x25519 constructions by hand.
//
// Concurrency: a WASM instance's linear memory is not concurrency-safe, so a
// single instance behind one mutex made signing a process-wide serial section —
// measured at 1.58 ms/op on a 2-vCPU box with zero speedup from a second core,
// i.e. a hard ~630 req/s ceiling for the whole relay no matter how many cores it
// has. The module is compiled once and instantiated N times instead; each
// instance owns its memory, so signing scales with cores. Instances are handed
// out through a buffered channel, which also caps concurrent WASM memory use.
package ccore

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

//go:embed ccore.wasm
var wasmBin []byte

// instance is one WASM module instance with its own linear memory.
type instance struct {
	mod   api.Module
	alloc api.Function
	free  api.Function
	sign  api.Function
	seal  api.Function
	mem   api.Memory
}

var (
	initOnce sync.Once
	initErr  error
	rt       wazero.Runtime
	pool     chan *instance
	poolSize int
)

// Instances reports how many WASM instances the pool holds (= max concurrent
// signs). Exposed so the admin surface can show the real signing concurrency
// instead of leaving it to guesswork.
func Instances() int {
	if err := ensure(); err != nil {
		return 0
	}
	return poolSize
}

func instanceCount() int {
	if v := os.Getenv("MA_CCORE_INSTANCES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	n := runtime.GOMAXPROCS(0)
	if n < 2 {
		// Even on a single core, two instances keep one signing while the other
		// is blocked in Go-side setup.
		n = 2
	}
	if n > 16 {
		n = 16 // beyond this the linear memory cost stops paying for itself
	}
	return n
}

func ensure() error {
	initOnce.Do(func() {
		ctx := context.Background()
		rt = wazero.NewRuntime(ctx)
		compiled, err := rt.CompileModule(ctx, wasmBin)
		if err != nil {
			initErr = fmt.Errorf("compile ccore.wasm: %w", err)
			return
		}
		poolSize = instanceCount()
		pool = make(chan *instance, poolSize)
		for i := 0; i < poolSize; i++ {
			// An empty module name is required to instantiate the same compiled
			// module more than once — a named module can only exist once per
			// runtime.
			m, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
			if err != nil {
				initErr = fmt.Errorf("instantiate ccore.wasm (#%d): %w", i, err)
				return
			}
			inst := &instance{
				mod:   m,
				alloc: m.ExportedFunction("cc_alloc"),
				free:  m.ExportedFunction("cc_free"),
				sign:  m.ExportedFunction("cc_sign"),
				seal:  m.ExportedFunction("cc_seal"),
				mem:   m.Memory(),
			}
			if inst.alloc == nil || inst.free == nil || inst.sign == nil || inst.seal == nil || inst.mem == nil {
				initErr = fmt.Errorf("ccore.wasm missing required exports")
				return
			}
			pool <- inst
		}
	})
	return initErr
}

// acquire takes an instance, blocking until one is free.
func acquire() *instance { return <-pool }
func (in *instance) release() {
	// Reset nothing: cc_alloc/cc_free leave the instance in a reusable state and
	// every call frees what it allocated. Returning a dirty instance would show
	// up as a cc_sign null, which callers surface as an error rather than a
	// silently wrong signature.
	pool <- in
}

// put copies data into this instance's memory, returns [ptr,len].
func (in *instance) put(ctx context.Context, data []byte) (uint64, uint64, error) {
	r, err := in.alloc.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, 0, err
	}
	ptr := r[0]
	if !in.mem.Write(uint32(ptr), data) {
		return 0, 0, fmt.Errorf("wasm mem.Write out of range")
	}
	return ptr, uint64(len(data)), nil
}

func (in *instance) freep(ctx context.Context, ptr, ln uint64) {
	_, _ = in.free.Call(ctx, ptr, ln)
}

// readRet decodes cc_sign/cc_seal return i64 = (ptr<<32)|len; ptr==0 => null.
func (in *instance) readRet(ret uint64) ([]byte, bool) {
	hi := uint32(ret >> 32)
	lo := uint32(ret & 0xffffffff)
	if hi == 0 {
		return nil, false
	}
	b, ok := in.mem.Read(hi, lo)
	if !ok {
		return nil, false
	}
	out := make([]byte, len(b))
	copy(out, b)
	// cc_sign/cc_seal allocate the return buffer too. Copying it into Go does
	// not release WASM heap ownership; without this every request permanently
	// grows the long-lived instance, eventually exhausting the host's memory.
	in.freep(context.Background(), uint64(hi), uint64(lo))
	return out, true
}

// call runs one exported function over the given byte segments, handling the
// copy-in / free-after dance for all of them.
func (in *instance) call(fn api.Function, segs [][]byte, what string) ([]byte, error) {
	ctx := context.Background()
	args := make([]uint64, 0, 2*len(segs))
	ptrs := make([][2]uint64, 0, len(segs))
	release := func() {
		for _, pr := range ptrs {
			in.freep(ctx, pr[0], pr[1])
		}
	}
	for _, seg := range segs {
		p, l, err := in.put(ctx, seg)
		if err != nil {
			release()
			return nil, err
		}
		ptrs = append(ptrs, [2]uint64{p, l})
		args = append(args, p, l)
	}
	defer release()
	r, err := fn.Call(ctx, args...)
	if err != nil {
		return nil, err
	}
	out, ok := in.readRet(r[0])
	if !ok {
		return nil, fmt.Errorf("%s returned null (bad input / core unavailable)", what)
	}
	return out, nil
}

// Sign runs cc_sign(seed, canonical, meta, body) -> ed25519 signature bytes.
func Sign(seed []byte, canonical, meta string, body []byte) ([]byte, error) {
	if err := ensure(); err != nil {
		return nil, err
	}
	in := acquire()
	defer in.release()
	return in.call(in.sign, [][]byte{seed, []byte(canonical), []byte(meta), body}, "cc_sign")
}

// Seal runs cc_seal(sealPub, ephSeed, nonce, plaintext, aad) -> sealed blob.
func Seal(sealPub, ephSeed, nonce, plaintext, aad []byte) ([]byte, error) {
	if err := ensure(); err != nil {
		return nil, err
	}
	in := acquire()
	defer in.release()
	return in.call(in.seal, [][]byte{sealPub, ephSeed, nonce, plaintext, aad}, "cc_seal")
}
