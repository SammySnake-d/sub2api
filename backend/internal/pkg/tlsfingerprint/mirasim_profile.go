package tlsfingerprint

// MirasimProfile returns the ClientHello of Mirasim.app's bundled
// Electron 33.4.11 / Node 20.18.3 (BoringSSL) https.request, expressed in this
// package's declarative profile model.
//
// Captured by running the app's own runtime against a JA3 echo:
//
//	JA3  = 771,4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-13-51-45-43,29-23-24,0
//	hash = 71dc8c533dd919ae9f4963224a4ba8fd
//
// This is deliberately NOT the package default (Claude Code / Node.js 24.x,
// JA3 44f88fca027f27bab4bb08d4af15f23e). A mirasim account is an Electron app,
// not the Claude Code CLI; presenting the CLI's TLS fingerprint underneath a
// mirasim device signature is a self-inconsistent combination that is easier to
// spot than either half alone. The TLS handshake is the first packet, so the
// upstream classifies the client before it reads a single HTTP header.
//
// Differences from the package defaults, all load-bearing for the JA3 hash:
//
//   - 18 cipher suites, not the default 17, and in a different order: Electron's
//     BoringSSL emits ECDHE_RSA before ECDHE_ECDSA at each strength, and adds
//     0xc027 (ECDHE_RSA_AES_128_CBC_SHA256), which Node 24.x drops.
//   - 10 extensions, not the default 14: no ALPN (16), no encrypted_client_hello
//     (65037), no status_request (5), no signed_certificate_timestamp (18).
//   - ALPN absent means no protocol is negotiated and the connection falls back
//     to HTTP/1.1. Callers must therefore keep ForceAttemptHTTP2=false on the
//     transport; advertising h2 here would both change the JA3 and contradict
//     the real client's observed HTTP/1.1 traffic.
//
// A fresh value is returned on every call so callers cannot mutate shared state.
func MirasimProfile() *Profile {
	return &Profile{
		Name: "mirasim-electron-33",
		// Order is critical for JA3: the hash is the ordered join of these values.
		CipherSuites: []uint16{
			0x1301, // TLS_AES_128_GCM_SHA256
			0x1302, // TLS_AES_256_GCM_SHA384
			0x1303, // TLS_CHACHA20_POLY1305_SHA256
			0xc02f, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
			0xc02b, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
			0xc030, // TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
			0xc02c, // TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
			0xc027, // TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256
			0xcca9, // TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256
			0xcca8, // TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
			0xc009, // TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA
			0xc013, // TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA
			0xc00a, // TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA
			0xc014, // TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA
			0x009c, // TLS_RSA_WITH_AES_128_GCM_SHA256
			0x009d, // TLS_RSA_WITH_AES_256_GCM_SHA384
			0x002f, // TLS_RSA_WITH_AES_128_CBC_SHA
			0x0035, // TLS_RSA_WITH_AES_256_CBC_SHA
		},
		Curves: []uint16{
			29, // x25519
			23, // secp256r1 (P-256)
			24, // secp384r1 (P-384)
		},
		PointFormats: []uint16{
			0, // uncompressed
		},
		EnableGREASE: false, // BoringSSL here sends no GREASE values
		SignatureAlgorithms: []uint16{
			0x0403, // ecdsa_secp256r1_sha256
			0x0804, // rsa_pss_rsae_sha256
			0x0401, // rsa_pkcs1_sha256
			0x0503, // ecdsa_secp384r1_sha384
			0x0805, // rsa_pss_rsae_sha384
			0x0501, // rsa_pkcs1_sha384
			0x0806, // rsa_pss_rsae_sha512
			0x0601, // rsa_pkcs1_sha512
			0x0201, // rsa_pkcs1_sha1
		},
		ALPNProtocols:     nil, // no ALPN extension is emitted; see the note above
		SupportedVersions: []uint16{0x0304, 0x0303},
		KeyShareGroups:    []uint16{29}, // x25519 only
		PSKModes:          []uint16{1},  // psk_dhe_ke
		// Extension type IDs in wire order. ALPN (16) is deliberately absent.
		Extensions: []uint16{
			0,     // server_name
			23,    // extended_master_secret
			65281, // renegotiation_info
			10,    // supported_groups
			11,    // ec_point_formats
			35,    // session_ticket
			13,    // signature_algorithms
			51,    // key_share
			45,    // psk_key_exchange_modes
			43,    // supported_versions
		},
	}
}
