# 架构债:一个上游实体 ≠ 一个协议

**状态**:已识别,未实施。归属 `refactor/provider-abstraction` 分支。
**记录时间**:2026-09-16
**触发场景**:mirasim 接入时暴露;开第二个协议时**必须**解决。

---

## 一、问题陈述

sub2api 的核心假设是 **一个账号 = 一个协议**(`Account.Platform` 单值 string,
`backend/internal/service/account.go:27`)。

真实世界里这个假设不成立。以 mirasim 为例,**同一个上游账号**同时支持三个协议:

| 模型族 | 端点 | agent 标识 |
|---|---|---|
| claude | `/v1/messages` | `claude` |
| codex | `/v1/responses` | `codex` |
| kimi | `/v1/chat/completions` | `kimi` |

而这三个族**共享同一组配额计数器**(`5h` / `7d` / `7d_claude` / `7d_fable`)、
同一个设备身份(`DeviceSeed`)、同一个出口 IP。

按 sub2api 现有模型,这个账号必须**导入多条记录**,一条一个 platform。后果:

1. **配额状态分裂**。claude 侧把 `5h` 打满后,上游对该号的 codex 请求同样 429,
   但 codex 那条记录的状态是"健康",调度器会持续往一个已经没额度的号上撞。
2. **封禁状态分裂**。403 只会禁用被撞到的那一条,另一条继续给一个已死的号送请求。
3. **身份重复**。同一个 `DeviceSeed` 存在多份,任一份被改写就产生分歧。
4. **用量统计分裂**。后台永远看不到这个号的真实总消耗。

这不是"统计稍微不准",是**调度器持有错误的世界模型**。

## 二、为什么不能简单改 `Platform` 为多值

耦合规模(2026-09-16 实测):

| 指标 | 数量 |
|---|---|
| `.Platform` 引用点 | **973** |
| 按 platform 分派/筛选(`Platform ==` / `switch`) | **447** |
| platform 常量 | 11(`domain/constants.go:21-34`) |
| DB CHECK 约束 | `user_platform_quotas.platform`、`composite_model_routes.target_platform`(`migrations/237`) |

把 `Platform` 改成数组会触及全部 973 处,且 DB 约束要重建。**这条路不可行。**
正确方向是**在 `Platform` 之上加一层**,让它保持单值语义不变。

## 三、已有的相关机制(可借鉴,但不解决本问题)

**`composite` 平台**(`domain/constants.go:34`、`handler/composite_platform.go`、
`migrations/172_composite_model_routes.sql`):

- `composite_model_routes(model, match_type, endpoint, target_platform)`
  按模型名把请求路由到某个 `target_platform`
- `DetectModelPlatform(model)` 从模型名推断 platform
- `WithResolvedTargetPlatform(ctx, ...)` 把解析结果放进 ctx,
  `effectiveAPIKeyPlatform` 优先读它

**这解决的是入口侧**:一个 composite 分组的 key 可以访问多个 platform 的模型。
**但路由终点仍是"某个 platform 的账号池"**,账号本身依旧单协议。与本问题正交。

值得复用的是它已有的 `endpoint` 枚举:
`'any','messages','count_tokens','responses','chat_completions','embeddings','images','gemini'`
—— "模型 → 端点"的映射概念已经存在,只是没有下沉到账号层。

## 四、正确的分层

现在 `Platform` 一个字段同时承担了至少五种职责:

1. 路由(走哪个端点、哪套转发逻辑)
2. 调度(按 platform 筛账号池)
3. 限流语义(不同 platform 的状态码含义不同)
4. 计费(模型定价表归属)
5. 用量统计维度

这五种里,**只有 1 和 3 真正属于"协议"**;2、4、5 属于"上游实体"。混在一起才导致
"一个实体多协议"无法表达。

拆成两个正交概念:

```
Account(上游实体)        —— 一个号 = 一条记录
  ├─ 凭据 / token
  ├─ 配额状态(mirasim 是四层窗口)
  ├─ 出口 IP / proxy
  ├─ 设备身份(DeviceSeed / SessionID)
  └─ 封禁状态

Capability(能力)         —— 一个 Account 可以有 N 个
  ├─ protocol(anthropic / openai-responses / openai-chat / ...)
  ├─ 支持的模型集
  └─ 端点路径
```

调度顺序随之变成:
**先按 (protocol, model) 筛出具备该能力的 Account,再在其中按配额/粘性选号。**
配额与封禁状态挂在 Account 上,天然跨协议共享。

## 五、增量迁移路径(不动那 973 处)

1. **保留 `Account.Platform` 作为"主协议"**,语义不变,现有代码全部照跑。
2. **新增 `account_capabilities` 表**:`(account_id, protocol, model_pattern, endpoint)`。
   存量账号迁移时,为每条记录生成一条与其 `Platform` 对应的 capability —— **行为零变化**。
3. **调度入口加一层筛选**:选号前先按 `(protocol, model)` 过 capability 表。
   只有一条 capability 的账号(即今天所有账号)筛选结果与现在完全相同。
4. **配额/封禁状态改挂 Account**(而非 platform-scoped 记录)。这是唯一有行为变化的一步,
   需要单独验证。
5. 多协议账号(mirasim)此时才真正启用:一条 Account + 三条 Capability。

前四步都是**可独立上线、行为不变**的。风险集中在第 4 步。

## 六、什么时候必须做

- **现在不做**:mirasim 短期只开 claude 族(`platform=anthropic` + `extra.mirasim=true`),
  单协议,不触发本问题。
- **开第二个协议时必须做**:一旦要同时开 mirasim 的 codex 或 kimi,
  导入两遍带来的状态分裂会立刻变成真实的调度错误(见第一节 1-4)。
- **接入下一个多协议渠道时同样必须做**:这不是 mirasim 特有的形状,
  任何"一个号支持多种模型/协议"的上游都会撞上。

## 七、与 mirasim 特化的关系

mirasim 分支(`feat/mirasim-provider`)**不解决**本问题,只是把四层配额模型**建对**,
使得将来启用多协议时是"加一条端点分流",而不是"重做配额模型"。

参见 `ACCEPTANCE-mirasim.md` 的 H 段。
