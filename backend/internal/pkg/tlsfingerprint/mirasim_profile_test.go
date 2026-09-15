// Package tlsfingerprint — JA3 conformance test for the Mirasim.app profile.
//
// This file has no build tag on purpose: it needs no network and no external
// service, so `go test ./internal/pkg/tlsfingerprint/...` runs it. The
// fingerprint is read off the wire — a loopback listener captures the real
// ClientHello that Dialer.DialTLSContext emits — rather than being recomputed
// from the Profile struct. A struct-level check would pass even if
// buildClientHelloSpecFromProfile dropped or reordered an extension on the way
// to the socket, which is exactly the failure that matters here.
package tlsfingerprint

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// mirasimJA3 is the fingerprint captured from Mirasim.app's bundled
	// Electron 33.4.11 / Node 20.18.3 (BoringSSL) https.request.
	mirasimJA3 = "771,4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-13-51-45-43,29-23-24,0"
	// mirasimJA3Hash is the MD5 of mirasimJA3.
	mirasimJA3Hash = "71dc8c533dd919ae9f4963224a4ba8fd"
	// claudeCodeJA3Hash is this package's DEFAULT profile (Claude Code /
	// Node.js 24.x). The mirasim profile must not collapse onto it.
	claudeCodeJA3Hash = "44f88fca027f27bab4bb08d4af15f23e"

	// mirasimTestSNIHost is dialed instead of the listener's literal IP so the
	// SNI extension is actually emitted: utls omits server_name entirely for IP
	// literals (RFC 6066), which would silently drop extension 0 from the JA3.
	mirasimTestSNIHost = "mirasim.example.invalid"
)

// TestMirasimProfileJA3 asserts that MirasimProfile, driven through the real
// dial path, puts Mirasim.app's ClientHello on the wire — and that it is not
// the Claude Code fingerprint this package defaults to.
func TestMirasimProfileJA3(t *testing.T) {
	// [[cov:ID:tls-ja3]]
	profile := MirasimProfile()
	if profile.Name != "mirasim-electron-33" {
		t.Fatalf("unexpected profile name %q", profile.Name)
	}

	hello := captureClientHello(t, profile)
	gotJA3, err := ja3FromClientHello(hello)
	if err != nil {
		t.Fatalf("parse captured ClientHello: %v", err)
	}
	gotHash := md5Hex(gotJA3)

	// Primary: the wire fingerprint is Mirasim.app's, field for field.
	if gotJA3 != mirasimJA3 {
		t.Errorf("JA3 string mismatch (spec was ported wrong, do NOT adjust the expectation)\n want: %s\n  got: %s\n%s",
			mirasimJA3, gotJA3, describeJA3Diff(mirasimJA3, gotJA3))
	}
	if gotHash != mirasimJA3Hash {
		t.Errorf("JA3 hash mismatch: want %s, got %s (from %s)", mirasimJA3Hash, gotHash, gotJA3)
	}

	// Counter-example: a test that only asserts equality with the expected value
	// stays green if someone wires the Claude Code profile in under the mirasim
	// name. Pin the negative explicitly.
	if gotHash == claudeCodeJA3Hash {
		t.Errorf("mirasim profile emitted the Claude Code fingerprint %s; the upstream would see the CLI, not the app", claudeCodeJA3Hash)
	}

	// Same negative, measured rather than hardcoded: run the package DEFAULT
	// profile through the identical path and require the two to diverge.
	defaultHello := captureClientHello(t, &Profile{Name: "default-claude-code"})
	defaultJA3, err := ja3FromClientHello(defaultHello)
	if err != nil {
		t.Fatalf("parse default-profile ClientHello: %v", err)
	}
	if defaultHash := md5Hex(defaultJA3); defaultHash == gotHash {
		t.Errorf("default profile and mirasim profile produced the same JA3 %s; the mirasim spec is not being applied", defaultHash)
	} else {
		t.Logf("default profile JA3 = %s (%s)", defaultJA3, defaultHash)
	}

	t.Logf("mirasim profile JA3 = %s (%s)", gotJA3, gotHash)
}

// captureClientHello dials a loopback listener through Dialer.DialTLSContext
// and returns the raw ClientHello handshake message (record header stripped).
// The handshake necessarily fails — the listener never answers — which is fine:
// the first flight is the entire subject of a JA3.
func captureClientHello(t *testing.T, profile *Profile) []byte {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type captured struct {
		data []byte
		err  error
	}
	done := make(chan captured, 1)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- captured{nil, fmt.Errorf("accept: %w", err)}
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

		// TLS record header: type(1) version(2) length(2).
		var hdr [5]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			done <- captured{nil, fmt.Errorf("read record header: %w", err)}
			return
		}
		if hdr[0] != 0x16 {
			done <- captured{nil, fmt.Errorf("first record is type %d, want 22 (handshake)", hdr[0])}
			return
		}
		body := make([]byte, int(hdr[3])<<8|int(hdr[4]))
		if _, err := io.ReadFull(conn, body); err != nil {
			done <- captured{nil, fmt.Errorf("read record body: %w", err)}
			return
		}
		done <- captured{body, nil}
	}()

	// Route the fake SNI hostname to the listener without touching DNS.
	baseDialer := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := NewDialer(profile, baseDialer).DialTLSContext(ctx, "tcp", net.JoinHostPort(mirasimTestSNIHost, "443"))
	if err == nil {
		// The listener never completes a handshake, so this should not happen;
		// close defensively rather than leak the connection.
		_ = conn.Close()
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("capture ClientHello: %v", res.err)
		}
		return res.data
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ClientHello")
		return nil
	}
}

// ja3FromClientHello builds the JA3 string from a raw ClientHello handshake
// message: TLSVersion,Ciphers,Extensions,EllipticCurves,ECPointFormats — values
// joined by "-" within a field and fields joined by ",". GREASE values are
// excluded from ciphers, extensions and curves per the JA3 definition.
func ja3FromClientHello(msg []byte) (string, error) {
	c := &ja3Cursor{buf: msg}

	if typ := c.u8(); typ != 0x01 {
		return "", fmt.Errorf("handshake type %d, want 1 (client_hello)", typ)
	}
	bodyLen := int(c.u8())<<16 | int(c.u8())<<8 | int(c.u8())
	body := c.take(bodyLen)
	if c.err != nil {
		return "", c.err
	}
	c = &ja3Cursor{buf: body}

	version := c.u16() // legacy_version
	c.take(32)         // random
	c.take(int(c.u8()))
	ciphers := c.take(int(c.u16()))
	c.take(int(c.u8())) // compression methods
	extBlock := c.take(int(c.u16()))
	if c.err != nil {
		return "", c.err
	}

	var extIDs, curves, pointFormats []uint16
	e := &ja3Cursor{buf: extBlock}
	for e.remaining() > 0 {
		id := e.u16()
		data := e.take(int(e.u16()))
		if e.err != nil {
			return "", e.err
		}
		if !isGREASEValue(id) {
			extIDs = append(extIDs, id)
		}
		switch id {
		case 10: // supported_groups
			g := &ja3Cursor{buf: data}
			list := g.take(int(g.u16()))
			if g.err != nil {
				return "", g.err
			}
			for _, v := range readU16s(list) {
				if !isGREASEValue(v) {
					curves = append(curves, v)
				}
			}
		case 11: // ec_point_formats
			p := &ja3Cursor{buf: data}
			list := p.take(int(p.u8()))
			if p.err != nil {
				return "", p.err
			}
			for _, v := range list {
				pointFormats = append(pointFormats, uint16(v))
			}
		}
	}

	cipherIDs := make([]uint16, 0, len(ciphers)/2)
	for _, v := range readU16s(ciphers) {
		if !isGREASEValue(v) {
			cipherIDs = append(cipherIDs, v)
		}
	}

	return strings.Join([]string{
		strconv.Itoa(int(version)),
		joinU16s(cipherIDs),
		joinU16s(extIDs),
		joinU16s(curves),
		joinU16s(pointFormats),
	}, ","), nil
}

// ja3Cursor is a bounds-checked big-endian reader. Once it overruns it latches
// the error and returns zero values, so parsing never panics on a short buffer.
type ja3Cursor struct {
	buf []byte
	err error
}

func (c *ja3Cursor) remaining() int { return len(c.buf) }

func (c *ja3Cursor) take(n int) []byte {
	if c.err != nil {
		return nil
	}
	if n < 0 || n > len(c.buf) {
		c.err = errors.New("truncated ClientHello")
		c.buf = nil
		return nil
	}
	out := c.buf[:n]
	c.buf = c.buf[n:]
	return out
}

func (c *ja3Cursor) u8() uint8 {
	b := c.take(1)
	if len(b) != 1 {
		return 0
	}
	return b[0]
}

func (c *ja3Cursor) u16() uint16 {
	b := c.take(2)
	if len(b) != 2 {
		return 0
	}
	return uint16(b[0])<<8 | uint16(b[1])
}

func readU16s(b []byte) []uint16 {
	out := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		out = append(out, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return out
}

func joinU16s(vals []uint16) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = strconv.Itoa(int(v))
	}
	return strings.Join(parts, "-")
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s)) // #nosec G401 — JA3 is defined as MD5, not a security primitive
	return hex.EncodeToString(sum[:])
}

// describeJA3Diff names the first JA3 field that differs, so a failure says
// "extensions" instead of leaving two long strings to be eyeballed.
func describeJA3Diff(want, got string) string {
	names := []string{"version", "ciphers", "extensions", "curves", "point_formats"}
	w, g := strings.Split(want, ","), strings.Split(got, ",")
	var b strings.Builder
	for i := range names {
		var wf, gf string
		if i < len(w) {
			wf = w[i]
		}
		if i < len(g) {
			gf = g[i]
		}
		if wf != gf {
			fmt.Fprintf(&b, " first differing field: %s\n   want: %s\n    got: %s\n", names[i], wf, gf)
			return b.String()
		}
	}
	return " fields match pairwise; difference is in field count\n"
}
