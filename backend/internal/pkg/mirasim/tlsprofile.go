package mirasim

import "github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"

// TLSProfile is the ClientHello of Mirasim.app's bundled Electron 33.4.11 /
// Node 20.18.3 (BoringSSL) https.request, expressed in sub2api's declarative
// profile model. It reproduces ma-relay's nodeClientHelloSpec()
// (internal/relay/dialer.go), which was captured by running the app's own
// runtime against a JA3 echo:
//
//	JA3  = 771,4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-13-51-45-43,29-23-24,0
//	hash = 71dc8c533dd919ae9f4963224a4ba8fd
//
// This is NOT sub2api's built-in Claude Code profile (JA3
// 44f88fca027f27bab4bb08d4af15f23e): a mirasim account is an Electron app, not
// the Claude Code CLI, and presenting the CLI's TLS fingerprint under a mirasim
// device signature is exactly the kind of self-inconsistent combination that is
// easier to spot than either piece alone.
//
// The ALPN extension (16) is ABSENT from the extension list on purpose — the
// real client sends no ALPN, so the connection negotiates HTTP/1.1. sub2api's
// TLS-fingerprint transport already sets ForceAttemptHTTP2=false whenever a
// profile is in play, which matches ma-relay.
var TLSProfile = &tlsfingerprint.Profile{
	Name: "mirasim-electron-33",
	CipherSuites: []uint16{
		0x1301, // TLS_AES_128_GCM_SHA256
		0x1302, // TLS_AES_256_GCM_SHA384
		0x1303, // TLS_CHACHA20_POLY1305_SHA256
		0xc02f, // ECDHE_RSA_AES_128_GCM_SHA256
		0xc02b, // ECDHE_ECDSA_AES_128_GCM_SHA256
		0xc030, // ECDHE_RSA_AES_256_GCM_SHA384
		0xc02c, // ECDHE_ECDSA_AES_256_GCM_SHA384
		0xc027, // ECDHE_RSA_AES_128_CBC_SHA256
		0xcca9, // ECDHE_ECDSA_CHACHA20_POLY1305
		0xcca8, // ECDHE_RSA_CHACHA20_POLY1305
		0xc009, // ECDHE_ECDSA_AES_128_CBC_SHA
		0xc013, // ECDHE_RSA_AES_128_CBC_SHA
		0xc00a, // ECDHE_ECDSA_AES_256_CBC_SHA
		0xc014, // ECDHE_RSA_AES_256_CBC_SHA
		0x009c, // RSA_AES_128_GCM_SHA256
		0x009d, // RSA_AES_256_GCM_SHA384
		0x002f, // RSA_AES_128_CBC_SHA
		0x0035, // RSA_AES_256_CBC_SHA
	},
	// X25519, P-256, P-384.
	Curves:       []uint16{29, 23, 24},
	PointFormats: []uint16{0},
	EnableGREASE: false,
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
	ALPNProtocols:     nil, // no ALPN extension is emitted; see above
	SupportedVersions: []uint16{0x0304, 0x0303},
	KeyShareGroups:    []uint16{29}, // X25519
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
