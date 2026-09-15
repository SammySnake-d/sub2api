package admin

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// mirasimImportTestSeed builds an obviously-fake but structurally valid device
// seed: a 32-byte ed25519 seed whose bytes spell the label. Nothing here is a
// real credential, and no test in this file ever prints a seed.
func mirasimImportTestSeed(t *testing.T, label string) string {
	t.Helper()
	if len(label) > ed25519.SeedSize {
		t.Fatalf("test seed label is %d bytes, max %d", len(label), ed25519.SeedSize)
	}
	raw := make([]byte, ed25519.SeedSize)
	copy(raw, label)
	return base64.RawStdEncoding.EncodeToString(raw)
}

// mirasimImportTestBrokenSeed models a seed damaged in transit: the base64
// decodes, but to the wrong number of bytes, so no ed25519 key exists for it.
func mirasimImportTestBrokenSeed(t *testing.T) string {
	t.Helper()
	raw := make([]byte, ed25519.SeedSize-1)
	copy(raw, "test-seed-truncated")
	return base64.RawStdEncoding.EncodeToString(raw)
}

func mirasimImportTestEntryJSON(t *testing.T, name, seed string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"name":                name,
		"mirasim_device_seed": seed,
		"mirasim_session_id":  "session_" + name,
		"access_token":        "test-access-token-" + name,
		"refresh_token":       "test-refresh-token-" + name,
		"expires_at":          "2030-01-02T03:04:05Z",
	})
	if err != nil {
		t.Fatalf("marshal mirasim import entry: %v", err)
	}
	return string(payload)
}

func mirasimImportTestHandler(svc service.AdminService) *AccountHandler {
	return NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
}

func mirasimImportTestRequest() MirasimAccountImportRequest {
	return MirasimAccountImportRequest{SkipDefaultGroupBind: boolPtr(true)}
}

// TestImportMirasimAccountsVerifiesEverySeedAndFailsTheBadOne is the core gate:
// a batch where three seeds are usable and one is damaged must report exactly
// three verified imports, and the damaged one must land in Failed — never in
// Created with a silent pass.
func TestImportMirasimAccountsVerifiesEverySeedAndFailsTheBadOne(t *testing.T) {
	// [[cov:MG:per-account-signature]]
	goodSeeds := []string{
		mirasimImportTestSeed(t, "test-seed-0001"),
		mirasimImportTestSeed(t, "test-seed-0002"),
		mirasimImportTestSeed(t, "test-seed-0003"),
	}
	brokenSeed := mirasimImportTestBrokenSeed(t)

	svc := newCodexImportMemoryAdminService(nil)
	handler := mirasimImportTestHandler(svc)
	req := mirasimImportTestRequest()
	req.Content = strings.Join([]string{
		mirasimImportTestEntryJSON(t, "alpha", goodSeeds[0]),
		mirasimImportTestEntryJSON(t, "beta", goodSeeds[1]),
		mirasimImportTestEntryJSON(t, "broken", brokenSeed),
		mirasimImportTestEntryJSON(t, "gamma", goodSeeds[2]),
	}, "\n")

	entries, err := parseMirasimImportEntries(req)
	if err != nil {
		t.Fatalf("parseMirasimImportEntries error = %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("parseMirasimImportEntries returned %d entries, want 4", len(entries))
	}

	result, err := handler.importMirasimAccounts(context.Background(), req, entries)
	if err != nil {
		t.Fatalf("importMirasimAccounts error = %v", err)
	}

	if result.Created != 3 || result.Failed != 1 || result.Updated != 0 || result.Skipped != 0 {
		t.Fatalf("MirasimAccountImportResult counters = created %d / failed %d / updated %d / skipped %d, want 3/1/0/0",
			result.Created, result.Failed, result.Updated, result.Skipped)
	}
	// Verified is the anti-sampling invariant: every account that was written
	// carries a proven device identity, so the two numbers must agree exactly.
	if result.Verified != result.Created+result.Updated {
		t.Fatalf("MirasimAccountImportResult.Verified = %d, want Created+Updated = %d",
			result.Verified, result.Created+result.Updated)
	}

	// Per-account, not per-sample: derive each device id independently through
	// mirasim.NewDeviceSigner and require the importer to have reported that
	// exact id for that exact account.
	wantDeviceIDs := map[string]string{}
	for i, seed := range goodSeeds {
		signer, signerErr := mirasim.NewDeviceSigner(seed)
		if signerErr != nil {
			t.Fatalf("mirasim.NewDeviceSigner rejected good test seed %d: %v", i, signerErr)
		}
		if signer.DeviceID == "" {
			t.Fatalf("mirasim.NewDeviceSigner derived an empty DeviceID for good test seed %d", i)
		}
		wantDeviceIDs[seed] = signer.DeviceID
	}
	if len(wantDeviceIDs) != len(goodSeeds) {
		t.Fatalf("distinct seeds collapsed to %d device ids, want %d", len(wantDeviceIDs), len(goodSeeds))
	}

	if len(svc.createdAccounts) != 3 {
		t.Fatalf("service.CreateAccount called %d times, want 3", len(svc.createdAccounts))
	}
	reportedByDeviceID := map[string]MirasimAccountImportItem{}
	for _, item := range result.Items {
		if item.Action == "created" {
			reportedByDeviceID[item.DeviceID] = item
		}
	}
	for _, created := range svc.createdAccounts {
		if created.Platform != service.PlatformAnthropic || created.Type != service.AccountTypeAPIKey {
			t.Fatalf("created account platform/type = %q/%q, want %q/%q",
				created.Platform, created.Type, service.PlatformAnthropic, service.AccountTypeAPIKey)
		}
		if got := created.Credentials[mirasim.CredProvider]; got != mirasim.ProviderMirasim {
			t.Fatalf("created account credentials[%s] = %v, want %q", mirasim.CredProvider, got, mirasim.ProviderMirasim)
		}
		storedSeed, ok := created.Credentials[mirasim.CredDeviceSeed].(string)
		if !ok {
			t.Fatalf("created account has no string credentials[%s]", mirasim.CredDeviceSeed)
		}
		wantDeviceID, known := wantDeviceIDs[storedSeed]
		if !known {
			t.Fatalf("created account stored a device seed that was not in the source batch (device id %q)",
				mustMirasimDeviceID(t, storedSeed))
		}
		item, reported := reportedByDeviceID[wantDeviceID]
		if !reported {
			t.Fatalf("account with device id %q was created but not reported as verified", wantDeviceID)
		}
		if item.DeviceID != wantDeviceID {
			t.Fatalf("reported DeviceID = %q, want %q", item.DeviceID, wantDeviceID)
		}
		delete(wantDeviceIDs, storedSeed)
	}
	if len(wantDeviceIDs) != 0 {
		t.Fatalf("%d source accounts were never created", len(wantDeviceIDs))
	}

	// The damaged seed must be a hard failure with an explanation, and the
	// explanation must not quote the seed.
	if len(result.Errors) != 1 {
		t.Fatalf("MirasimAccountImportResult.Errors = %d entries, want 1", len(result.Errors))
	}
	if result.Errors[0].Index != 3 {
		t.Fatalf("error reported at index %d, want the third (broken) entry", result.Errors[0].Index)
	}
	if strings.Contains(result.Errors[0].Message, brokenSeed) {
		t.Fatalf("error message leaked the device seed")
	}
	for _, created := range svc.createdAccounts {
		if created.Credentials[mirasim.CredDeviceSeed] == brokenSeed {
			t.Fatalf("the unverifiable seed was written to an account anyway")
		}
	}
}

// TestImportMirasimAccountsRepeatedRunIsIdempotent re-runs the identical batch
// against the same store: the second run must converge on the same accounts
// rather than minting a second copy of every device.
func TestImportMirasimAccountsRepeatedRunIsIdempotent(t *testing.T) {
	// [[cov:MG:idempotent]]
	seeds := []string{
		mirasimImportTestSeed(t, "test-seed-0001"),
		mirasimImportTestSeed(t, "test-seed-0002"),
	}
	svc := newCodexImportMemoryAdminService(nil)
	handler := mirasimImportTestHandler(svc)
	req := mirasimImportTestRequest()
	req.Content = strings.Join([]string{
		mirasimImportTestEntryJSON(t, "alpha", seeds[0]),
		mirasimImportTestEntryJSON(t, "beta", seeds[1]),
	}, "\n")

	entries, err := parseMirasimImportEntries(req)
	if err != nil {
		t.Fatalf("parseMirasimImportEntries error = %v", err)
	}

	first, err := handler.importMirasimAccounts(context.Background(), req, entries)
	if err != nil {
		t.Fatalf("importMirasimAccounts (first run) error = %v", err)
	}
	if first.Created != 2 || first.Updated != 0 || first.Failed != 0 {
		t.Fatalf("first run = created %d / updated %d / failed %d, want 2/0/0", first.Created, first.Updated, first.Failed)
	}

	second, err := handler.importMirasimAccounts(context.Background(), req, entries)
	if err != nil {
		t.Fatalf("importMirasimAccounts (second run) error = %v", err)
	}
	if second.Created != 0 || second.Updated != 2 || second.Failed != 0 {
		t.Fatalf("second run = created %d / updated %d / failed %d, want 0/2/0 (a repeat must not create)",
			second.Created, second.Updated, second.Failed)
	}
	if second.Verified != second.Created+second.Updated {
		t.Fatalf("second run Verified = %d, want Created+Updated = %d", second.Verified, second.Created+second.Updated)
	}

	if len(svc.createdAccounts) != 2 {
		t.Fatalf("service.CreateAccount called %d times across two identical runs, want 2", len(svc.createdAccounts))
	}

	// The store must hold exactly one account per device identity, and the
	// second run must not have disturbed the identity of the first.
	deviceIDs := map[string]int64{}
	for _, account := range svc.accounts {
		storedSeed, ok := account.Credentials[mirasim.CredDeviceSeed].(string)
		if !ok {
			t.Fatalf("stored account %d has no string credentials[%s]", account.ID, mirasim.CredDeviceSeed)
		}
		deviceID := mustMirasimDeviceID(t, storedSeed)
		if previous, duplicate := deviceIDs[deviceID]; duplicate {
			t.Fatalf("device id %q is owned by two accounts (%d and %d) after a repeated import",
				deviceID, previous, account.ID)
		}
		deviceIDs[deviceID] = account.ID
		if account.Credentials[mirasim.CredProvider] != mirasim.ProviderMirasim {
			t.Fatalf("stored account %d lost credentials[%s] on the repeat run", account.ID, mirasim.CredProvider)
		}
	}
	if len(deviceIDs) != 2 {
		t.Fatalf("store holds %d distinct device identities, want 2", len(deviceIDs))
	}

	firstIDs := mirasimImportTestAccountIDs(first)
	secondIDs := mirasimImportTestAccountIDs(second)
	if !reflect.DeepEqual(firstIDs, secondIDs) {
		t.Fatalf("account ids moved between runs: %v then %v", firstIDs, secondIDs)
	}
}

// TestImportMirasimAccountsLeavesSourceDataUntouched pins the read-only
// contract: the parsed source records and the caller-supplied maps must come out
// of the import byte-identical, and the stored account must not alias them.
func TestImportMirasimAccountsLeavesSourceDataUntouched(t *testing.T) {
	// [[cov:MG:source-preserved]]
	seed := mirasimImportTestSeed(t, "test-seed-0001")
	req := mirasimImportTestRequest()
	req.Contents = []string{
		"[" + mirasimImportTestEntryJSON(t, "alpha", seed) + "]",
	}
	req.CredentialExtras = map[string]any{"pool_mode": "test-pool"}
	req.Extra = map[string]any{"test_extra_flag": true}

	entries, err := parseMirasimImportEntries(req)
	if err != nil {
		t.Fatalf("parseMirasimImportEntries error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("parseMirasimImportEntries returned %d entries, want 1", len(entries))
	}

	sourceBefore := mirasimImportTestSnapshot(t, entries)
	contentsBefore := append([]string(nil), req.Contents...)
	credentialExtrasBefore := mirasimImportTestSnapshot(t, req.CredentialExtras)
	extraBefore := mirasimImportTestSnapshot(t, req.Extra)

	svc := newCodexImportMemoryAdminService(nil)
	handler := mirasimImportTestHandler(svc)
	result, err := handler.importMirasimAccounts(context.Background(), req, entries)
	if err != nil {
		t.Fatalf("importMirasimAccounts error = %v", err)
	}
	if result.Created != 1 || result.Failed != 0 {
		t.Fatalf("MirasimAccountImportResult = created %d / failed %d, want 1/0", result.Created, result.Failed)
	}

	if got := mirasimImportTestSnapshot(t, entries); got != sourceBefore {
		t.Fatalf("importMirasimAccounts modified the parsed source records")
	}
	if !reflect.DeepEqual(req.Contents, contentsBefore) {
		t.Fatalf("importMirasimAccounts modified MirasimAccountImportRequest.Contents")
	}
	if got := mirasimImportTestSnapshot(t, req.CredentialExtras); got != credentialExtrasBefore {
		t.Fatalf("importMirasimAccounts modified MirasimAccountImportRequest.CredentialExtras")
	}
	if got := mirasimImportTestSnapshot(t, req.Extra); got != extraBefore {
		t.Fatalf("importMirasimAccounts modified MirasimAccountImportRequest.Extra")
	}

	// Aliasing probe: later writes to the stored credentials/extra (token
	// rotation does exactly this) must not reach back into the source record or
	// the caller's maps.
	if len(svc.createdAccounts) != 1 {
		t.Fatalf("service.CreateAccount called %d times, want 1", len(svc.createdAccounts))
	}
	created := svc.createdAccounts[0]
	if created.Credentials[mirasim.CredDeviceSeed] != seed {
		t.Fatalf("created account did not carry the source device seed")
	}
	created.Credentials[mirasim.CredAccessToken] = "test-rotated-token"
	created.Credentials[mirasim.CredDeviceSeed] = mirasimImportTestSeed(t, "test-seed-rotated")
	created.Extra[mirasim.ExtraSessionID] = "session_rotated"
	if got := mirasimImportTestSnapshot(t, entries); got != sourceBefore {
		t.Fatalf("stored credentials alias the source records: rotating a token rewrote the import source")
	}
	if got := mirasimImportTestSnapshot(t, req.Extra); got != extraBefore {
		t.Fatalf("stored extra aliases MirasimAccountImportRequest.Extra")
	}
	if got := mirasimImportTestSnapshot(t, req.CredentialExtras); got != credentialExtrasBefore {
		t.Fatalf("stored credentials alias MirasimAccountImportRequest.CredentialExtras")
	}
}

func TestImportMirasimAccountsSkipsDuplicateDeviceWithinOneBatch(t *testing.T) {
	seed := mirasimImportTestSeed(t, "test-seed-0001")
	svc := newCodexImportMemoryAdminService(nil)
	handler := mirasimImportTestHandler(svc)
	req := mirasimImportTestRequest()
	req.Content = strings.Join([]string{
		mirasimImportTestEntryJSON(t, "alpha", seed),
		mirasimImportTestEntryJSON(t, "alpha-copy", seed),
	}, "\n")

	entries, err := parseMirasimImportEntries(req)
	if err != nil {
		t.Fatalf("parseMirasimImportEntries error = %v", err)
	}
	result, err := handler.importMirasimAccounts(context.Background(), req, entries)
	if err != nil {
		t.Fatalf("importMirasimAccounts error = %v", err)
	}
	if result.Created != 1 || result.Skipped != 1 {
		t.Fatalf("result = created %d / skipped %d, want 1/1", result.Created, result.Skipped)
	}
	if len(svc.createdAccounts) != 1 {
		t.Fatalf("service.CreateAccount called %d times, want 1", len(svc.createdAccounts))
	}
}

func TestVerifyMirasimDeviceIdentityIsStableAndRejectsUnusableSeeds(t *testing.T) {
	seed := mirasimImportTestSeed(t, "test-seed-0001")
	first, err := verifyMirasimDeviceIdentity(seed)
	if err != nil {
		t.Fatalf("verifyMirasimDeviceIdentity rejected a valid seed: %v", err)
	}
	second, err := verifyMirasimDeviceIdentity(seed)
	if err != nil {
		t.Fatalf("verifyMirasimDeviceIdentity second call error = %v", err)
	}
	if first != second || first == "" {
		t.Fatalf("verifyMirasimDeviceIdentity returned %q then %q, want one stable non-empty device id", first, second)
	}
	signer, err := mirasim.NewDeviceSigner(seed)
	if err != nil {
		t.Fatalf("mirasim.NewDeviceSigner error = %v", err)
	}
	if first != signer.DeviceID {
		t.Fatalf("verifyMirasimDeviceIdentity device id = %q, want mirasim.NewDeviceSigner id %q", first, signer.DeviceID)
	}
	for _, bad := range []string{"", "   ", "not-base64-!!!", mirasimImportTestBrokenSeed(t)} {
		if _, err := verifyMirasimDeviceIdentity(bad); err == nil {
			t.Fatalf("verifyMirasimDeviceIdentity accepted an unusable seed (%d chars)", len(bad))
		}
	}
}

func TestNormalizeMirasimImportEntryFillsAccountShape(t *testing.T) {
	seed := mirasimImportTestSeed(t, "test-seed-0001")
	var value any
	if err := json.Unmarshal([]byte(mirasimImportTestEntryJSON(t, "alpha", seed)), &value); err != nil {
		t.Fatalf("unmarshal test entry: %v", err)
	}
	item, err := normalizeMirasimImportEntry(mirasimImportEntry{Index: 1, Value: value}, mirasimImportTestRequest())
	if err != nil {
		t.Fatalf("normalizeMirasimImportEntry error = %v", err)
	}
	if item.Credentials[mirasim.CredProvider] != mirasim.ProviderMirasim {
		t.Fatalf("credentials[%s] = %v, want %q", mirasim.CredProvider, item.Credentials[mirasim.CredProvider], mirasim.ProviderMirasim)
	}
	if item.Credentials["base_url"] != mirasimImportDefaultRelayBase {
		t.Fatalf("credentials[base_url] = %v, want %q", item.Credentials["base_url"], mirasimImportDefaultRelayBase)
	}
	if item.Extra["anthropic_passthrough"] != true {
		t.Fatalf("extra[anthropic_passthrough] = %v, want true", item.Extra["anthropic_passthrough"])
	}
	if item.Extra[mirasim.ExtraSessionID] != "session_alpha" {
		t.Fatalf("extra[%s] = %v, want session_alpha", mirasim.ExtraSessionID, item.Extra[mirasim.ExtraSessionID])
	}
	if got := item.Credentials[mirasim.CredExpiresAt]; got != "2030-01-02T03:04:05Z" {
		t.Fatalf("credentials[%s] = %v, want RFC3339 2030-01-02T03:04:05Z", mirasim.CredExpiresAt, got)
	}
	// The seed is attached only after verification, so normalization alone must
	// not have written it into the credentials that would be persisted.
	if _, present := item.Credentials[mirasim.CredDeviceSeed]; present {
		t.Fatalf("normalizeMirasimImportEntry wrote credentials[%s] before verification", mirasim.CredDeviceSeed)
	}
	if item.DeviceSeed != seed {
		t.Fatalf("normalizeMirasimImportEntry lost the source device seed")
	}
}

func TestNormalizeMirasimImportEntryRejectsMissingIdentity(t *testing.T) {
	req := mirasimImportTestRequest()
	noSeed := map[string]any{"access_token": "test-access-token"}
	if _, err := normalizeMirasimImportEntry(mirasimImportEntry{Index: 1, Value: noSeed}, req); err == nil {
		t.Fatalf("normalizeMirasimImportEntry accepted a record with no device seed")
	}
	noToken := map[string]any{"mirasim_device_seed": mirasimImportTestSeed(t, "test-seed-0001")}
	if _, err := normalizeMirasimImportEntry(mirasimImportEntry{Index: 1, Value: noToken}, req); err == nil {
		t.Fatalf("normalizeMirasimImportEntry accepted a record with no token")
	}
}

func TestParseMirasimProxyURLParsesCredentialsAndProtocol(t *testing.T) {
	spec, err := parseMirasimProxyURL("socks5h://test-user:test-pass@198.51.100.7:1080")
	if err != nil {
		t.Fatalf("parseMirasimProxyURL error = %v", err)
	}
	if spec.Protocol != "socks5h" || spec.Host != "198.51.100.7" || spec.Port != 1080 || spec.Username != "test-user" {
		t.Fatalf("mirasimProxySpec = %+v, want socks5h/198.51.100.7/1080/test-user", mirasimProxySpec{
			Protocol: spec.Protocol, Host: spec.Host, Port: spec.Port, Username: spec.Username,
		})
	}
	if _, err := parseMirasimProxyURL("ftp://198.51.100.7:1080"); err == nil {
		t.Fatalf("parseMirasimProxyURL accepted an unsupported protocol")
	}
	if _, err := parseMirasimProxyURL("http://198.51.100.7"); err == nil {
		t.Fatalf("parseMirasimProxyURL accepted a proxy without a port")
	}
}

func TestImportMirasimAccountsBindsSharedProxyOnce(t *testing.T) {
	seeds := []string{
		mirasimImportTestSeed(t, "test-seed-0001"),
		mirasimImportTestSeed(t, "test-seed-0002"),
	}
	svc := newCodexImportMemoryAdminService(nil)
	svc.proxies = []service.Proxy{{
		ID:       77,
		Protocol: "socks5h",
		Host:     "198.51.100.7",
		Port:     1080,
		Username: "test-user",
		Status:   service.StatusActive,
	}}
	handler := mirasimImportTestHandler(svc)
	req := mirasimImportTestRequest()
	entryJSON := func(name, seed string) string {
		var object map[string]any
		if err := json.Unmarshal([]byte(mirasimImportTestEntryJSON(t, name, seed)), &object); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		object["proxy"] = "socks5h://test-user:test-pass@198.51.100.7:1080"
		encoded, err := json.Marshal(object)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(encoded)
	}
	req.Content = strings.Join([]string{entryJSON("alpha", seeds[0]), entryJSON("beta", seeds[1])}, "\n")

	entries, err := parseMirasimImportEntries(req)
	if err != nil {
		t.Fatalf("parseMirasimImportEntries error = %v", err)
	}
	result, err := handler.importMirasimAccounts(context.Background(), req, entries)
	if err != nil {
		t.Fatalf("importMirasimAccounts error = %v", err)
	}
	if result.Created != 2 || result.Failed != 0 {
		t.Fatalf("result = created %d / failed %d, want 2/0: %v", result.Created, result.Failed, result.Errors)
	}
	for _, created := range svc.createdAccounts {
		if created.ProxyID == nil || *created.ProxyID != 77 {
			t.Fatalf("created account ProxyID = %v, want the existing proxy 77", created.ProxyID)
		}
	}
	if len(svc.createdProxies) != 0 {
		t.Fatalf("service.CreateProxy was called %d times for an already-known proxy", len(svc.createdProxies))
	}
}

func mustMirasimDeviceID(t *testing.T, seed string) string {
	t.Helper()
	signer, err := mirasim.NewDeviceSigner(seed)
	if err != nil {
		t.Fatalf("mirasim.NewDeviceSigner error = %v", err)
	}
	return signer.DeviceID
}

// mirasimImportTestSnapshot renders a value to canonical JSON so two states can
// be compared without printing secrets when they match.
func mirasimImportTestSnapshot(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("snapshot marshal error = %v", err)
	}
	return string(encoded)
}

func mirasimImportTestAccountIDs(result MirasimAccountImportResult) []string {
	ids := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		ids = append(ids, fmt.Sprintf("%s=%d", item.DeviceID, item.AccountID))
	}
	return ids
}
