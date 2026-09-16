package repository

// mirasim lane 的 **body 层**身份收敛：metadata.user_id。
//
// 收口之前，同一个 mirasim 账号的出站身份是三层不一致的：
//
//	层                        现状                                上游看到
//	────────────────────────  ──────────────────────────────────  ──────────────────────
//	x-mirasim-session 头      客户端带了 x-claude-code-session-id  会话随客户走
//	                          就用它，否则回落账号级
//	                          accounts.extra.mirasim_session_id
//	UA / x-stainless-* 头     已收口为 canonical                  同一台设备
//	                          (mirasim.ApplyCanonicalIdentityHeaders)
//	body 的 metadata.user_id  客户端原值透传，零处理               每次都变，内含客户端
//	                                                              真实 device ID 与
//	                                                              session ID
//
// 第三行就是这个文件补的缺口。它比"三层都不收口"更糟：头层已经统一声称"同一台设备"，
// body 里却逐字带着客户端自己的 device id —— 同一个账号被多个客户端用时，上游看到
// 一台设备报出多个 device id；反过来同一个客户端用多个账号时,上游能凭这个 device id
// 把这些账号关联到同一台真实机器。
//
// ── 为什么不复用 service 里已有的那套重写 ─────────────────────────────────────
//
// **那套重写对 mirasim 从未执行过**，而且它的两个前置条件在 mirasim 上都不成立。
// gateway_upstream_request.go:62 的 identityService.RewriteUserIDWithMasking 挂在
// `account.IsOAuth()` 分支里，IsOAuth() = (Type == oauth || Type == setup_token)
// （service/account.go:245）。实测事实：
//
//   - 生产库里 143 个 mirasim 账号**全部**是 type = apikey、platform = anthropic
//     → 那个分支一次都没进过；
//   - 就算进了，它还要求 extra.account_uuid 非空，而那 143 个账号里 **0 个**有
//     account_uuid → 里层的 if 也过不去。
//
// 所以下一个读到这里的人最可能做的错事是：「已经有 session_id_masking 开关了，把这个
// 文件删掉去复用它」。别做。两个理由，各自独立成立：
//
//  1. session_id_masking 服务的是 Anthropic OAuth 账号，它的伪装 session 是
//     **15 分钟滚动**的（identity_service.go 的 GetMaskedSessionID / SetMaskedSessionID，
//     TTL 15 分钟、每请求续期）。mirasim 要的是账号级**稳定**身份，而稳定身份已经有
//     一个持久化真源了（accounts.extra.mirasim_session_id，见 mirasim.ExtraSessionID
//     的注释：它必须持久化，否则整个号池会在每次进程重启时同步轮换 session）。
//     再挂一个会变的东西上去就是第二个真源。
//  2. 那条路径在**网关**里，而网关只覆盖客户流量：账号健康检查、计划探测、额度探测
//     都不走网关。这正是头层收口当初从两条网关路径搬到传输层签名点的原因（2026-09-16
//     线上 usage_logs 实录：客户流量 MacOS/0.112.1，健康检查 Linux/0.94.0，同一个账号
//     在上游看来是两台机器，而且完全静默 —— 健康检查照样 200）。body 层如果留在网关，
//     就是把同一个错再犯一遍。
//
// ── 落点 ───────────────────────────────────────────────────────────────────────
//
// 唯一落点是 repository.mirasimUpstream.sign，和 mirasim.ApplyCanonicalIdentityHeaders
// 同一个函数体。头和 body 的身份从**同一份 canonical 定义**推出来，"头和 body 说的是
// 同一台设备"因此是结构上成立的事，不是两处各自维护出来的巧合。
//
// 顺序约束（不可调换）：mirasim 的签名**覆盖 body**（ccore.Sign 把 body 一起哈希，
// 见 device.go headersWithNonce），所以改写必须发生在 mirasim.SignAndSeal **之前**，
// 并且必须把改写后的字节重新装回 *http.Request，否则签名与上线字节不一致 → 上游 403。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 派生盐。分三把是为了让同一颗设备根派生出的三个字段互不可推导：只拿到 body 里的
// device_id 段推不出 account_uuid 段，反之亦然。
const (
	mirasimMetaDeviceSalt  = "mirasim-metadata-device\x00"
	mirasimMetaAccountSalt = "mirasim-metadata-account\x00"
	mirasimMetaSessionSalt = "mirasim-metadata-session\x00"
)

// mirasimMetaDeviceID 把 mirasim 的 22 字符 device id 映射成 Claude Code 的
// metadata.user_id device 段形态：32 字节 hex（64 字符）。
//
// 为什么要换形态而不是直接塞 signer.DeviceID：metadata.user_id 的 legacy 形态被
// service.legacyUserIDRegex 钉成 ^user_([a-fA-F0-9]{64})_account_..._session_...，
// 22 字符 base64url 连自家解析器都过不了；上游侧更是一个"这个客户端我没见过"的
// 形态信号。
//
// 为什么从 signer.DeviceID 派生而不是另铸一个持久字段：DeviceID 本身已经是设备根
// （accounts.credentials.mirasim_device_seed）的确定性函数（device.go 里
// NewDeviceSigner 对 SPKI 公钥取 sha256 再截 22 字符），所以派生值跨请求、跨客户端、
// 跨进程重启都不变，不同账号必不相同，而且不需要第二个持久化字段来维护一致性 ——
// 它和签名里那台设备是同一台，这一点由派生关系保证，不靠人记得同步。
// sha256 单向，派生值不泄露 seed。
func mirasimMetaDeviceID(deviceID string) string {
	sum := sha256.Sum256([]byte(mirasimMetaDeviceSalt + deviceID))
	return hex.EncodeToString(sum[:])
}

// mirasimMetaAccountUUID 给 account 段一个账号级稳定的 UUID。
//
// 为什么不保留客户端原值、也不留空：**presence 本身会随客户变**。一个走 OAuth 登录的
// Claude Code 送来真 uuid，一个走 api key 的送来空串（legacy 正则里那个 `*` 就是为了
// 容纳空串才写的），保留任何一种都会让同一个账号的 user_id 随客户变形，反关联当场失效。
//
// 为什么种子取 DeviceID 而不是 prepared.AccountSub（上游账号的 JWT sub，语义上更像
// "账号"）：AccountSub 可以为空（token 不是 JWT / 没有 sub 声明时 jwtClaimString 返回
// 空串），空了就又回到"presence 随状态变"。DeviceID 恒非空且与账号 1:1。
func mirasimMetaAccountUUID(deviceID string) string {
	return uuidV4Shaped(sha256.Sum256([]byte(mirasimMetaAccountSalt + deviceID)))
}

// mirasimMetaSessionUUID 把账号级稳定的 mirasim_session_id 映射成 UUID 形态。
//
// 真源仍然是 accounts.extra.mirasim_session_id（mirasim.ExtraSessionID），这里只换形态：
// 它的原值是 randomPrefixedID("session_")，即 "session_" + 24 hex，而 metadata.user_id
// 的 session 段在真实 Claude Code 里是 UUID v4（legacy 正则也要求 36 字符）。带前缀的
// 原值塞进去就是一个"这个客户端我没见过"的形态信号，所以做一次确定性映射，而**不是**
// 再铸一个新的、需要单独持久化的值。
func mirasimMetaSessionUUID(sessionID string) string {
	return uuidV4Shaped(sha256.Sum256([]byte(mirasimMetaSessionSalt + sessionID)))
}

// uuidV4Shaped 把一个 sha256 摘要格式化成 UUID v4 的形状（含 version/variant 位）。
func uuidV4Shaped(sum [32]byte) string {
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// mirasimCanonicalUserID 组装这个账号该对上游声称的 metadata.user_id。
// 返回空串表示算不出（缺设备身份），调用方据此放弃改写。
func mirasimCanonicalUserID(prepared mirasim.Prepared) string {
	if prepared.Signer == nil || prepared.Signer.DeviceID == "" {
		return ""
	}
	deviceID := prepared.Signer.DeviceID

	sessionSeed := prepared.SessionID
	if sessionSeed == "" {
		// 不可达：mirasim.Credential.ensureSessionIDLocked 保证 Prepare 返回时它非空。
		// 真的空了也**不能**把客户端原值放过去（那正是本文件要堵的泄漏），退回设备根
		// 派生 —— 仍然是账号级稳定值，fail-closed。
		sessionSeed = deviceID
	}

	// 输出形态（JSON 对象 vs legacy 拼接串）由客户端版本决定
	// （service.NewMetadataFormatMinVersion = 2.1.78）。这里刻意取 **canonical UA 里的
	// 版本**，既不取客户端送来的版本，也不从 req.Header 回读：
	//   - 取客户端版本 → body 形态随客户变（同一台设备今天发 JSON、明天发拼接串）；
	//   - 从 req.Header 回读 → 网关用 setHeaderRaw 写的是小写 "user-agent"，而
	//     ApplyCanonicalIdentityHeaders 用 h.Set 写的是 "User-Agent"，两份共存时
	//     可能读到不是要发出去的那一份。
	// 从同一份 canonical 定义取值，"body 声称的版本 == 头声称的版本"就是结构上成立的。
	version := service.ExtractCLIVersion(mirasim.CanonicalIdentityHeaders()["User-Agent"])

	return service.FormatMetadataUserID(
		mirasimMetaDeviceID(deviceID),
		mirasimMetaAccountUUID(deviceID),
		mirasimMetaSessionUUID(sessionSeed),
		version,
	)
}

// applyMirasimCanonicalUserID 把 body 里的 metadata.user_id 收敛成账号级稳定身份，
// 并把改写后的字节重新装回 req（Body / ContentLength / GetBody 三处一起，漏掉任何一处
// 都会让上线字节与被签名的字节不一致）。返回最终要签名的 body。
//
// **必须在 mirasim.SignAndSeal 之前调用**；返回值就是要交给 SignAndSeal 的那一份。
// 调用方只保留一个 body 变量，改写与签名之间没有第二个副本可以走散。
//
// 三条"不凭空造"的边界 —— metadata 不存在 / 不是对象 / user_id 缺失或非字符串时一律
// 原样返回，连 req 都不碰。客户端没发这个字段就不该让它出现：凭空注入一个 user_id
// 本身就是指纹（真实 Claude Code 在没有会话上下文时也不发 metadata）。
//
// 命中时是**无条件整段替换**，不先 ParseMetadataUserID 成功才替换。客户端送来的原值
// 对本 lane 毫无用处，而"解析失败就原样透传"会留一个逃生口：畸形原值照样把客户端的
// 真 device id 顶到上游。
func applyMirasimCanonicalUserID(req *http.Request, body []byte, prepared mirasim.Prepared) []byte {
	if len(body) == 0 {
		return body
	}
	metadata := gjson.GetBytes(body, "metadata")
	if !metadata.Exists() || metadata.Type == gjson.Null {
		return body
	}
	if !strings.HasPrefix(strings.TrimSpace(metadata.Raw), "{") {
		return body
	}
	userID := metadata.Get("user_id")
	if !userID.Exists() || userID.Type != gjson.String || userID.String() == "" {
		return body
	}

	canonical := mirasimCanonicalUserID(prepared)
	if canonical == "" || canonical == userID.String() {
		return body
	}

	// sjson 做的是就地替换：body 其余字节（thinking 块、cache_control、键顺序、未转义的
	// < > &）逐字节保留。**绝不能**改成 json.Unmarshal + Marshal 往返 —— 那会重排键并
	// HTML 转义，毁掉上游的 prompt-cache 前缀（identity_service.go 的注释警告过同一件事，
	// 而 fingerprint.go 记着一个兄弟网关为此把 >90% 的缓存命中打到 0）。
	newBody, err := sjson.SetBytes(body, "metadata.user_id", canonical)
	if err != nil || len(newBody) == 0 {
		return body
	}

	replaceRequestBody(req, newBody)
	return newBody
}

// replaceRequestBody 把 b 装回 req。GetBody 必须一起换：drainRequestBody 装的那个
// 闭包捕获的是**改写前**的字节，transport 自己的重试会从 GetBody 取，留着就会把
// 未改写的 body 送上线，而签名覆盖的是改写后的那一份。
func replaceRequestBody(req *http.Request, b []byte) {
	if req == nil {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.ContentLength = int64(len(b))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}
