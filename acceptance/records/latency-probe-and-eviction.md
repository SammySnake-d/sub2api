# latency-probe-and-eviction

状态: **通过**

判定日期: 2026-09-16
判定人: snakesammy（取证由 Claude Opus 5 在生产实例上执行）

## 核对内容

(1) `latency_test_url` 是轻量 204 探针，上游域名只出现在 `latency_authorities`；
(2) 熔断演练：延迟超阈值的节点被熔断、挂在它上面的 sticky 账号自动迁走。

## 第一条：探测靶子

已从人判降级为**机器可判** —— 配置真源进了仓库（`deploy/resin-runtime.json`），
由 `backend/internal/service/mirasim_deploy_resin_config_test.go` 断言。
线上值由 `deploy/apply-resin-runtime.sh` 推送并**回读校验**（18 项一致）。

这条曾经配错过，代价记在那份 json 的 `_rationale` 里：上游域名被填进
`latency_test_url`，该域名对裸 GET 不返回可用响应 → 每个节点的延迟探测全部超时 →
所有节点看起来都极慢 → P2C 选点退化成随机，实测一次请求 3s → 170s。

## 第二条：熔断迁移演练（真机，不是构造的）

取一个**真实处在熔断状态**的节点做演练，而不是人为制造一个：

```
账号 mirasim-102 演练前的租约：
  node_hash = aecb6ceb250393363d352d556b5478d2
  node_tag  = monosans-http/http-181.78.74.252:999
  egress_ip = 181.78.23.187

该节点的健康状态：
  circuit_open_since   = 2026-09-16T03:06:47.636998253Z
  reference_latency_ms = 1518        ← 超过 max_routable_latency_ms=1000
```

经 `Default.mirasim-102` 发一次真实请求后：

```
账号 mirasim-102 演练后的租约：
  node_hash = 79cdac0f20b3135e80342eb1bb48796b
  node_tag  = thespeedx5/http-84.36.141.180:1976
  egress_ip = 84.36.141.180          ← 换了节点，也换了出口 IP
```

延迟 1518ms 超过 1000ms 上限 → 断路器打开 → 该节点被移出可路由视图 →
下一次请求的粘性租约自动迁到另一个节点。三段都有实测值，不是「符合预期」。

## 旁证：清理是**持续**发生的，不是一次性的

对 143 个粘性身份连打两轮（每轮一次真实 CONNECT 到 relay.mirasim.ai）：

```
第一轮 失败 49/143
第二轮 失败 57/143
```

两轮的失败集合**几乎不重叠**（第一轮失败的 1/13/15/21/38/40/41/42/54/58/59/78/
80/83/84/86/88/89/95/113/121 在第二轮全部恢复）。这正是熔断在清理：每个坏节点被
淘汰后租约迁走，而订阅刷新又送进一批新的坏节点。

## 结论

两条判据均通过。

## 顺带查出的缺陷（已修）

主动延迟探测**只会** dial `latency_test_url`，从不主动打 authority 域名 ——
authority 延迟完全依赖真实流量。所以「能通 gstatic 但通不了上游」的节点会一直
留在可路由集里，只有真实请求踩到它才被发现，代价是赔掉调用方的请求。

resin 侧已修（单次请求内换节点重试 + 每次 dial 限时 8s）。同一口径实测：
坏身份率 **30% → 6.7%**（前 60 个身份 18 坏 → 4 坏）。

## 机器核不到的那一半

- 「阈值恢复后该节点重新进池」这一段没有当场演练：它要等该节点的下一次成功探测，
  周期最长 10 分钟。代码路径（`EnforceLatencyCeiling` 的注释与 `RecordResult`）
  是读过的，但**没有观测到一次真实的恢复**。这一条仍是推断。
