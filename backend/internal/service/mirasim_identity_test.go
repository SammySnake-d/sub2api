package service

// 身份恒定（identity invariance）验收。
//
// 运营者的要求是一句话：一个号一个设备 id，迁移之后 id 必须不变，打进这个号的
// 多个客户的请求在上游看来必须是「同一台设备的正常请求」。
//
// 这条要求落到代码上是三个互相独立的不变量，任何一个破了都会在上游侧产生一个
// 真实设备装机群不可能产生的信号：
//
//  1. 设备根 —— DeviceSeed。deviceID 与 ed25519 私钥都由它派生
//     （internal/pkg/mirasim/device.go:32 NewDeviceSigner），所以 seed 一变就是
//     换了一台设备。它只存在于 accounts.credentials（唯一持久层），进程内存态
//     （mirasim.Registry）与账号快照缓存都只是它的副本。
//
//  2. 会话根 —— SessionID。它极易被当成临时值丢掉，而它不持久化的后果不是
//     「多一个随机值」：每个账号每次进程启动都会新铸一个 session id，于是**整个
//     号池在同一时刻同步轮换 session**。任何一组独立安装的真实客户端都不会
//     产生这种同步信号（见 runtime.go:48-57 ExtraSessionID 的注释）。
//
//  3. 客户端版本 —— 出站声称的 claude-cli 版本必须是账号（= 设备）的属性，
//     不能是「当前这个客户的属性」。sub2api 现有逻辑是
//     shouldMimicClaudeCode := account.IsOAuth() && !isClaudeCode
//     （gateway_forward.go:196），而 mirasim 账号是 api_key 账号，
//     IsOAuth() 恒为 false，于是走 allowedHeaders 白名单原样透传客户端的
//     user-agent / x-stainless-*。同一台「设备」的版本因此随客户而跳、甚至倒退。
//
// 以及一条与协议强耦合的配套不变量：x-mirasim-client 的值被写进签名规范串的
// 第 6 段（device.go:98），所以「声称的版本」与「实际签名算法」是一个组合，
// 不能单独 bump。
//
// 测试落点说明：
//
//   - 不变量 1（设备根）不在本文件。它的两条义务 ID:deviceseed-stable /
//     ID:deviceseed-db-backed 讲的是「真源是 accounts 行，不是缓存」和「跨客户端、
//     跨重启不变」，而生产里的「缓存」是 repository/mirasim_upstream.go 的
//     mirasimUpstream.bindings（TTL = mirasimBindingTTL）与 mirasim.Registry，
//     「客户端」是进到装饰器的那个 *http.Request。本文件在 package service 内，
//     反向 import repository 会成环，所以这两条打在
//     repository/mirasim_upstream_test.go 上，驱动真的装饰器与真的缓存层。
//     在这里用两个测试自造的 map 扮演「DB」和「缓存」只能得到恒真结论。
//
//   - 不变量 2（会话根）打在真正的身份内核 mirasim.Registry 上，账号读取路径按
//     repository/mirasim_upstream.go:262-272 逐字段镜像（mirasimIDIdentity）。
//
//   - 不变量 3 与配套不变量直接跑真实的出站组装函数 buildUpstreamRequest 与真实
//     签名实现 DeviceSigner.Headers / ccore.Sign。
//
//   - 末尾的 SG:retry-resigns 跑真实的透传重试循环
//     （forwardAnthropicAPIKeyPassthrough），因为那段代码就在 package service 里。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim/ccore"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// ============================================================================
// Fixtures
// ============================================================================

const mirasimIDSignPath = "/v1/messages"

var mirasimIDBody = []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

// mirasimIDAccessToken 造一个 JWT 形态的 access token：runtime.go 的 tokenExpiry /
// jwtClaimString 会去解 payload 的 exp / sub，exp 给足余量，避免 Prepare 触发刷新。
func mirasimIDAccessToken(sub string, exp time.Time) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims, _ := json.Marshal(map[string]any{"sub": sub, "exp": exp.Unix()})
	return head + "." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// mirasimIDAccount 构造一个 mirasim 账号的真实形态：platform=anthropic +
// credentials.provider=mirasim（repository/mirasim_upstream.go:294 isMirasimAccount
// 的判据），没有新 platform 枚举。
func mirasimIDAccount(id int64, seed string) *Account {
	exp := time.Now().Add(40 * time.Minute)
	return &Account{
		ID:       id,
		Name:     "mirasim-identity-test",
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			mirasim.CredProvider:    mirasim.ProviderMirasim,
			mirasim.CredDeviceSeed:  seed,
			mirasim.CredAccessToken: mirasimIDAccessToken("usr_identity", exp),
			mirasim.CredExpiresAt:   exp.Format(time.RFC3339),
			"base_url":              "https://relay.mirasim.ai",
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func mirasimIDNewSeed(t *testing.T) string {
	t.Helper()
	seed, err := mirasim.NewDeviceSeed()
	require.NoError(t, err)
	return seed
}

// ----------------------------------------------------------------------------
// 持久层 / 缓存层的显式分层
// ----------------------------------------------------------------------------

// mirasimIDDurable 是 accounts 表：**唯一**的持久层。进程重启、缓存清空都不影响它，
// 而它之外的任何一层丢了，身份都必须还能原样恢复。
type mirasimIDDurable struct {
	mu          sync.Mutex
	rows        map[int64]*Account
	reads       int
	credWrites  []map[string]any
	extraWrites []map[string]any
}

func mirasimIDNewDurable(accounts ...*Account) *mirasimIDDurable {
	d := &mirasimIDDurable{rows: map[int64]*Account{}}
	for _, acc := range accounts {
		d.rows[acc.ID] = acc
	}
	return d
}

// read 返回深拷贝并计数：拿到的永远是「刚从库里读出来的一行」，
// 调用方不可能拿到上一次读的同一个对象。
func (d *mirasimIDDurable) read(id int64) *Account {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	row, ok := d.rows[id]
	if !ok {
		return nil
	}
	clone := *row
	clone.Credentials = map[string]any{}
	for k, v := range row.Credentials {
		clone.Credentials[k] = v
	}
	clone.Extra = map[string]any{}
	for k, v := range row.Extra {
		clone.Extra[k] = v
	}
	return &clone
}

func (d *mirasimIDDurable) writeCredentials(id int64, updates map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.rows[id]
	if row.Credentials == nil {
		row.Credentials = map[string]any{}
	}
	for k, v := range updates {
		row.Credentials[k] = v
	}
	d.credWrites = append(d.credWrites, updates)
}

func (d *mirasimIDDurable) writeExtra(id int64, updates map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.rows[id]
	if row.Extra == nil {
		row.Extra = map[string]any{}
	}
	for k, v := range updates {
		row.Extra[k] = v
	}
	d.extraWrites = append(d.extraWrites, updates)
}

func (d *mirasimIDDurable) extra(id int64, key string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.rows[id]
	if row.Extra == nil {
		return ""
	}
	s, _ := row.Extra[key].(string)
	return s
}

// extraWriteCount 统计 ExtraSessionID 被写了几次：一个账号只应该铸一次 session id，
// 每次重启都写一次就是「号池同步轮换」的直接证据。
func (d *mirasimIDDurable) extraWriteCount(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, w := range d.extraWrites {
		if _, ok := w[key]; ok {
			n++
		}
	}
	return n
}

// mirasimIDProcess 是一个运行中的 sub2api 进程：
//   - reg   = mirasim.Registry，纯进程内存态，重启即失；
//   - cache = 账号快照缓存（对应 Redis 账号缓存 / 装饰器的 bindings 那一层），可随时清空。
//
// 「重启」= mirasimIDBoot 出一个新 process；「清缓存」= flushCache。
type mirasimIDProcess struct {
	durable *mirasimIDDurable
	reg     *mirasim.Registry

	mu    sync.Mutex
	cache map[int64]*Account
}

func mirasimIDBoot(d *mirasimIDDurable) *mirasimIDProcess {
	return &mirasimIDProcess{durable: d, reg: mirasim.NewRegistry(), cache: map[int64]*Account{}}
}

func (p *mirasimIDProcess) flushCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache = map[int64]*Account{}
}

func (p *mirasimIDProcess) account(id int64) *Account {
	p.mu.Lock()
	if acc, ok := p.cache[id]; ok {
		p.mu.Unlock()
		return acc
	}
	p.mu.Unlock()

	acc := p.durable.read(id)
	if acc != nil {
		p.mu.Lock()
		p.cache[id] = acc
		p.mu.Unlock()
	}
	return acc
}

// mirasimIDIdentity 逐字段镜像 repository/mirasim_upstream.go:262-272 的账号读取。
// 用的是真实的 Account.GetCredential / GetCredentialAsTime 与 mirasim 的常量，
// 没有自造字段名。
func mirasimIDIdentity(acc *Account) mirasim.Identity {
	ident := mirasim.Identity{
		DeviceSeed:   acc.GetCredential(mirasim.CredDeviceSeed),
		AccessToken:  acc.GetCredential(mirasim.CredAccessToken),
		RefreshToken: acc.GetCredential(mirasim.CredRefreshToken),
		AuthBase:     acc.GetCredential(mirasim.CredAuthBase),
		RelayBase:    strings.TrimSpace(acc.GetCredential("base_url")),
	}
	if acc.Extra != nil {
		s, _ := acc.Extra[mirasim.ExtraSessionID].(string)
		ident.SessionID = strings.TrimSpace(s)
	}
	if t := acc.GetCredentialAsTime(mirasim.CredExpiresAt); t != nil {
		ident.ExpiresAt = *t
	}
	return ident
}

// mirasimIDDeadRelay 让控制面调用（device ticket 铸造）一律失败：
// runtime.go:374 authorizationLocked 的约定是「铸不出 ticket 就退回 access token」，
// 单元测试里不需要也不应该有网络。
type mirasimIDDeadRelay struct{}

func (mirasimIDDeadRelay) Do(_ *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func (p *mirasimIDProcess) prepareErr(id int64) (mirasim.Prepared, error) {
	acc := p.account(id)
	if acc == nil {
		return mirasim.Prepared{}, errors.New("account row is gone")
	}
	invalidate := func(accountID int64) {
		p.mu.Lock()
		delete(p.cache, accountID)
		p.mu.Unlock()
	}
	return p.reg.Prepare(context.Background(), id, mirasimIDIdentity(acc), mirasimIDDeadRelay{},
		func(_ context.Context, accountID int64, updates map[string]any) error {
			p.durable.writeCredentials(accountID, updates)
			invalidate(accountID)
			return nil
		},
		func(_ context.Context, accountID int64, updates map[string]any) error {
			p.durable.writeExtra(accountID, updates)
			invalidate(accountID)
			return nil
		})
}

// prepare 是「这个账号收到一个客户请求」的最小真实动作。
func (p *mirasimIDProcess) prepare(t *testing.T, id int64) mirasim.Prepared {
	t.Helper()
	out, err := p.prepareErr(id)
	require.NoError(t, err)
	return out
}

// mirasimIDContextHeaders 复刻 repository/mirasim_upstream.go:174-188 的 session 取值
// （客户端自带的 session 优先，其次账号的稳定 fallback），然后调用真实的
// mirasim.ApplyContextHeaders 产出一次请求的 x-mirasim-* 上下文头。
func mirasimIDContextHeaders(prepared mirasim.Prepared, clientSessionID string) http.Header {
	sessionID := strings.TrimSpace(clientSessionID)
	if sessionID == "" {
		sessionID = prepared.SessionID
	}
	h := http.Header{}
	mirasim.ApplyContextHeaders(h, mirasim.ContextInput{
		Path:       mirasimIDSignPath,
		SessionID:  sessionID,
		AccountSub: prepared.AccountSub,
		Locale:     prepared.Locale,
	})
	return h
}

// ----------------------------------------------------------------------------
// 出站请求组装（版本类义务用）
// ----------------------------------------------------------------------------

// mirasimIDOutbound 跑真实的出站组装。mirasim 账号是 api_key 账号，
// gateway_forward.go:196 的 shouldMimicClaudeCode = account.IsOAuth() && !isClaudeCode
// 对它恒为 false，所以这里第 9 个参数传 false —— 与生产一致，不是为了让测试好看。
func mirasimIDOutbound(t *testing.T, acc *Account, clientHeaders map[string]string) *http.Request {
	t.Helper()
	require.False(t, acc.IsOAuth(),
		"前提自检：mirasim 账号是 api_key 账号，IsOAuth() 必须为 false —— "+
			"这正是它掉出 sub2api 现有版本归一逻辑的原因")

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, mirasimIDSignPath, bytes.NewReader(mirasimIDBody))
	for k, v := range clientHeaders {
		c.Request.Header.Set(k, v)
	}

	svc := &GatewayService{cfg: &config.Config{}}
	req, _, err := svc.buildUpstreamRequest(
		context.Background(), c, acc, mirasimIDBody,
		"upstream-token", "api_key", "claude-opus-5", false, false,
	)
	require.NoError(t, err)
	return req
}

// mirasimIDCLIVersion 从 user-agent 里抠出 claude-cli 的版本号段。
func mirasimIDCLIVersion(ua string) string {
	const prefix = "claude-cli/"
	i := strings.Index(ua, prefix)
	if i < 0 {
		return ""
	}
	rest := ua[i+len(prefix):]
	if j := strings.IndexAny(rest, " \t("); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// mirasimIDSnapshot 是一组「确实一起出厂过」的客户端身份值。
//
// 不变量是「三个值来自同一组快照」，而不是「必须是哪一组」：实现方选 sub2api
// 自己的 defaultFingerprint，或选 internal/pkg/mirasim/fingerprint.go:44-53
// 记录的 ma-relay 抓包快照，都算自洽；把两组混着发才是「一个从未发行过的组合」，
// 比版本号旧更容易被上游认出来。
type mirasimIDSnapshot struct {
	name             string
	cliVersion       string
	stainlessPackage string
	stainlessRuntime string
}

func mirasimIDKnownSnapshots() []mirasimIDSnapshot {
	return []mirasimIDSnapshot{
		{
			name:             "sub2api defaultFingerprint",
			cliVersion:       claude.CLIVersion(),
			stainlessPackage: defaultFingerprint.StainlessPackageVersion,
			stainlessRuntime: defaultFingerprint.StainlessRuntimeVersion,
		},
		{
			// internal/pkg/mirasim/fingerprint.go:44-53 记录的 ma-relay 抓包快照
			// （Claude Code 2.1.272 的真实请求头），必须整组取用。
			name:             "ma-relay captured snapshot",
			cliVersion:       "2.1.272",
			stainlessPackage: "0.112.1",
			stainlessRuntime: "v26.3.0",
		},
	}
}

// ============================================================================
// 义务
// ============================================================================

func TestMirasimIdentitySessionIDStableAcrossRestart(t *testing.T) {
	// [[cov:ID:sessionid-stable]] x-mirasim-session fallback 跨重启不变
	seed := mirasimIDNewSeed(t)
	durable := mirasimIDNewDurable(mirasimIDAccount(21, seed))

	first := mirasimIDBoot(durable).prepare(t, 21)
	require.NotEmpty(t, first.SessionID, "首次使用必须铸出一个 session fallback")
	require.Equal(t, first.SessionID, durable.extra(21, mirasim.ExtraSessionID),
		"新铸的 session id 必须落到持久层的 extra.%s，否则重启就丢", mirasim.ExtraSessionID)

	// 重启：全新 Registry + 空缓存。
	restarted := mirasimIDBoot(durable)
	restarted.flushCache()
	second := restarted.prepare(t, 21)
	require.Equal(t, first.SessionID, second.SessionID,
		"重启后 session id 变了 = 整个号池在同一时刻同步轮换 session，"+
			"这是任何一组独立客户端安装都不会产生的信号")
	require.Equal(t, 1, durable.extraWriteCount(mirasim.ExtraSessionID),
		"session id 只应在首次使用时铸造并写入一次；每次重启都写一次即为轮换")

	// 客户端没带自己的 session 时，出站 x-mirasim-session 用的就是这个稳定 fallback。
	require.Equal(t, first.SessionID,
		mirasimIDContextHeaders(second, "").Get("x-mirasim-session"),
		"ApplyContextHeaders 必须把稳定 fallback 写进 x-mirasim-session")
}

func TestMirasimIdentitySessionIDDistinctPerAccount(t *testing.T) {
	// [[cov:ID:sessionid-distinct]] 不同账号的 SessionID 互不相同（防止被归一成同一个常量）
	durable := mirasimIDNewDurable(
		mirasimIDAccount(31, mirasimIDNewSeed(t)),
		mirasimIDAccount(32, mirasimIDNewSeed(t)),
		mirasimIDAccount(33, mirasimIDNewSeed(t)),
	)
	proc := mirasimIDBoot(durable)

	seen := map[string]int64{}
	for _, id := range []int64{31, 32, 33} {
		prepared := proc.prepare(t, id)
		require.NotEmpty(t, prepared.SessionID, "账号 %d 没有 session fallback", id)
		if other, dup := seen[prepared.SessionID]; dup {
			t.Fatalf("账号 %d 与账号 %d 的 SessionID 相同（%q）：多个号共用一个 session "+
				"等于把整池请求归一成同一个客户端会话", id, other, prepared.SessionID)
		}
		seen[prepared.SessionID] = id
		require.Equal(t, prepared.SessionID, durable.extra(id, mirasim.ExtraSessionID),
			"账号 %d 的 session id 必须按账号各自持久化", id)
	}
	require.Len(t, seen, 3, "三个账号必须得到三个互不相同的 SessionID")
}

func TestMirasimIdentityOutboundVersionIndependentOfClient(t *testing.T) {
	// [[cov:ID:version-client-independent]] 出站 claude-cli 版本与入站客户端版本无关，恒为 canonical 值
	acc := mirasimIDAccount(41, mirasimIDNewSeed(t))

	// 两个客户端打同一个号：一个比 canonical 旧，一个比 canonical 新。
	// （两个都不等于 canonical，所以「出站等于 canonical」不可能是碰巧撞上其中之一。）
	const clientOldVersion = "2.1.100"
	const clientNewVersion = "2.9.0"
	canonical := claude.CLIVersion()
	require.NotEqual(t, clientOldVersion, canonical)
	require.NotEqual(t, clientNewVersion, canonical)

	outOld := mirasimIDOutbound(t, acc, map[string]string{
		"User-Agent":                  "claude-cli/" + clientOldVersion + " (external, cli)",
		"X-Stainless-Package-Version": "0.91.1",
		"X-Stainless-Runtime-Version": "v22.14.0",
	})
	outNew := mirasimIDOutbound(t, acc, map[string]string{
		"User-Agent":                  "claude-cli/" + clientNewVersion + " (external, cli)",
		"X-Stainless-Package-Version": "0.112.1",
		"X-Stainless-Runtime-Version": "v26.3.0",
	})

	uaOld := getHeaderRaw(outOld.Header, "User-Agent")
	uaNew := getHeaderRaw(outNew.Header, "User-Agent")

	require.Equal(t, uaOld, uaNew,
		"同一个号（= 同一台设备）不得因为换了客户而改口径。\n"+
			"旧客户出站 UA: %q\n新客户出站 UA: %q", uaOld, uaNew)
	require.NotEqual(t, clientOldVersion, mirasimIDCLIVersion(uaOld),
		"出站版本等于旧客户自报的版本 = 没有归一，只是透传")
	require.NotEqual(t, clientNewVersion, mirasimIDCLIVersion(uaNew),
		"出站版本等于新客户自报的版本 = 设备版本会随客户跳，甚至倒退")
	require.Equal(t, canonical, mirasimIDCLIVersion(uaOld),
		"出站版本必须恒为 canonical 值 claude.CLIVersion()=%s", canonical)
}

func TestMirasimIdentityOutboundVersionSetIsCoherent(t *testing.T) {
	// [[cov:ID:version-coherent]] UA 版本与 x-stainless-package-version / runtime-version 自洽
	acc := mirasimIDAccount(42, mirasimIDNewSeed(t))

	// 入站是一个从未出厂过的混搭：claude-cli/2.1.100 配 ma-relay 快照里属于
	// 2.1.272 的 stainless 版本。原样透传出去，上游一看就是拼出来的。
	out := mirasimIDOutbound(t, acc, map[string]string{
		"User-Agent":                  "claude-cli/2.1.100 (external, cli)",
		"X-Stainless-Package-Version": "0.112.1",
		"X-Stainless-Runtime-Version": "v26.3.0",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Runtime":         "node",
	})

	gotCLI := mirasimIDCLIVersion(getHeaderRaw(out.Header, "User-Agent"))
	gotPackage := getHeaderRaw(out.Header, "X-Stainless-Package-Version")
	gotRuntime := getHeaderRaw(out.Header, "X-Stainless-Runtime-Version")

	var matched string
	for _, snap := range mirasimIDKnownSnapshots() {
		if gotCLI == snap.cliVersion && gotPackage == snap.stainlessPackage && gotRuntime == snap.stainlessRuntime {
			matched = snap.name
			break
		}
	}
	// 先钉死出站的三个值各自是什么，再判它们属于同一份快照。
	// 只断言 "matched 非空" 的话，把三个值全改成另一份一致的快照照样绿 ——
	// 那就丢掉了「归一到 canonical 值」这半条不变量。
	require.Equal(t, claude.CLIVersion(), gotCLI,
		"出站 claude-cli 版本必须是 canonical 值，而不是客户自报的 2.1.100")
	// 比的是 **mirasim 自己的** canonical 快照，不是 sub2api 的 defaultFingerprint。
	// 两者此刻并不相同：defaultFingerprint.StainlessPackageVersion 还停在 0.94.0，
	// 而真实 Claude Code 2.1.272 实测发的是 0.112.1（2026-09-16 本地抓包，
	// 连同 x-stainless-runtime-version=v26.3.0 一起核过）。mirasim lane 用的是后者。
	// 拿 defaultFingerprint 当期望值会把这条门钉在一个**过时**的值上。
	canonicalHeaders := mirasim.CanonicalIdentityHeaders()
	require.Equal(t, canonicalHeaders["X-Stainless-Package-Version"], gotPackage,
		"出站 x-stainless-package-version 必须是 mirasim canonical 值")
	require.Equal(t, canonicalHeaders["X-Stainless-Runtime-Version"], gotRuntime,
		"出站 x-stainless-runtime-version 必须是 mirasim canonical 值")

	require.NotEmptyf(t, matched,
		"出站的 (claude-cli, x-stainless-package-version, x-stainless-runtime-version) "+
			"必须整组来自同一份真实快照，不得混搭。\n"+
			"实际出站: claude-cli=%s package=%s runtime=%s\n"+
			"已知快照: sub2api defaultFingerprint = (%s, %s, %s) / ma-relay = (2.1.272, 0.112.1, v26.3.0)",
		gotCLI, gotPackage, gotRuntime,
		claude.CLIVersion(), defaultFingerprint.StainlessPackageVersion, defaultFingerprint.StainlessRuntimeVersion)
}

func TestMirasimIdentityClientHeaderPairedWithSigningScheme(t *testing.T) {
	// [[cov:ID:mirasim-client-paired]] x-mirasim-client 的值与实际签名算法版本属同一组合
	//
	// x-mirasim-client 是唯一不进 seal 的 x-mirasim-* 头（seal.go:84），服务端在解封
	// 之前就能读到它，因而可以据此推断「这个版本应该用哪套签名规范串」。它同时是
	// 规范串的第 6 段（device.go:98），所以声称的版本与签名方案是一个组合：
	// 单独 bump 版本号 = 声称 0.0.326（mrs-sig-v2）却按 0.0.307 的方式签，
	// 这比版本号旧更容易被认出来。
	seed := mirasimIDNewSeed(t)
	signer, err := mirasim.NewDeviceSigner(seed)
	require.NoError(t, err)

	require.Equal(t, "0.0.307", mirasim.ClientVersion,
		"ma-relay 在生产上跑的就是 0.0.307 这套方案；换方案之前不得 bump 这个常量")

	const credential = "test-credential"
	const meta = ""
	hdrs, err := signer.Headers(http.MethodPost, mirasimIDSignPath, mirasimIDBody, credential, meta, time.Now())
	require.NoError(t, err)
	require.Equal(t, mirasim.ClientVersion, hdrs["x-mirasim-client"],
		"出站声称的版本必须就是签名实现用的那个常量")

	// 按服务端的方式重建规范串：method \0 path \0 ts \0 nonce \0 deviceId \0
	// clientVersion \0 credential，clientVersion 取**明文头里声称的那个值**。
	canonicalWith := func(version string) string {
		return strings.Join([]string{
			http.MethodPost,
			mirasimIDSignPath,
			hdrs["x-mirasim-ts"],
			hdrs["x-mirasim-nonce"],
			hdrs["x-mirasim-device"],
			version,
			credential,
		}, "\x00")
	}

	declaredSig, err := ccore.Sign(signer.Seed(), canonicalWith(hdrs["x-mirasim-client"]), meta, mirasimIDBody)
	require.NoError(t, err)
	require.Equal(t, hdrs["x-mirasim-sig"], base64.RawURLEncoding.EncodeToString(declaredSig),
		"用明文声称的 x-mirasim-client 重算签名必须与实际发出的 x-mirasim-sig 一致 —— "+
			"这就是「声称的版本」与「实际签名」属同一组合的定义")

	// 反向对照：换成 mrs-sig-v2 时代的版本号，签名必须不同。
	// 签名对版本号不敏感 = 服务端无法从版本推断签名形态，这条配对不变量也就不存在了。
	otherSig, err := ccore.Sign(signer.Seed(), canonicalWith("0.0.326"), meta, mirasimIDBody)
	require.NoError(t, err)
	require.NotEqual(t, base64.RawURLEncoding.EncodeToString(declaredSig), base64.RawURLEncoding.EncodeToString(otherSig),
		"规范串第 6 段换了版本号签名却没变，说明 ClientVersion 根本没进签名")
}

// ============================================================================
// 重试路径：每次尝试都是新建 + 重新签名的请求
// ============================================================================

// mirasimRetryCredential 是 seam 注入的设备票据，扮演生产里
// mirasim.Prepared.Credential 的位置：它只可能由签名层写上去，所以它出现在
// 「进入签名层的那一刻」就等于这个请求已经被签过一次了。
const mirasimRetryCredential = "mirasim-device-ticket"

// mirasimRetryAttempt 是签名层在一次尝试里看到的东西。
type mirasimRetryAttempt struct {
	req     *http.Request // 指针身份：重放的话两次会是同一个对象
	inbound http.Header   // 进入签名层那一刻的头（克隆，尚未被本层改写）
	body    []byte
	sealed  string // 本层签完之后的 x-mirasim-enc
}

// mirasimRetrySeam 站在生产里签名装饰器所在的**那个位置**：GatewayService.httpUpstream。
// 组装时包住这个口子的就是 repository.NewMirasimUpstream(NewHTTPUpstream(cfg), accountRepo)，
// 所以「重试时装饰器会看到什么」等价于「这个 seam 会看到什么」。
//
// 本包 import repository 会成环，所以这里不是包装那个装饰器，而是在同一个位置、按
// repository/mirasim_upstream.go sign() 的同一顺序跑**真实的**签名实现
// （删 x-api-key → 换 Authorization → ApplyContextHeaders → SignAndSeal），
// 并把每次尝试进来时的原始头原样留证。
type mirasimRetrySeam struct {
	signer   *mirasim.DeviceSigner
	statuses []int
	attempts []mirasimRetryAttempt
}

func (s *mirasimRetrySeam) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, concurrency)
}

func (s *mirasimRetrySeam) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	att := mirasimRetryAttempt{req: req, inbound: req.Header.Clone()}
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		att.body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}

	const credential = mirasimRetryCredential
	req.Header.Del("x-api-key")
	req.Header.Set("Authorization", "Bearer "+credential)
	mirasim.ApplyContextHeaders(req.Header, mirasim.ContextInput{
		Path:       req.URL.Path,
		SessionID:  "session_from_client",
		AccountSub: "usr_identity",
		Locale:     "en-US",
	})
	if err := mirasim.SignAndSeal(req.Header, s.signer, req.Method, req.URL.Path, att.body, credential); err != nil {
		return nil, err
	}
	att.sealed = req.Header.Get("x-mirasim-enc")
	s.attempts = append(s.attempts, att)

	status := s.statuses[len(s.statuses)-1]
	if i := len(s.attempts) - 1; i < len(s.statuses) {
		status = s.statuses[i]
	}
	body := `{"type":"error","error":{"type":"api_error","message":"boom"}}`
	if status == http.StatusOK {
		body = `{"id":"msg_1","type":"message","usage":{"input_tokens":12,"output_tokens":7}}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func TestMirasimRetryRebuildsAndResignsInsteadOfReplaying(t *testing.T) {
	// [[cov:SG:retry-resigns]] 失败重试走的是「重新构造请求 + 重新签名」，
	// 不是把上一次那个已签请求改一改再发一遍。
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, mirasimIDSignPath, bytes.NewReader(mirasimIDBody))
	c.Request.Header.Set("x-claude-code-session-id", "session_from_client")

	seed := mirasimIDNewSeed(t)
	signer, err := mirasim.NewDeviceSigner(seed)
	require.NoError(t, err)
	seam := &mirasimRetrySeam{signer: signer, statuses: []int{http.StatusInternalServerError, http.StatusOK}}

	acc := mirasimIDAccount(51, seed)
	// 透传路径的前置条件，与生产一致：apikey 账号（GetAccessToken 要 api_key，
	// 导入器写的是占位符）+ extra.anthropic_passthrough=true。
	acc.Credentials["api_key"] = "mirasim-placeholder"
	// 500 要真的进这个重试循环，账号必须开自定义错误码且 500 不在表里：
	// account.go:1254 ShouldHandleErrorCode 在未开启时返回 true，于是
	// shouldRetryUpstreamError 恒为 false，5xx 会直接走账号 failover 而不是本循环。
	acc.Credentials["custom_error_codes_enabled"] = true
	acc.Credentials["custom_error_codes"] = []any{float64(http.StatusPaymentRequired)}
	acc.Extra = map[string]any{"anthropic_passthrough": true}
	require.True(t, IsMirasimAccount(acc), "前提自检：这必须是一个 mirasim 账号")

	svc := &GatewayService{
		cfg:              &config.Config{},
		httpUpstream:     seam,
		rateLimitService: &RateLimitService{},
	}

	result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, acc,
		mirasimIDBody, "claude-opus-5", "claude-opus-5", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, seam.attempts, 2,
		"上游 500 之后必须真的有第二次尝试，否则这条义务无从谈起（前提自检）")
	first, second := seam.attempts[0], seam.attempts[1]

	// 核心区分度：第二次尝试到达签名层时，是一个**全新的、还没签过的**请求。
	require.NotSame(t, first.req, second.req,
		"重试把第一次那个 *http.Request 又交给了签名层 = 改写并重发已签请求")
	for k := range second.inbound {
		require.Falsef(t, strings.HasPrefix(strings.ToLower(k), "x-mirasim-"),
			"第二次尝试进签名层时就带着 %s：上一次签好的头被重放了", k)
	}
	require.NotEmpty(t, second.inbound.Get("x-api-key"),
		"第二次尝试没有重新走 buildUpstreamRequestAnthropicAPIKeyPassthrough："+
			"入站鉴权头在上一次签名时已经被删掉，它不会自己长回来")
	require.NotEqual(t, "Bearer "+mirasimRetryCredential, second.inbound.Get("Authorization"),
		"第二次尝试带着上一次签名层注入的设备票据进来 = 同一个已签请求被重发")

	// 重新签名的结果：两次的封套不同。这是必要条件不是充分条件
	// （SealHeaders 每次都画新 nonce），真正的区分度在上面三条。
	require.NotEmpty(t, second.sealed, "第二次尝试没有被签名")
	require.NotEqual(t, first.sealed, second.sealed,
		"两次的 x-mirasim-enc 完全相同：同一个封套被重放了")

	// 重试不得改写请求体：mirasim 的签名覆盖 body，改一个字节两边就对不上。
	require.NotEmpty(t, first.body)
	require.Equal(t, first.body, second.body, "重试改写了请求体")
	require.Equal(t, mirasim.ClientVersion, second.req.Header.Get("x-mirasim-client"),
		"重签用的仍必须是 mirasim.ClientVersion 这套方案")
}
