package mirasim

// Differential test: the mirasim header set this package produces must be BYTE
// IDENTICAL to what ma-relay's production implementation produces for the same
// inputs. A wrong signature is a 403 on every request, so "it looks right" is
// not an acceptable standard here.
//
// How the reference side is executed, and what that does and does not cover:
//
//   COVERED BY EXECUTED ma-relay CODE (byte-identical assertions):
//     - device id and SPKI public key derivation from the device seed
//     - x-mirasim-device / x-mirasim-ts / x-mirasim-nonce / x-mirasim-client
//     - x-mirasim-sig — i.e. the canonical string assembly (field order, the
//       \x00 join, the uppercased method, the path-without-query rule, the
//       client version, the credential) AND the ccore/WASM invocation AND the
//       body hashing that happens inside the core.
//     - ccore.Seal's output for a fixed (sealPub, ephSeed, nonce, plaintext,
//       aad): the port's sealed blob must equal ma-relay's byte for byte.
//     - the SET OF HEADERS that get sealed. ma-relay's collector is unexported,
//       but SealHeaders deletes exactly what it sealed, so diffing the header
//       map across the call observes the real selection rule.
//
//   NOT COVERED BY EXECUTED ma-relay CODE (and why):
//     - metaFromHeaders' SERIALISATION (the sorted k\0v\0k\0v join). meta is
//       computed inside ma-relay's unexported metaFromHeaders and is consumed
//       only by SignAndSeal, whose signature output is sealed away before it can
//       be read back. Its key-SELECTION rule is covered (see above, it is the
//       sealed set minus the four signature headers); only the join format is
//       not. Asserted here against a verbatim transcription instead.
//     - the seal AAD string and the fixed relay seal public key. Both are inputs
//       to an AEAD we cannot decrypt (we do not hold the relay private key) and
//       neither is observable through an exported ma-relay symbol. Asserted here
//       against a verbatim transcription.
//   Both uncovered items are load-bearing for the upstream to accept a request,
//   so the live end-to-end call (HTTP 200 with content) is their real oracle: a
//   wrong meta produces a signature mismatch and a wrong AAD/seal key produces
//   an unopenable seal, and either is a hard upstream rejection, never a silent
//   partial success.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// defaultMaRelaySrc is the read-only ma-relay checkout the reference harness is
// built against. Override with MIRASIM_MARELAY_SRC. The harness NEVER writes
// into this tree.
const defaultMaRelaySrc = "/Users/snakesammy/.codex/visualizations/2026/09/08/01a0811c-8ca5-7ec1-be88-d5e5bf51ca26/mira2api-frontend-01/checks/scan-layout-01/source"

const vectorsPath = "testdata/marelay_vectors.json"

type diffJob struct {
	SeedB64    string `json:"seed_b64"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	BodyB64    string `json:"body_b64"`
	Credential string `json:"credential"`
	Meta       string `json:"meta"`
	TSMillis   int64  `json:"ts_millis"`

	SealPubB64   string `json:"seal_pub_b64"`
	EphB64       string `json:"eph_b64"`
	SealNonceB64 string `json:"seal_nonce_b64"`
	PlaintextB64 string `json:"plaintext_b64"`
	AADB64       string `json:"aad_b64"`

	Headers map[string]string `json:"headers"`
}

type diffResult struct {
	DeviceID     string            `json:"device_id"`
	PublicKeyB64 string            `json:"public_key_b64"`
	SigHeaders   map[string]string `json:"sig_headers"`
	SealBlobB64  string            `json:"seal_blob_b64"`
	SealedNames  []string          `json:"sealed_names"`
	SurvivingEnc string            `json:"surviving_enc"`
	Err          string            `json:"err,omitempty"`
}

type vectorCase struct {
	Name   string     `json:"name"`
	Job    diffJob    `json:"job"`
	Result diffResult `json:"result"`
}

type vectorFile struct {
	Note          string       `json:"note"`
	ClientVersion string       `json:"client_version"`
	Cases         []vectorCase `json:"cases"`
}

// testSeed is a throwaway ed25519 device seed used only by these vectors. It is
// NOT a real account's seed.
const testSeed = "dGVzdC1taXJhc2ltLWRldmljZS1zZWVkLTMyYnl0ZXM"

func diffJobs(t *testing.T) []vectorCase {
	t.Helper()

	// Realistic claude-lane context header set, as ApplyContextHeaders leaves it
	// just before signing.
	ctxHeaders := map[string]string{
		"x-mirasim-session": "session_0f1e2d3c4b5a69788796a5b4",
		"x-mirasim-agent":   "claude",
		"x-mirasim-call":    "call_a1b2c3d4e5f60718293a4b5c",
		"x-mirasim-locale":  "en-US",
		"x-mirasim-account": "usr_diffvectorsubject",
		"x-mirasim-client":  ClientVersion,
		"content-type":      "application/json",
		"user-agent":        "claude-cli/2.1.272 (external, sdk-cli)",
	}
	ctxMeta := metaFromHeaders(headerFromMap(ctxHeaders))

	probeHeaders := map[string]string{
		"x-mirasim-session": "session_ffffffffffffffffffffffff",
		"x-mirasim-agent":   "claude",
		"x-mirasim-call":    "call_000000000000000000000000",
		"x-mirasim-locale":  "ja-JP",
		"x-mirasim-probe":   "usage",
		"x-mirasim-client":  ClientVersion,
	}

	body := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`)

	// Deterministic seal probe inputs.
	eph := bytes.Repeat([]byte{0x11}, sealEphSize)
	sealNonce := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	plaintext, _ := json.Marshal(map[string]string{
		"x-mirasim-agent":  "claude",
		"x-mirasim-device": "AAAAAAAAAAAAAAAAAAAAAA",
		"x-mirasim-nonce":  "bm9uY2U",
		"x-mirasim-sig":    "c2ln",
		"x-mirasim-ts":     "1786250000123",
	})
	aad := sealAAD("post", "/v1/messages")

	return []vectorCase{
		{
			Name: "messages_with_context_meta",
			Job: diffJob{
				SeedB64:      testSeed,
				Method:       "post",
				Path:         "/v1/messages",
				BodyB64:      base64.StdEncoding.EncodeToString(body),
				Credential:   "tkt_differential_test_credential",
				Meta:         ctxMeta,
				TSMillis:     1786250000123,
				SealPubB64:   base64.StdEncoding.EncodeToString(sealPubKey),
				EphB64:       base64.StdEncoding.EncodeToString(eph),
				SealNonceB64: base64.StdEncoding.EncodeToString(sealNonce),
				PlaintextB64: base64.StdEncoding.EncodeToString(plaintext),
				AADB64:       base64.StdEncoding.EncodeToString(aad),
				Headers:      ctxHeaders,
			},
		},
		{
			Name: "device_session_mint_empty_meta",
			Job: diffJob{
				SeedB64:    testSeed,
				Method:     "POST",
				Path:       "/v1/device/session",
				BodyB64:    base64.StdEncoding.EncodeToString([]byte(`{"publicKey":"pk","deviceId":"did"}`)),
				Credential: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c3JfdGVzdCJ9.sig",
				Meta:       "",
				TSMillis:   1786250111222,
			},
		},
		{
			Name: "limits_probe_empty_body",
			Job: diffJob{
				SeedB64:    testSeed,
				Method:     "get",
				Path:       "/v1/limits",
				BodyB64:    "",
				Credential: "tkt_probe",
				Meta:       metaFromHeaders(headerFromMap(probeHeaders)),
				TSMillis:   1786250222333,
				Headers:    probeHeaders,
			},
		},
	}
}

func headerFromMap(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

// TestDifferentialAgainstMaRelay is the acceptance gate. It asserts the port's
// output against vectors produced by ma-relay's real implementation. When the
// ma-relay source tree is reachable it regenerates those vectors first, so the
// check is a live differential rather than a frozen snapshot.
func TestDifferentialAgainstMaRelay(t *testing.T) {
	cases := diffJobs(t)

	// Committed vectors: asserted always, so the gate still runs where the
	// ma-relay tree is absent (CI).
	var suites []vectorFile
	raw, err := os.ReadFile(vectorsPath)
	if err == nil {
		var vf vectorFile
		if err := json.Unmarshal(raw, &vf); err != nil {
			t.Fatal(err)
		}
		if vf.ClientVersion != ClientVersion {
			t.Fatalf("vectors were generated for client version %q but this port signs as %q; delete %s and re-run with the ma-relay source present", vf.ClientVersion, ClientVersion, vectorsPath)
		}
		if len(vf.Cases) != len(cases) {
			t.Fatalf("vector file has %d cases, test defines %d", len(vf.Cases), len(cases))
		}
		suites = append(suites, vf)
	}

	// Live differential: freshly executed ma-relay code, compared in memory.
	// ma-relay draws a new nonce per call, so every generation produces different
	// (but equally valid) vectors — writing them back on every run would make the
	// committed file churn for no information, so it is only written when absent
	// or stale.
	fresh, live := regenerateVectors(t, cases)
	if live {
		suites = append(suites, fresh)
		if err != nil {
			writeVectors(t, fresh)
			t.Logf("wrote missing %s", vectorsPath)
		}
	}
	if len(suites) == 0 {
		t.Fatalf("no vectors: %s is unreadable (%v) and the ma-relay source is not available to regenerate it", vectorsPath, err)
	}

	// [[cov:SG:bytewise-parity]] For a fixed (method, path, body, deviceSeed, ts,
	// nonce, credential, clientVersion), the five x-mirasim-* headers produced by
	// DeviceSigner.headersWithNonce must equal ma-relay's byte for byte — the
	// signature included, so this covers the canonical string assembly and the
	// ccore/WASM call, not just the plaintext fields.
	for _, vf := range suites {
		for _, vc := range vf.Cases {
			t.Run(vc.Name, func(t *testing.T) {
				if vc.Result.Err != "" {
					t.Fatalf("reference harness errored: %s", vc.Result.Err)
				}
				signer, err := NewDeviceSigner(vc.Job.SeedB64)
				if err != nil {
					t.Fatal(err)
				}

				// --- identity ---
				if signer.DeviceID != vc.Result.DeviceID {
					t.Errorf("device id: port=%q ma-relay=%q", signer.DeviceID, vc.Result.DeviceID)
				}
				if signer.PublicKeyB64 != vc.Result.PublicKeyB64 {
					t.Errorf("public key: port=%q ma-relay=%q", signer.PublicKeyB64, vc.Result.PublicKeyB64)
				}

				// --- signature headers, byte for byte ---
				body, err := base64.StdEncoding.DecodeString(vc.Job.BodyB64)
				if err != nil {
					t.Fatal(err)
				}
				if vc.Job.BodyB64 == "" {
					body = nil
				}
				nonce, err := base64.RawURLEncoding.DecodeString(vc.Result.SigHeaders["x-mirasim-nonce"])
				if err != nil {
					t.Fatal(err)
				}
				got, err := signer.headersWithNonce(vc.Job.Method, vc.Job.Path, body, vc.Job.Credential, vc.Job.Meta, time.UnixMilli(vc.Job.TSMillis), nonce)
				if err != nil {
					t.Fatal(err)
				}
				for _, k := range []string{"x-mirasim-device", "x-mirasim-ts", "x-mirasim-nonce", "x-mirasim-sig", "x-mirasim-client"} {
					if got[k] != vc.Result.SigHeaders[k] {
						t.Errorf("%s: port=%q ma-relay=%q", k, got[k], vc.Result.SigHeaders[k])
					}
				}
				if len(got) != len(vc.Result.SigHeaders) {
					t.Errorf("header count: port=%d ma-relay=%d", len(got), len(vc.Result.SigHeaders))
				}

				// --- deterministic seal blob, byte for byte ---
				if vc.Job.SealPubB64 != "" {
					if base64.StdEncoding.EncodeToString(sealPubKey) != vc.Job.SealPubB64 {
						t.Fatalf("port seal public key differs from the one handed to ma-relay")
					}
					h := headerFromMap(map[string]string{})
					_ = h
					eph, _ := base64.StdEncoding.DecodeString(vc.Job.EphB64)
					sn, _ := base64.StdEncoding.DecodeString(vc.Job.SealNonceB64)
					pt, _ := base64.StdEncoding.DecodeString(vc.Job.PlaintextB64)
					aad, _ := base64.StdEncoding.DecodeString(vc.Job.AADB64)
					blob, err := sealBlobForTest(pt, aad, eph, sn)
					if err != nil {
						t.Fatal(err)
					}
					if enc := base64.RawURLEncoding.EncodeToString(blob); enc != vc.Result.SealBlobB64 {
						t.Errorf("seal blob: port=%q ma-relay=%q", enc, vc.Result.SealBlobB64)
					}
				}

				// --- sealed header selection rule ---
				if len(vc.Job.Headers) > 0 {
					plain, _ := sealablePlaintext(headerFromMap(vc.Job.Headers))
					portNames := make([]string, 0, len(plain))
					for k := range plain {
						portNames = append(portNames, k)
					}
					sort.Strings(portNames)
					refNames := append([]string(nil), vc.Result.SealedNames...)
					sort.Strings(refNames)
					if strings.Join(portNames, ",") != strings.Join(refNames, ",") {
						t.Errorf("sealed header set: port=%v ma-relay=%v", portNames, refNames)
					}
				}
			})
		}
	}
}

// sealBlobForTest exercises the port's own seal path (sealHeadersWith) and
// returns the raw blob, so the comparison covers the real code rather than a
// direct ccore.Seal call written for the test.
func sealBlobForTest(plaintext, aad, eph, nonce []byte) ([]byte, error) {
	// Rebuild the header set the plaintext was marshalled from, so
	// sealHeadersWith re-marshals it identically (json.Marshal sorts keys, and
	// the reference plaintext was produced the same way).
	var m map[string]string
	if err := json.Unmarshal(plaintext, &m); err != nil {
		return nil, err
	}
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	// AAD is "mrs-seal-v1\nMETHOD\npath": recover method and path from it so the
	// port derives the AAD itself instead of being handed one.
	parts := strings.SplitN(string(aad), "\n", 3)
	if len(parts) != 3 || parts[0] != sealSchemeLine {
		return nil, errBadAAD
	}
	if err := sealHeadersWith(h, parts[1], parts[2], eph, nonce); err != nil {
		return nil, err
	}
	return base64.RawURLEncoding.DecodeString(h.Get(sealHeaderName))
}

var errBadAAD = errBadAADType{}

type errBadAADType struct{}

func (errBadAADType) Error() string { return "aad does not match the mrs-seal-v1 scheme" }

// regenerateVectors runs the reference harness against the read-only ma-relay
// tree. Returns ok=false (with a t.Log, not a failure) when that tree is not
// present, in which case the committed vectors — themselves generated by
// ma-relay — are still asserted.
func regenerateVectors(t *testing.T, cases []vectorCase) (vectorFile, bool) {
	t.Helper()
	src := os.Getenv("MIRASIM_MARELAY_SRC")
	if src == "" {
		src = defaultMaRelaySrc
	}
	if _, err := os.Stat(filepath.Join(src, "internal", "relay", "seal.go")); err != nil {
		t.Logf("ma-relay source not available at %s (%v): asserting against the committed vectors only", src, err)
		return vectorFile{}, false
	}

	dir := t.TempDir()
	mainSrc, err := os.ReadFile("testdata/marelay_harness/main.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	modSrc, err := os.ReadFile("testdata/marelay_harness/go.mod.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), mainSrc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), bytes.ReplaceAll(modSrc, []byte("MARELAY_SRC"), []byte(src)), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "marelaysig")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Logf("building the ma-relay reference harness failed (%v):\n%s\nasserting against the committed vectors only", err, out)
		return vectorFile{}, false
	}

	out := vectorFile{
		Note:          "GENERATED by executing ma-relay's own relay.DeviceSigner.Headers / relay.SealHeaders / ccore.Seal. Do not hand-edit.",
		ClientVersion: ClientVersion,
	}
	for _, c := range cases {
		in, err := json.Marshal(c.Job)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin)
		cmd.Stdin = bytes.NewReader(in)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("reference harness failed for case %s: %v", c.Name, err)
		}
		var res diffResult
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			t.Fatalf("decode reference output for case %s: %v", c.Name, err)
		}
		if res.Err != "" {
			t.Fatalf("reference harness errored for case %s: %s", c.Name, res.Err)
		}
		out.Cases = append(out.Cases, vectorCase{Name: c.Name, Job: c.Job, Result: res})
	}
	t.Logf("regenerated %d differential vectors from ma-relay at %s", len(out.Cases), src)
	return out, true
}

func writeVectors(t *testing.T, vf vectorFile) {
	t.Helper()
	raw, err := json.MarshalIndent(vf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vectorsPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMetaSerialisationMatchesTranscription pins the one derivation the live
// differential cannot reach (see the file header): ma-relay's metaFromHeaders
// join format. The expectation below is a hand transcription of ma-relay's
// serialisation, so this catches a transcription slip in the port, not a
// divergence from executed reference code.
func TestMetaSerialisationMatchesTranscription(t *testing.T) {
	h := headerFromMap(map[string]string{
		"x-mirasim-session": "s1",
		"x-mirasim-agent":   "claude",
		"x-mirasim-call":    "c1",
		"x-mirasim-locale":  "en-US",
		"x-mirasim-account": "usr_1",
		"x-mirasim-empty":   "",
		// excluded: signature headers, client, enc
		"x-mirasim-device": "d",
		"x-mirasim-ts":     "1",
		"x-mirasim-nonce":  "n",
		"x-mirasim-sig":    "g",
		"x-mirasim-client": ClientVersion,
		"x-mirasim-enc":    "e",
		"content-type":     "application/json",
	})
	want := strings.Join([]string{
		"x-mirasim-account", "usr_1",
		"x-mirasim-agent", "claude",
		"x-mirasim-call", "c1",
		"x-mirasim-locale", "en-US",
		"x-mirasim-session", "s1",
	}, "\x00")
	if got := metaFromHeaders(h); got != want {
		t.Fatalf("meta:\n got %q\nwant %q", got, want)
	}
}

// TestSealAADMatchesTranscription pins the other uncovered derivation.
func TestSealAADMatchesTranscription(t *testing.T) {
	if got, want := string(sealAAD("post", "/v1/messages")), "mrs-seal-v1\nPOST\n/v1/messages"; got != want {
		t.Fatalf("aad = %q want %q", got, want)
	}
	if sealPubKeyB64 != "HlyNMMeGXryasYLJuYQ/9ksCD4AYVVy1zXKAtJdpJn4=" {
		t.Fatalf("seal public key drifted from the upstream constant")
	}
}
