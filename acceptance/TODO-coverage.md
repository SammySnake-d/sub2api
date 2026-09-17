# 待写测试清单(由 obligations.generated.json 机器生成)

> **2026-09-17：`obligations.generated.json` 已从仓库删除**（连同 `calibration/`
> 与 `adequacy/` 两个目录），只保留 `ACCEPTANCE.yaml` 与本目录下的人工签字记录。
> 所以下面那段重新生成脚本当前跑不起来 —— 先跑 `acceptance-lint --expand`
> 把义务集重新展开出来。删除前的整套内容备份在仓库外：
> `~/.local/src/sub2api-acceptance-artifacts-20260917.tgz`

**这不是手写清单。** 它由 `ACCEPTANCE.yaml` 的 `structure_sources[]` 展开 43 条义务,
再按 `criteria[].covers[]` 映射到各自的测试文件。改结构源后重新生成,不要手改。

重新生成:
```bash
cd /Users/snakesammy/.local/src/sub2api
python3 ~/.claude/skills/acceptance-atlas/scripts/acceptance-lint.py --root . --expand
# 然后用本文件末尾的脚本重新映射
```

## 规则(否则标签不算数)

1. `[[cov:<义务ID>]]` 必须写在**测试函数体内**(注释或函数名),写在文件头部不算。
2. 该测试函数必须有**非平凡断言** —— 断言里要出现来自被测源码的符号,
   `assertEqual(len("abc"), 3)` 这类不算。
3. **一条断言填不实两格义务**:同一函数内的标签数不得超过非平凡断言数。
4. 确实不该测的,挂白名单代号豁免(带 `by`/`date`/`ref` + 反证),**不许静默跳过**。

## 当前状态

`|E| = 43`,已填实 `0`,空白领土 `43`。
移植 agent 已产出 `backend/internal/pkg/mirasim/`(含 `differential_test.go` +
`testdata/marelay_vectors.json`)与 `repository/mirasim_upstream_test.go`、`mirasim_live_test.go`,
**这些测试已存在但还没挂标签**,是最快能填实的一批。

---

## backend/internal/pkg/mirasim/ccore/sign_parity_test.go (2 条) ← C1
```
[[cov:SG:bytewise-parity]]      固定输入下五个 x-mirasim-* 头与 ma-relay 逐字节相同
[[cov:SG:body-sensitivity]]     body 改 1 bit 签名必须变化(防止签了常量)
```
注:`pkg/mirasim/differential_test.go` + `testdata/marelay_vectors.json` 很可能已经覆盖了这两条,
确认后把标签挂上去即可,不必新建文件 —— 若已在别的文件覆盖,改 `ACCEPTANCE.yaml` 的 `check` 指向它。

## backend/internal/repository/mirasim_upstream_test.go (5 条) ← C2
```
[[cov:SG:path-signed]]            改 path 上游拒绝(证明签名真在起作用)
[[cov:SG:query-not-signed]]       URL 追加 ?beta=true 后仍 200
[[cov:SG:no-post-sign-mutation]]  签名后 body sha256 与 header 快照不变
[[cov:SG:retry-resigns]]          重试产生全新请求并重新签名,不改写重发已签请求
[[cov:SG:no-redirect]]            禁止跟随重定向(Go 跳转换 path 必破签)
```

## backend/internal/service/mirasim_cache_prefix_test.go (6 条) ← C4/C5/C6
```
[[cov:CP:system-bytewise]]     出站 system 与入站逐字节相同,断点在原位
[[cov:CP:tools-bytewise]]      出站 tools 与入站逐字节相同(前缀最前一段)
[[cov:CP:prefix-monotonic]]    第 2 轮是第 1 轮的字节前缀(bytes.HasPrefix)
[[cov:CP:no-key-reorder]]      对象 key 顺序不变(map round-trip 会按字母序重排)
[[cov:CP:no-html-escape]]      < > & 不被转义成 < 等
[[cov:CP:bigint-precision]]    大整数不丢精度(12345678901234567890 不得变成 ...567000)
```

## backend/internal/service/mirasim_identity_test.go (7 条) ← C8/C9
```
[[cov:ID:deviceseed-stable]]            跨重启、跨客户端不变
[[cov:ID:deviceseed-db-backed]]         清空 Redis 后仍不变(必须来自 DB)
[[cov:ID:sessionid-stable]]             跨重启不变(否则整池同步轮换 session)
[[cov:ID:sessionid-distinct]]           账号间互不相同(防归一成常量)
[[cov:ID:version-client-independent]]   出站版本与入站客户端版本无关
[[cov:ID:version-coherent]]             UA 版本与 stainless 组属同一次实抓
[[cov:ID:mirasim-client-paired]]        x-mirasim-client 与签名算法版本配对
```

## backend/internal/service/mirasim_statuscode_test.go (11 条) ← C13/C14/C15
```
[[cov:SC:503-capacity]]     账号保持 schedulable,不写任何冷却
[[cov:SC:429-5h]]           写 5h scope;其他三个 scope 不受影响
[[cov:SC:429-7d]]           写 7d scope;5h 不受影响
[[cov:SC:429-7d-claude]]    只锁 claude 族,fable 仍可调度
[[cov:SC:429-7d-fable]]     只锁 fable 族,opus 仍可调度
[[cov:SC:403-banned]]       账号禁用(既定决策,测试把此行为固定住防重构误改)
[[cov:SC:400-badrequest]]   不污染仅 body 相同、header 不同的后续请求
[[cov:SC:401-expired]]      触发 refresh 并重试,不禁用账号
[[cov:SC:402-nobalance]]    本地拦截,不发上游
[[cov:SC:499-cancel]]       不重试到另一账号
[[cov:RG:existing-channels-unchanged]]  既有渠道冷却时长与禁用条件未变
```

## backend/internal/service/mirasim_quota_test.go (4 条) ← C11
```
[[cov:QU:opus-window-and]]     opus 可调度性 = 5h ∧ 7d ∧ 7d_claude
[[cov:QU:fable-window-and]]    fable 可调度性 = 5h ∧ 7d ∧ 7d_fable
[[cov:QU:family-isolation]]    7d_claude 耗尽时 opus 停调而 fable 仍可调度
[[cov:QU:single-quota-state]]  同一上游实体只有一套配额状态
```
实现提示:sub2api 已有 per-(account, scope) 的多槽冷却
(`accounts.extra->'model_rate_limits'->scope`,`service/model_rate_limit.go`),
且 Fable 7d_oi 已是族级 scope 的生产先例(`anthropicFableRateLimitKey`)。
四层窗口应映射成四个 scope,而非新建状态表。

## backend/internal/service/mirasim_billing_test.go (4 条) ← C18
```
[[cov:BL:cache-read-discount]]  cache_read 按 0.1x
[[cov:BL:cache-write-5m]]       5m TTL 的 cache_creation 按 1.25x
[[cov:BL:cache-write-1h]]       1h TTL 的 cache_creation 按 2x
[[cov:BL:no-price-fallback]]    每个启用模型都有显式定价,不 fallback
```

## backend/internal/handler/admin/account_mirasim_import_test.go (3 条) ← C16
```
[[cov:MG:per-account-signature]]  导入后逐账号签名差分,全部通过(不接受抽样)
[[cov:MG:idempotent]]             重复执行结果一致,不产生重复账号
[[cov:MG:source-preserved]]       ma-relay 侧原始配置未被修改或删除
```
模板:`internal/handler/admin/account_codex_import.go`(`CodexSessionImportRequest`
接受原始 JSON 批量导入,返回 created/updated/skipped/failed)。
`CreateAccountRequest.Credentials` 是 `map[string]any`,能直接装下 DeviceSeed/SessionID/at/rt。

## backend/internal/pkg/tlsfingerprint/mirasim_profile_test.go (1 条) ← C10
```
[[cov:ID:tls-ja3]]  JA3 == 71dc8c533dd919ae9f4963224a4ba8fd(Mirasim.app Electron)
                    不是 sub2api 现成的 44f88fca...(Claude Code)
```
来源:ma-relay `internal/relay/dialer.go:262 nodeClientHelloSpec()`。

---

## 人判项(机器只能核记录存在,核不了结论)

| 标准 | 记录位 | 内容 |
|---|---|---|
| C3 | `acceptance/records/live-request.md` | 真实账号 200 + 真实 content + 非零 usage |
| C7 | `acceptance/records/cache-hit-rate.md` | 生产命中率不低于 ma-relay 基准的 90% |
| C12 | `acceptance/records/quota-visibility.md` | 运维能看懂某号为何不可调度 |
| C17 | `acceptance/records/migration-signoff.md` | 导入数量与来源一致 |
| C19 | `acceptance/records/billing-reconciliation.md` | 记账与手算金额一致 |
| C20 | `acceptance/records/statuscode-semantics.md` | 状态码处理与 mirasim 真实语义一致 |

## 重新生成本清单的脚本

```python
import json, yaml, collections
obl = json.load(open('obligations.generated.json'))
acc = yaml.safe_load(open('ACCEPTANCE.yaml'))
ids = [o['id'] if isinstance(o, dict) else o for o in (obl.get('obligations') or obl.get('E') or [])]
owner = {ob: (c['id'], c['check']) for c in acc['criteria'] for ob in (c.get('covers') or [])}
byfile = collections.defaultdict(list)
for i in ids:
    cid, f = owner.get(i, ('(无人认领)', '(未指定)'))
    byfile[f].append((i, cid))
for f in sorted(byfile):
    print(f"## {f}   ({len(byfile[f])} 条)")
    for i, cid in sorted(byfile[f]):
        print(f"  [[cov:{i}]]   ← {cid}")
```
