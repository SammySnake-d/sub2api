# proxy-auth-and-exposure

状态: **通过**

判定日期: 2026-09-16
判定人: snakesammy（取证由 Claude Opus 5 执行）

## 核对内容

(1) 代理认证走 `Proxy-Authorization` 而非 `Authorization`；
(2) 对外只有 IP:6699 明文 HTTP（腾讯云未备案，上不了域名），这个事实被显式记录。

## 第一条：已降级为机器可判

`backend/internal/service/mirasim_deploy_proxy_test.go` 打在真实路径上：
`tlsfingerprint.HTTPProxyDialer.DialTLSContext` 手写 CONNECT 并
`Set("Proxy-Authorization", "Basic "+auth)`；mirasim 请求恒走这条
（`repository/mirasim_upstream.go:113` 强制 `MirasimProfile()` →
`repository/http_upstream.go:1438` 选 `NewHTTPProxyDialer`）。
差分阴性摘掉 userinfo → 必须 407；反例保护把凭据放进 `Authorization`
（ma-relay 踩过的写法）→ 必须 407。

## 第二条：对外暴露面（人判）

- 对外监听：`0.0.0.0:6699`，**明文 HTTP**，无域名、无 TLS。
- 原因：腾讯云未备案，域名用不了；运营者已明确「用 IP 就行，不需要配域名」。
- 后果**显式记录在此**，不留给下一个人去撞：
  - 客户端 API key 在链路上是明文的。任何能被动嗅探这条链路的人都能拿到它。
  - 管理后台口令同样明文过链路。
  - 缓解措施目前只有「知道 IP 和端口的人才打得到」，这**不是**安全边界。
- resin 的代理口只监听 `127.0.0.1:2260`，外网不可达（已验证）。
- 备案下来之后的正确动作：上域名 + TLS，而不是继续在明文上加别的东西。

## 结论

两条判据均通过（第二条的「通过」= 事实已被记录，不是「这样做是安全的」）。

## 机器核不到的那一半

「明文 HTTP 的风险是否可接受」是运营者的判断，不是机器能判的。
这份记录只保证下一个接手的人不会以为这里有 TLS。
