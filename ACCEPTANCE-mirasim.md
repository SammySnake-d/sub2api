# mirasim-on-sub2api 验收标准

> **本文件是人读的背景、证据与 file:line 依据。**
> **机器判决的那份在 `ACCEPTANCE.yaml`**(43 条义务 + 20 条验收标准,
> 用 `acceptance-lint.py --root . --gate release` 判决),待写测试清单在
> `acceptance/TODO-coverage.md`。两份内容一致;有冲突时以 `ACCEPTANCE.yaml` 为准,
> 因为只有它会被执行。

判据先于实现。每条都要有**可测量的读数**,"看起来对"不算通过。
每条标注:**门**(不过不能上生产) / **观测**(记录读数,不阻塞)。

目标系统:`SammySnake-d/sub2api` 分支 `feat/mirasim-provider`
对照系统:`ma-relay` v0.18.0(生产,命中率 96.1%,可作为基准)

---

## A. 签名正确性 —— 错一个字节 = 全量 403

**A1【门】差分测试逐字节相同**
固定输入 (method, path, body, deviceSeed, ts, nonce, credential, clientVersion),
sub2api 新包与 ma-relay 原实现产出的 `x-mirasim-*` 五个头必须**逐字节相同**。
- 通过判据:`assert.Equal` 在 5 个头上全绿,无豁免。
- 若某字段无法对齐,必须**显式列出是哪个、为什么**,其余仍须逐字节相同。
- 反例保护:故意改 1 bit body → 断言签名**必须**变化(防止签了个常量)。

**A2【门】真实请求 200**
用真实 mirasim 账号,`claude-haiku-4-5`,`max_tokens<=16`,非流式:
- 通过判据:HTTP 200 且返回真实 content 且 usage 非零。
- 记录:request_id、耗时、token 用量。

**A3【门】签名覆盖面正确**
- 签 path **不签 query**:验证给 URL 追加 `?beta=true` 后仍 200(sub2api 确实会加)。
- 改 path 会导致 403:构造一次错误 path,断言上游拒绝(证明签名真在起作用,不是被忽略)。

**A4【门】签名后无二次改写**
`HTTPUpstream` 装饰器签名之后,到真正发出为止,body 与 header 不得再被任何代码改动。
- 通过判据:在装饰器内签名后记录 body 的 sha256 与 header 快照,在 transport 层(或用 httptrace/自定义 RoundTripper)再取一次,断言相同。
- **失败重试路径同样要验**:重试若复用同一个 `*http.Request` 并再改写,等价于同一个 bug。

---

## B. 出站身份归一 —— 范围已缩小:**版本归一,不做全覆盖**

**决策(运营者 2026-09-16)**:客户侧用分组的"仅允许 Claude Code 客户端"限制住入站形态,
因此**不需要**把任意 harness 改写成 CC(全覆盖)。但**必须做版本归一**,否则多号版本乱跳。
全覆盖(harness 无关化)留作后续讨论,不进本批次。

**现状(已查证,sub2api v0.2.5)—— 归一化零件齐全,但没接到 CC 路径上:**

```go
// gateway_forward.go:196
shouldMimicClaudeCode := account.IsOAuth() && !isClaudeCode
```
`isClaudeCode` = UA 匹配 `claude-cli/X.Y.Z` **且** `metadata.user_id` 可解析(AND)。
→ 真 CC 客户端 ⇒ `mimic=false` ⇒ 走 `allowedHeaders` 白名单**原样透传**,
而白名单(`gateway_service.go:427`)**包含** `user-agent` / 全部 `x-stainless-*` / `x-app` / `anthropic-version`。
**净效果:开了"仅 CC"之后,版本归一化恰好被关掉。**

已有可复用零件:`claude.CLIVersion()`(进程级解析一次 + `SUB2API_CLAUDE_CLI_VERSION` 只准向上覆盖)、
`claude.DefaultHeaders`、`applyClaudeCodeMimicHeaders()`、`syncBillingHeaderVersion()`(同步 body 内 `cc_version`)。

**B1【门】同一账号跨客户端版本恒定**
两个不同 CC 版本的客户端(如 2.1.250 与 2.1.272)先后打到**同一个 mirasim 账号**:
- 通过判据:两次**出站**的 `user-agent` 版本段、`x-stainless-package-version`、
  `x-stainless-runtime-version`、`x-stainless-os`、`x-stainless-arch` **完全一致**。
- 反例保护:断言出站版本**不等于**入站版本(证明确实归一了,而非碰巧同版本)。
- 特别注意**版本倒退**:真实设备版本单调递增,倒退比乱跳更刺眼。

**B2【门】归一化目标本身自洽(比"版本够新"更重要)**
sub2api 现有 `DefaultHeaders` 是一个**真实世界不存在的组合**:
| 头 | sub2api 现值 | 真实 CC 2.1.272(本机实抓) |
|---|---|---|
| UA 版本 | `CLICurrentVersion = 2.1.258` | `2.1.272` |
| `x-stainless-package-version` | `0.94.0` | `0.112.1` |
| `x-stainless-runtime-version` | `v24.3.0` | `v26.3.0` |
| UA 后缀 | `(external, cli)` | `(external, sdk-cli)` |
- 通过判据:出站指纹整套等于**同一次实抓**的自洽快照,不是各字段分别拍脑袋。
- 理由:乱跳还能解释成多设备;**不存在的版本组合只能解释成伪造**。

**B3【门】body 内版本与 header 一致**
`syncBillingHeaderVersion` 写进 body 的 `cc_version` 必须等于出站 UA 的版本段。
- 通过判据:同一请求的两处版本相等。

**B4【观测】版本自动同步**
sub2api 有 `openai_codex_version_sync_service.go`,claude 侧**无对位**(硬编码 + env 覆盖)。
- 读数:记录 `CLICurrentVersion` 与 npm `@anthropic-ai/claude-code` latest 的差距。
- 处置:先用 `SUB2API_CLAUDE_CLI_VERSION` 顶住;自动同步服务作为后续项(可移植 ma-relay `internal/fingerprint`)。

**B5【门】归一化不碰 body 前缀** —— 已由审计细化为 B5a~B5d(证据见下)

审计结论(带 file:line,2026-09-16):mimic 路径上"三段逐字节不变"**不可能成立**,
因为 `system` 本来就被整体替换。故拆成四条可机械判决的门。
**这也是选择 api_key + passthrough 落法、而非 mimic 的决定性依据。**

已查证的前缀改写源(按危险程度):
| 改写 | JSON 路径 | 触发条件 | 手法 |
|---|---|---|---|
| `rewriteSystemForNonClaudeCodeWithPromptBlocks`<br>`gateway_claude_oauth_body.go:903` | **`system` 整体替换** `:925`;原 system 塞进 `messages[0]/[1]` `:963-975` | mimic && `claude_oauth_system_prompt_injection`(**默认 true**) | 结构性重写 |
| `applyToolNameRewriteToBody`<br>`gateway_tool_rewrite.go:178` | **`tools[*].name`**、`tool_choice.name`、历史 `messages[*].content[*].name` | mimic,**无任何开关** | sjson |
| `filterThinkingBlocksInternal`<br>`gateway_request.go:1324` | **整个 body** | `gateway_forward.go:355`,对 `claude-*` 恒真 | **`map[string]any` round-trip** |
| `StripEmptyTextBlocks` `gateway_request.go:517` | messages 子树 | `gateway_forward.go:335` 无条件 | **map round-trip** |
| `FilterWebSearchHistoryBlocks` `gateway_websearch_block_filter.go:46` | messages 子树 | `gateway_forward.go:342` 无条件 | **map round-trip** |
| `syncBillingHeaderVersion` `gateway_billing_header.go:54` | **`system[i].text`**(非 metadata) | oauth+mimic,或 OAuth+有指纹 | sjson;会话内稳定 |
| `sanitizeAnthropicBodyForBetaTokens` 分支5<br>`gateway_request.go:1035` | **`messages[i].output_config`**;system 角色空消息整条删 `:1075` | 守门 token 缺失时 | `[]json.RawMessage` 重建 |
| `RewriteUserIDWithMasking` `identity_service.go:394` | `metadata.user_id` | OAuth | sjson ✅ **前缀外,安全** |

map round-trip 实测代价:顶层 key 字母序重排、对象内 key 重排、`<`→`<` HTML 转义、
**大整数精度丢失**(`12345678901234567890` → `...567000`,工具入参里出现即静默改值)。

**B5a【门】system 不被搬位、不被替换**
出站 `system` 仍是客户端原数组、`cache_control` 断点在原位、`messages` 逐字节不变。
唯一允许的差异:`system[i].text` 里 `cc_version=X.Y.Z[.fp]` 这一个子串(即归一化本身)。

**B5b【门】tools 逐字节不变**
出站 `$.tools` 与入站逐字节相同 → 必须绕过 `gateway_tool_rewrite.go:178` 与 `:261`。
`tools` 是缓存前缀**最前一段**,动它整条链全废。

**B5c【门】跨轮前缀单调 —— 最便宜、覆盖面最广,建议作为主判据**
同一会话连续两轮,第 2 轮出站 body 的 `tools`+`system`+`messages` 序列化结果必须是
第 1 轮的**字节前缀**(prefix-of),而**不是"相等"**。
这一条同时抓 HTML 转义、key 重排、断点漂移三类问题,且不需要枚举改写源。

**B5d【观测】beta 守门未被打穿**
`sanitizeAnthropicBodyForBetaTokens` 分支 5 的守门 token 是
`mid-conversation-output-config-2026-07-01`。它有两条被摘掉的真实路径:
①管理员 beta-policy `filter` 规则(`mergeAnthropicBetaDropping` `:465-468` **会剔掉 required 列表里的 token**);
②账号级 `header_override` 直接整体替换 `finalBetaHeader`(`gateway_upstream_request.go:111-113`)。
- 读数:出站 `finalBetaHeader` 是否含该 token;不含时告警。
- ⚠️ 方向待定:若 mirasim 上游**不认**该 beta,则需反过来主动 strip。
  定论方式:对 mirasim 实发一次带 message 级 `output_config` 的请求看返回码。

**B6【门】`x-mirasim-client` 与签名方案必须配对 —— 版本归一的第二层**
版本归一有**两层**,容易只看到第一层:

| 层 | 字段 | 要求 |
|---|---|---|
| harness | `claude-cli/<ver>` UA + x-stainless 组 | 全号统一,跟随 npm latest(B1/B2) |
| **mirasim app** | **`x-mirasim-client`** | 全号统一,**且与签名方案配对** |

第二层与签名**强耦合**:`x-mirasim-client` 的值被写进签名的规范串。
- ma-relay 现状:`0.0.307` + 旧签名方案。真实最新:`0.0.326` + `mrs-sig-v2`。
- **通过判据**:出站 `x-mirasim-client` 的值,与实际使用的签名算法版本**属于同一组合**。
- **反模式**:只把版本号 bump 到 0.0.326 而仍按旧方式签名 —— "声称新版却用旧签名"
  比单纯用旧版本号更容易被识别。两者必须整组迁移。
- 本批次决策:**沿用 ma-relay 现有的 (0.0.307, 旧签名) 组合**,不在移植中顺手升级。
  升级 `mrs-sig-v2` 是独立的后续项,需单独灰度。

**B7【观测】跨族头不泄漏**
出站不应携带 codex/openai 家族头(`openai-beta`/`originator`/`chatgpt-account-id`)。
- passthrough 的白名单本就不含这些,预期自动满足;记录一次读数确认。

---

## C. 缓存前缀稳定 —— 这是最贵的一条

**C1【门】body 逐字节透传(或改写确定性可证)**

**已查证的 passthrough 路径实际改写点**(`gateway_anthropic_passthrough.go`,按执行序):

| 段 | 位置 | 改什么 | 手法 | 真实触发? |
|---|---|---|---|---|
| `system` | — | **0 处** | — | ✅ 天然安全 |
| `tools` | `:302` `stripDeferredToolCacheControl`<br>(`gateway_tool_rewrite.go:309`) | 删 `$.tools[i].cache_control` | sjson 保序 | ⚠️ **会** —— Claude Code 确实发 `custom.defer_loading` |
| `messages` | **`:82` `StripEmptyTextBlocks`**<br>(`gateway_request.go:517`) | 删空 text 块 | ❗**`map[string]any` round-trip** | ⚠️ **会** —— tool_result 里常见 |
| `messages` | `:87` `FilterWebSearchHistoryBlocks` | 删 web-search 历史块 | map round-trip | ✅ 不开 emulation 即 `modified=false`,返回同一 slice |
| `messages` | `:324` sanitize 分支5 | strip `messages[i].output_config` | `[]json.RawMessage` 重建 | ✅ 五道短路;CC 自发 beta 与 body 自洽 |

**通过判据**:入站 body 与出站 body 逐字节相同;若有必要改写,必须是**保序的外科手术**,
并单独断言"除该字段外其余字节不变"。

**必改项(唯一)**:`StripEmptyTextBlocks` 的 map round-trip。
- 代价实测:顶层与对象内 key 按字母序重排、`<`→`<` HTML 转义、
  **大整数精度丢失**(`12345678901234567890` → `...567000`,工具入参里出现即静默改值)。
- **修法(仓内现成)**:用 `buildJSONArrayRaw`(`gateway_claude_oauth_body.go:92`,纯 `append`,
  不经 `json.Marshal`)—— 只重建被改动的那一条消息、其余保留原始 raw。
  这样是**真正字节稳定**,而非"降级成只剩空白压缩 + HTML 转义"
  (换成 `[]json.RawMessage` 重建只能做到后者,因为 `json.Marshal` 仍会 compact + escapeHTML)。

**待实测项**:`stripDeferredToolCacheControl` —— mirasim 上游是否也拒绝 deferred tool 上的
`cache_control`?若不拒绝,对 mirasim 跳过可让 `tools` 段**完全字节透传**。
`tools` 是缓存前缀最前一段,收益最大。
- 定论方式:对 mirasim 实发一个带 `custom.defer_loading:true` + `cache_control` 的 tool 看返回码。

**passthrough 的两条天然优势(已确认)**:
- `anthropic-beta` **不 Del 不重算**(`:336-346` 白名单原样透传)→ **C3 自动成立**。
- `x-claude-code-session-id` **透传** → **E1 会话粘性自动成立**(mimic 路径反而不透传)。

**passthrough 的一条既有缺口**:没有 `enforceCacheControlLimit`
(它在 `gateway_forward.go:264`,位于提前 return 之后)。客户端发 >4 个 `cache_control` 断点时
网关不兜底,直接被上游 400。既有行为,对 mirasim 同样成立,需知情。

**C2【门】`system` 不得被搬位**
- 通过判据:入站 `system` 是数组(带 `cache_control`)时,出站 `system` 仍是数组、断点仍在原位、`messages` **逐字节不变**。
- 背景:ma-relay 曾把 caller 的 system 拍进 `messages[0]`,导致 8% 请求从 >90% 命中掉到 0%,中位重建 87K token,最坏 470-760K / 105-222s TTFT → 客户端放弃 → 取消丢弃缓存写入 → 重试从零 → **活锁**。

**C3【门】`anthropic-beta` 与 body 一致**
sub2api 已知会 `Del` 掉再重算 beta。
- 通过判据:入站带 `effort` 字段 + `effort-2025-11-24` beta 的请求,出站 beta 仍包含 `effort-2025-11-24`,且上游返回 **200 而非 400**。
- 背景:beta 与 body 强耦合,不匹配时上游只回一句笼统的 "The request was rejected as invalid"。

**C4【门】连续请求真的命中**
同一会话连续发 3 次(每次间隔 <60s,前缀相同,仅追加一轮对话):
- 通过判据:第 2、3 次的 `cache_read_input_tokens > 0`,且命中率 **>90%**。
- 这是唯一能证明前面三条真的有效的端到端读数。

**C5【观测】对照基准**
同样的 3 次请求打 ma-relay v0.18.0,记录命中率。sub2api 侧不应显著低于它(当前基准 96.1%)。

---

## D. 状态码 → 调度 —— mirasim 语义能否被 sub2 正确消费

每条都要**构造或捕获真实上游响应**后观察 sub2api 的账号状态变化,不能只读代码推断。

**D1【门】503 `service_capacity_overloaded` 不冷却账号**
mirasim 语义:**该模型**此刻没容量,账号健康。
- 通过判据:收到该 503 后,账号仍 `schedulable=true`、无 `overload_until`;若通过 `temp_unschedulable_rules` 配了模型级规则,则只有 (账号,模型) 对被冷却,该账号对**其他模型**仍可调度。
- 读数:变更前后的账号状态字段快照。

**D2【门】429 分类正确**
- 5h / 7d 窗口耗尽:按窗口冷却,`anthropic-ratelimit-unified-*` 头被解析。
- 7d_oi:只锁 Fable 家族,不影响其他模型。
- **region / shared-quota 受限**(`shared_quota_unavailable`):sub2api 目前**无此维度**,会落到 5 秒兜底。
  - 通过判据:要么补上该维度,要么**显式记录**这是已知缺口及其影响(该号会被反复重试而非冷却)。

**D3【门】403 = 账号封禁 → 禁用(按既定决策保持)**
运营者已明确:403 即号被禁,应禁用。**不改这段逻辑。**
- 通过判据:403 后账号被禁用(符合预期),且该行为**被测试固定住**,防止后续重构误改。
- 【观测】记录一次 403 会牵连多少账号(`max_account_switches` 默认 10),供运营者知情。

**D4【门】400 不被错误缓存/连坐**
- 通过判据:因**请求头**导致的 400,不得污染"仅 body 相同"的后续请求。
- 背景:ma-relay 曾因缓存键只含 (session, body-hash) 不含 header,把一次 beta 头引起的 400 重放了整整 15 分钟 —— 一个窗口里 722 个 400 中有 352 个是本地重放,客户在根因修好后仍持续看到 400。

**D5【门】不回归其他渠道**
现有 8 条渠道(openai / grok / gemini / bedrock / antigravity / …)的状态码行为不得改变。
- 通过判据:相关既有测试仍绿;若有测试缺口,至少人工验证 openai 一条主路。

---

## E. 调度与会话粘性

**E1【门】同一会话稳定粘同一账号**
同一 `x-claude-code-session-id` 连发 5 次:
- 通过判据:5 次命中同一个上游账号(除非该账号中途不可用)。
- 附带验证 `x-claude-code-session-id` 确实穿透到达了我们的粘性逻辑。

**E2【门】pin 存活于重启**
sub2api 的粘性存 Redis(TTL 1h)。
- 通过判据:重启 sub2api 应用进程后,同一会话仍粘原账号。
- 背景:ma-relay 的粘性只在内存,今晚一次重启把所有老会话当新会话,直接导致 47-76 万 token 全量重建和活锁。

**E3【观测】粘性未命中时的落点**
sub2api 的 Layer 2 回退终点是 `selectByLRU`(最久未用 = 最冷的号)。
- 读数:记录未命中回退发生的频率,以及回退后首次请求的 `cache_creation_input_tokens`。
- 判断:若频率低(因为 Redis pin 1h > Anthropic 缓存 5min,缓存热时 pin 必在),可暂不改;若读数显示频繁全量重建,再移植 ma-relay 的 warmth-preserving reseed。

**E4【门】一号一 IP 生效**
每个 mirasim 账号绑定自己的出站代理。
- 通过判据:抽 10 个账号各发一次,出口 IP 两两不同,且无 CN/HK。

---

## G. 身份持久性 —— 一号一设备,跨迁移不变

**目标(运营者)**:打进同一个号的多个客户,在上游看来必须是**同一台设备的正常请求**。

**身份组的根是 `DeviceSeed`,不是 deviceID。**
ma-relay `internal/config/types.go:89 Credential` 里 deviceID 与 ed25519 私钥**都由 `DeviceSeed` 派生**。
只抄 deviceID 而丢种子 = 签不出正确签名。完整组:

| 字段 | 作用域 | 迁移要求 |
|---|---|---|
| `DeviceSeed` | **每号唯一,永不变** | 原样迁移(deviceID + 私钥的根) |
| `SessionID` | **每号唯一,永不变** | 原样迁移(见 G2,最易漏) |
| `AccessToken`/`RefreshToken`/`AuthBase`/`ExpiresAt` | 每号唯一 | 原样迁移 |
| `Proxy` | 每号唯一 | 原样迁移(固定出口 IP) |
| clientVersion + 指纹头组 | **全号统一** | 归一化,见 B 段 |

**G1【门】DeviceSeed 迁移后签名仍然有效**
- 通过判据:迁移前后对同一 (method, path, ts, nonce, body) 产出的 `x-mirasim-*` 逐字节相同(即 A1 的差分测试用**迁移后**的种子跑一遍)。
- 真实请求 200(A2 复用)。

**G2【门】SessionID 必须一起迁移 —— 最易漏的一条**
ma-relay `types.go:98-103` 的注释给了理由:不持久化时每个账号每次启动都新铸 session id,
**整个号池会同步轮换 session —— 这是任何一组独立客户端安装都不会产生的信号**。
- 通过判据:重启 sub2api 后,各账号的 `x-mirasim-session` fallback 与重启前相同。
- 反例保护:断言不同账号的 SessionID **互不相同**(防止统一成一个常量)。

**G3【门】身份存 DB,不只存 Redis**
sub2api 现有的 per-account 指纹(`identity_service.go:126 Fingerprint`)存在 `IdentityCache`(Redis),
且 `GetOrCreateFingerprint`(`:164`)在缓存缺失时**从下一个请求的 header 重新生成** ——
对上游而言等于这个号换了台设备。
- 通过判据:清空 Redis 后,mirasim 账号的 DeviceSeed/SessionID **不变**(必须来自 DB `account.extra`)。
- 这是相对 ma-relay(写配置文件)的真实降级点,必须在移植时补齐。

**G4【门】身份是主动指定,不是"捡第一个客户的"**
sub2api 的指纹语义是"第一个打进来的客户长什么样,这个号就永远长什么样"(`identity_service.go:164`)。
mirasim 要的是相反的:版本/指纹**全号统一且由我们指定**,设备身份**每号唯一且由我们生成**。
- 通过判据:两个不同 UA 的客户先后首次使用同一个新导入的账号,出站指纹相同且等于 canonical 值
  (与 B1 同一读数,此处强调"首次"这个边界)。

**G5【观测】不复用 sub2api 的 Fingerprint 结构**
`Fingerprint` 只有 ClientID + UA + 7 个 stainless,**没有 deviceID/私钥的位置**(为 Anthropic OAuth 设计)。
- 记录:mirasim identity 落在 `account.extra` 的哪些 key 下,以及与 `anthropic_passthrough` 的共存方式。

---

## H. 配额与调度 —— mirasim 的四层窗口(审计已定稿 2026-09-16)

mirasim 每个账号有**四个并存**的计数器:`5h` / `7d` / `7d_claude` / `7d_fable`。
**即使只开 claude 族,四个也全部生效**(fable 属于 claude 家族但走独立的 `7d_fable` 窗口):

| 请求模型 | 消耗 |
|---|---|
| `claude-opus-5` / sonnet / haiku | `5h` ∧ `7d` ∧ `7d_claude` |
| `claude-fable-5-1` | `5h` ∧ `7d` ∧ `7d_fable` |

### 落点已确定:四层全都有现成的地方放,**不需要新建状态表**

sub2api 有**两层**限流状态,粒度不同:

| 层 | 存储 | 粒度 | mirasim 用它装什么 |
|---|---|---|---|
| 账号级标量 | `accounts` 表列(`rate_limit_reset_at` / `overload_until` / `temp_unschedulable_until` / `session_window_*`) | per-account | **`5h`、`7d`**(全局窗口,账号级正好) |
| per-(account, scope) | `accounts.extra->'model_rate_limits'-><scope>` JSONB | 任意 scope 字符串 | **`7d_claude`、`7d_fable`** |

- scope 写入:`account_repo.go:2315-2365`(`jsonb_set` 到 `ARRAY['model_rate_limits', $1]`)
- scope 读取:`service/model_rate_limit.go:169-190`
- key 推导:`modelRateLimitKeysForRequest` — `model_rate_limit.go:65-95`(按 platform 分支)
- 可调度判定:`IsSchedulableForModelWithContext` — `antigravity_quota_scope.go:37-52`
  = `IsSchedulable()` **AND NOT** `isModelRateLimitedWithContext`
- **族级先例已在生产跑**:`anthropicFableRateLimitKey = "claude-fable-5"`(`model_rate_limit.go:16-18`),
  7d_oi 命中后只锁 Fable 家族,账号对其他模型保持可调度。

**多窗口 AND 语义天然成立**(`model_rate_limit.go:40-47`):
```go
for _, key := range a.modelRateLimitKeysForRequest(ctx, requestedModel) {
    if a.isRateLimitActiveForKey(key) { return true }   // 任一 scope 命中即不可调度
}
```
scope 是任意字符串、无数量上限,写入侧唯一校验是非空(`account_repo.go:2316-2318`)。

**实现要点(与我早先的说法不同,以此为准)**:
`PlatformMirasim` **不存在**。mirasim 账号是 `platform=anthropic` + `credentials.provider=mirasim`
(`repository/mirasim_upstream.go:218-223`)。所以分支写在 `case PlatformAnthropic:` **内部按凭据标记分叉**,
不是新增 platform 常量(后者会牵动 `isMirasimAccount`、`isAllowedSchedulingThresholdPlatform`
(`account_scheduling_threshold_eval.go:44`)等一串白名单)。

**H1【门】可调度性是多窗口 AND**
- 通过判据:某账号 `7d_claude` 耗尽但 `7d_fable` 有余量时,opus 请求不选它、fable 请求仍可选它。

**H2【门】同一上游实体只有一套配额状态**
`Account.Platform` 是**单值 string**(`service/account.go:27`),同一个 mirasim 号若同时服务
claude 与 codex,按现有模型需**导入两条记录**。审计已列出这样做的具体故障(择要):
- **装饰器静默跳过**:codex 那条必然是 `platform=openai`,`isMirasimAccount` 返回 false,
  `mirasim_upstream.go:100-102` 直接 `return nil` → 请求裸发上游 → 403。**这个错误发生在配额问题之前。**
- **refresh token 互踢**:`Registry.creds` 是 `map[int64]*Credential`(`runtime.go:135`)按 accountID 分槽,
  两行 = 两条独立刷新时间线;上游轮转 refresh_token 后(`runtime.go:341-344` 已处理该情形),
  A 行刷新完 B 行手里的即失效。
- **两个 device 身份绑到一个上游账号**:seed 是 per-account 持久身份,两行 = 两个 device。
- **`DuplicateAccount` 保证分裂**:`admin_account.go:117-121` **显式丢弃** `model_rate_limits`
  与全部 `passive_usage_*`(注释:"must start fresh"),且不能改 platform(`:304`)。
- **5h/7d 恰恰落在唯一没有 scope 的路径上**:`persistAnthropicExhaustedWindowLimit`
  (`ratelimit_service.go:1459-1497`)走账号级 `SetRateLimited`,最该跨行同步的全局窗口反而无处同步。

- **当前决策**:短期只开 claude 族,一个号一条记录,不触发上述任何一条。
  多协议是架构债,见 `ARCH-multi-protocol-account.md`。

**H3【观测】主动读数 vs 被动 429**
- sub2api 对 Anthropic **没有**独立的定时配额轮询。利用率只有两条入口:
  被动响应头采样(`ratelimit_service.go:2034-2078`),或运营者配了 quota 模式的 channel monitor
  (`channel_monitor_quota_fetcher.go`,最小间隔 15s,写 `passive_usage_*` —— 正是阈值暂停读的键)。
- 调度**没有连续量**:anthropic 排序链是
  `filterByMinPriority` → `filterBySoonestReset`(默认关) → `filterByMinLoadRate` → `selectByLRU`
  (`gateway_scheduling.go:737-748`),**排序键里没有任何配额余量项**。
  利用率只作二元悬崖(`usedPercent >= threshold` 整号暂停)。
- OpenAI 侧**有**现成打分器 `quotaHeadroomFactor = 1 - clamp01(7d_used/100)`
  (`openai_account_scheduler.go:2951-2965`),但默认权重 **0**(`setting_parse.go:1062`)。
- 读数:mirasim 可主动查 `/v1/limits` 拿四个百分比。若要"优先选余量多的号",
  可参照 OpenAI 那套加权,而不是从零做。

**H4【观测】窗口累计用量查询已有**
`GetAccountWindowStats(ctx, accountID, startTime)` — `usage_log_repo_stats.go:308-335`,
SQL 仅 `WHERE account_id=$1 AND created_at>=$2`,**窗口是参数**,调用方传 `now-D` 即滚动窗口。
per-(account, model) 版本也存在:`GetModelStatsWithFilters` — `usage_log_repo_trend.go:432`,起止都是参数。
- 注意:`usage_logs` 是逐请求 append-only 行,**没有 per-(account,model) 聚合表**,全靠查询时 `GROUP BY`;
  三套 rollup 都不含 account 维度。高频查询需自行评估成本。

**H5【观测】字段命名对照**
上游 Anthropic 已有 `seven_day_sonnet`(`account_usage_service.go:188`),是**最接近 `7d_claude` 的既有字段**,
但目前只是展示字段,**没有**对应的冷却 scope。`7d_claude` 字面量全仓零命中。
- 处置:mirasim 的 scope 命名不要与之混淆,建议显式前缀。

## I. 传输层身份与连接 —— HTTP 头之下的那一层

**I1【门】TLS 指纹是 Mirasim.app 的,不是 Claude Code 的**
sub2api **已有** utls 基础设施(`internal/pkg/tlsfingerprint/dialer.go`,
`DoWithTLS(req, proxyURL, accountID, concurrency, profile)` 签名已带 profile 参数),
但现成 profile 是错的那一个:

| | JA3 hash | 模拟对象 |
|---|---|---|
| sub2api 现成 | `44f88fca027f27bab4bb08d4af15f23e` | Node.js / **Claude Code** |
| mirasim 需要 | `71dc8c533dd919ae9f4963224a4ba8fd` | **Mirasim.app** Electron 33.4.11 / Node 20.18.3 (BoringSSL) |

- 通过判据:对 JA3 回显服务发一次请求,拿到的 hash == `71dc8c53…`。
- 来源:ma-relay `internal/relay/dialer.go:262 nodeClientHelloSpec()` —— 实测抓取,
  密码套件顺序、10 个扩展的排列(无 ALPN)、曲线列表(X25519/P256/P384)全部对齐,直接搬。
- **为什么是门而不是观测**:TLS 握手是**第一个包**。上游在读到任何 header 之前
  就已经能判定"这个客户端不是 Mirasim.app"。头层画像做得再完美也被它一票否决。
- 注意 `ForceAttemptHTTP2: false`(ALPN 留空 → HTTP/1.1),这也是 Electron 行为的一部分。

**I2【门】非流式长生成不被超时杀掉**
sub2api 默认 `defaultResponseHeaderTimeout = 300s`(`repository/http_upstream.go:57`)。
- 背景:非流式请求的响应头**要等整个生成结束才到**。ma-relay 在 45s 时实测
  "一个 `max_tokens=8000` 长文请求连烧 5 个账号,每个都在恰好 45s 超时",
  且从头到尾没有任何一次有成功的可能。现值为 10 分钟。
- 通过判据:一个 `max_tokens>=8000` 的非流式请求能正常完成,不被 transport 截断。
- 配置位:`Gateway.ResponseHeaderTimeout`(`http_upstream.go:1298`),
  仓内已有 per-platform 覆盖先例(`OpenAIResponseHeaderTimeout` / `GrokResponseHeaderTimeout`)。

**I3【门】一号一 IP,且排除 CN/HK 出口**
- 通过判据:抽 10 个账号各发一次,出口 IP 两两不同,且无 CN/HK 归属。(与 E4 同一读数。)

**I4【观测】per-account 并发闸**
sub2api **已有**(`AcquireAccountSlotWithWaitTimeout` / `IncrementAccountWaitCount`,
`handler/gateway_handler.go:403-435`)。ma-relay 侧对应 `Credential.MaxConcurrent`。
- 读数:确认 mirasim 账号的并发上限被正确配置,不用新建机制。

---

## J. 凭据生命周期与数据迁移

**J1【门】token 自动刷新 —— 不做会集体 401**
mirasim 的 `AccessToken` 带 `ExpiresAt`,refresh 走它自己的 `AuthBase`,
**与 sub2api 现有的 Anthropic OAuth refresh 协议不通用**。
- 通过判据:构造一个即将过期的凭据,断言在过期前被自动刷新,且刷新后请求仍 200。
- 反例保护:刷新失败时账号应进入可识别状态,而不是静默持续 401。

**J2【门】迁移后逐号签名校验 —— DeviceSeed 错一字节该号即废**
165 个账号的 `(DeviceSeed, SessionID, AccessToken, RefreshToken, AuthBase, ExpiresAt, Proxy)`
需要导入器迁移。`DeviceSeed` 错误的表现**不是报错而是 403**,看起来像被封号,极难归因。
- 通过判据:导入完成后,**对每一个账号**跑一次 A1 的签名差分测试,全绿才算迁移成功。
- 不接受抽样:签名逐号独立,抽样过了不代表其余的过。

**J3【门】迁移可回滚且不丢数据**
- 通过判据:迁移脚本幂等可重复执行,且保留 ma-relay 侧原始配置不动(运营者长期约束:不删备份)。

---

## K. 计费口径

**K1【门】cache 折扣算对**
Anthropic 计价:cache read = 0.1×,cache write = 1.25×(5m TTL)/ 2×(1h TTL)。
- 通过判据:构造一次已知 usage 的请求,手算金额与系统记账一致。
- **方向性风险**:折扣算反时,缓存命中率越高亏得越多 —— 而我们正在优化命中率,两者叠加放大损失。

**K2【门】mirasim 定价表进 pricing**
- 通过判据:每个已启用模型都有定价条目,无 fallback 到默认价的情况。

**K3【门】余额熔断**
ma-relay 侧已实现按 token 计价 + 余额耗尽 402。
- 通过判据:余额为零的 key 被本地拦在 402,**不发出上游请求**。
- 背景:曾因此把"客户零余额"误读成"400 基线为 0",做过一次错误归因。

**K4【观测】客户端中途断开的记账**
流式请求中途 499 时已产生的 token 如何记账 —— 记录当前行为,由运营者决定口径。

---

## L. 其余必须覆盖项

**L1【门】严格模型白名单**
只有支持的模型可用,异类 model id 直接拦下(ma-relay `internal/relay/catalog.go` 已实现)。
- 通过判据:不在白名单的 model id 被本地拒绝,不发出上游请求。

**L2【门】499 取消不放大**
客户端取消后,网关**不得**把该请求重试到另一个账号。
- 背景:ma-relay 活锁链条的一环 —— 取消丢弃 in-flight 的 cache write,
  重试从零重建,叠加成 23 次连续 ~50s 取消。

**L3【观测】关键指标可见**
缓存命中率、400 率、503 率、P50/P95 TTFT 必须能在后台看到,否则 M3 无法执行。

---

## N. 部署与出口代理池(腾讯云 124.221.206.59 / agcn)

**背景更正(实测 2026-09-16)**:该 VPS 到上游的网络是**通的** ——
`mirasim-relay.mirofish.ai` 返回 404 / 0.85s,`api.anthropic.com` 返回 403 / 0.45s。
所以"mira 无法直连"的真实含义是**国内出口 IP 不可用于打上游**(关联/风控),不是网络不可达。
resin 的角色因此是**净化并固定出口**,不是打洞。这个区别影响判据:
可达性不是验收项,**出口 IP 的归属与稳定性**才是。

**现状**:x86_64 / 3.7G 内存(约 3G 可用)/ 51G 空闲;已跑 `caddy`(80,443)、
`antigravity-tools`(8045)、`ma-relay-worker-agcn`(无状态执行 worker,`MemoryMax=768M`)。
**无 PostgreSQL、无 Redis、无 resin、无 docker** —— 全部需要新装。

**N1【门】出口 IP 不含中国大陆与香港**
- 通过判据:抽 10 个账号各发一次真实请求,记录出口 IP 的 ASN 与国家,**无 CN、无 HK**。
- 反例保护:断言出口 IP **不等于** VPS 自身的公网 IP(124.221.206.59)——
  这条抓的是"代理没生效而请求直接裸奔出去",那种情况下功能测试全绿而出口是错的。

**N2【门】一号一 IP,且每 IP 承载 2-3 个号**
- 通过判据:N 个账号的出口 IP 分布满足:同一 IP 上的账号数 **≤ 3**;
  且同一账号在连续 10 次请求内**始终**是同一个出口 IP。
- 实现:resin fork 的 `max_leases_per_ip`(`SammySnake-d/Resin` 分支
  `feat/latency-circuit-breaker`),生产设 **3**(ma-relay 侧用的是 6)。
  机制是 `internal/routing/random.go calculateScore` 的 `overCapLeasePenalty=1e15` ——
  超 cap 加罚分,主导任何延迟项但**有限**,所以满了会 fallback 而不是硬失败。

**N3【门】按真实上游延迟选节点,不是按探测默认目标**
- 通过判据:resin 的 `latency_test_url` 指向**真实上游**(`mirasim-relay.mirofish.ai`),
  而非 `api.anthropic.com` 或任何默认值。
- 背景(已踩过):上游是 `mirasim-relay.mirofish.ai`(CloudFront 13.249.182.79),
  **不是 api.anthropic.com**。测错目标会让 `PREFER_LOW_LATENCY` 按无关延迟排序,
  等于没开。直连上游约 92ms;好节点 +170ms 可接受,中等节点 +1s 能感觉到。

**N4【门】延迟劣化时自动切走,不需要人介入**
- 通过判据:把 `max_routable_latency_ms` 临时设到一个极小值(如 1ms),
  断言节点被熔断、sticky 账号**自动迁移**到其他节点;恢复阈值后节点自动回到可路由集。
  (ma-relay 侧实测过双向:阈值 1ms → 10 节点全熔断;100s → 全恢复。)
- 生产值:`max_routable_latency_ms = 1000`。
- 机制:`internal/probe/manager.go performLatencyProbe` 成功后调
  `pool.EnforceLatencyCeiling(hash, domain)`,读 LatencyTable 的 EWMA 比阈值,
  CompareAndSwap 开熔断;低于阈值时下次探测自动恢复。
- **这一条解决的正是"纯 sticky 租到慢节点后卡死"** —— 没有它,一个账号会一直粘在劣化的节点上。

**N5【门】sticky 不靠时间过期**
- 通过判据:`sticky_ttl` 设长(ma-relay 侧用 168h),账号迁移**只由延迟熔断触发**,
  不由 TTL 到期触发。
- 理由:TTL 到期换 IP 对上游就是"这台设备换了网络",而延迟熔断换 IP 是有因由的;
  前者随机发生在所有账号上,后者只发生在真的变慢的那条线路上。

**N6【门】代理鉴权用对头**
- 通过判据:账号绑定的 proxy URL 形如
  `http://<Platform>.<Account>:<PROXY_TOKEN>@127.0.0.1:2260`,HTTPS 经 CONNECT 时
  账号从 **proxy-auth 用户名**取。
- 反例保护:构造一次用 `Authorization` 头而非 `Proxy-Authorization` 的请求,断言得到 407。
- 背景(ma-relay 踩过并已修):`SetBasicAuth` 设的是 `Authorization`,
  而 CONNECT 代理认证要 `Proxy-Authorization` → 407 会挡死整个代理集成。

**N7【门】sub2api 的依赖就位且不挤垮机器**
- 通过判据:PostgreSQL + Redis + sub2api + resin 四者同时运行时,
  `MemoryAvailable` 仍 > 500M,且 `ma-relay-worker-agcn` 未被 OOM killer 影响。
- 背景:总内存 3.7G,已有 worker 占用上限 768M。PostgreSQL 默认配置对这个规模偏大,
  需要调 `shared_buffers` / `max_connections`。

**N8【门】不影响既有服务**
- 通过判据:部署前后 `caddy`、`antigravity-tools`、`ma-relay-worker-agcn`
  三个服务的 `NRestarts` 不变,端口 80/443/8045 行为不变。
- **ma-relay 生产在另一台机器(45.205.28.160),本次部署完全不碰它。**

**N9【观测】节点源质量**
- 免费节点池会脏,**脏 IP 会杀号**。上生产前确认用的是付费源或已验证源。
- 读数:记录当前订阅源、健康节点数、其中非 CN/HK 的比例。

**M1** A~E、G~L 的全部【门】项通过。
**M2** `backend/` `go build ./...` 通过,新增包的测试全绿,未破坏既有构建。
**M3** 与 ma-relay 并行跑同样流量做对比,至少观测:命中率、400 率、503 率、P50/P95 TTFT。
  - 通过判据:命中率不低于 ma-relay 基准的 90%(即 >86%),400 率不高于基准。
**M4** 回滚路径明确:切回 ma-relay 的具体步骤 + 预计耗时,且**演练过一次**。
**M5** 单机唯一性:余额与账本是单进程状态,**任何时刻只允许一套在服务同一批客户**,否则算错钱。
  双跑必然重复计费或漏计费,这是硬约束不是建议。

---

## 已知缺口登记(不阻塞,但必须知情)

| 缺口 | 影响 | 处置 |
|---|---|---|
| sub2api 无 region/shared-quota 429 维度 | 该类 429 落 5 秒兜底,反复重试 | D2 记录;择期补 |
| sub2api 粘性未命中回退到 LRU | 冷号重建 | E3 观测后再定 |
| sub2api 无跨账号 400 确认(ma-relay 需 2 个账号确认) | 单账号异常可能被当成请求错误 | D4 覆盖;择期补 |
| sub2api 无 claude 版本自动同步 | 指纹陈旧 → 被上游降级 | B2 移植 `internal/fingerprint` 解决 |
| sub2api 非插件式 provider 抽象 | 未来非 Anthropic 协议渠道仍需平行路径 | 留给 `refactor/provider-abstraction` |
