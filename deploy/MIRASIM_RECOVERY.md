# Mirasim 故障恢复与计费

恢复策略覆盖 Anthropic Messages、Responses、Chat Completions。收到可换号的
502/503/504/529 时立即遍历尚未尝试的合规账号；整轮耗尽才指数退避。
默认直到成功或客户端取消，没有固定 10 次或 90 秒上限。
流式等待每 10 秒发送 SSE 注释保活，成功输出后不拼接其他账号的流。
所有上游持续不可用、客户端/反向代理超时及已输出后的中断仍可能失败。

## 配置

部署的真源为挂载数据目录内 `config.yaml` 的 `gateway` 节点；环境变量优先。
修改 YAML 后通过备用实例启动并切流生效。示例配置及四份 Compose 已接入：

| YAML 字段 | 默认 | 作用 |
|---|---:|---|
| `mirasim_failover_enabled` | `true` | 关闭时恢复旧次数预算 |
| `mirasim_failover_window_seconds` | `0` | 0 持续恢复；正值限制新尝试发起窗口，最大 600 秒 |
| `mirasim_backoff_initial_ms` | `1000` | 整池重试第一轮等待上限 |
| `mirasim_backoff_max_ms` | `8000` | 轮间等待与冷却状态复查的上限 |
| `mirasim_backoff_jitter` | `0.5` | 等待均匀分布在上限的 50%–100%；0 禁用抖动 |
| `mirasim_cooldown_base_seconds` | `30` | 单账号、单模型第一次失败的冷却 |
| `mirasim_cooldown_decay_seconds` | `1800` | 冷却结束后静默多久重置失败阶梯 |

环境变量为字段大写并添加 `GATEWAY_`。
已有数据库设置 `mirasim_capacity_park_minutes` 是冷却阶梯的最大分钟数，默认 60，
0 明确关闭失败记忆。它独立于请求恢复窗口；缩短窗口不会清掉账号失败记忆。

## 失败记忆

复用数据库 `accounts.extra.model_rate_limits["mirasim:capacity:<最终模型>"]`，
及现有 scheduler outbox/cache 同步，跨请求和实例重启有效。池模式也写入。
冷却中的账号不会直接请求上游；全池冷却时释放账号槽位并等待复查。
其他模型、额度窗口、模型权限和永久封禁保持隔离。
同模型成功仅清除该尝试观察到的冷却代次，不覆盖并发产生的新失败。

## 计费根因

已删除 Fable max 的模型名特判 3 倍注入。没有显式 `max_reasoning_effort_multiplier`
时不加价；显式倍率仍生效。请求、最终上游、响应模型计费模式共用此约定。
用户实扣与账号统计分别按各自明示价卡计算，不以关闭模型限制或切换计费模式绕过。
历史账单不自动重写。

## 验证

`go test -tags unit ./internal/config ./internal/handler ./internal/service ./internal/repository`
可运行全部单测；发布时至少覆盖 MirasimRecovery、MirasimCapacity、FableRecordUsage、
LoadMirasim 和冷却 CAS 测试。`sh deploy/tests/docker-compose-gateway-env-test.sh`
验证容器配置透传。验证报告需区分替身故障注入、数据库实写和真实上游调用。

蓝绿切换必须先固定旧实例承接流量，启动并直连验证备用实例，然后切代理目标；
旧实例保持运行直到在途请求排空。默认应用 shutdown 只有 5 秒，不足以代替排空。
