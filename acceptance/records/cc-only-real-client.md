# 人判记录：claude_code_only 门对真实 Claude Code 的放行

判定日期：2026-09-16
判定人：snakesammy（由 Claude Opus 5 执行取证）

## 结论

**此前记录的「开启 claude_code_only 会误拒真 CC」不成立，已在生产上证伪。**

## 取证方式（线上真机，不是单测）

1. 线上 `groups.claude_code_only` 由 `f` 改为 `t`（group_id=2 / mirasim），重启 sub2api 使分组缓存失效。
2. 用**真实 Claude Code 2.1.272** 打线上网关：
   `claude --settings '{"env":{"ANTHROPIC_BASE_URL":"http://<vps>:6699","ANTHROPIC_AUTH_TOKEN":"<e2e-test key>"}}' -p "reply with exactly: PONG"`
3. 结果：**返回 PONG，退出码 0**。sub2api 日志显示请求带 `group_id: 2` 通过了 CC 门并进入账号选择
   （`sticky.selecting_account` → `sticky.account_selected`），没有任何 `ErrClaudeCodeOnly`。

## 此前误诊的原因

诊断停在了校验器层（`ClaudeCodeValidator.Validate`）。那一层从未复现，于是被记成「查不到根因」。
真正缺的是一条**跨层**的证据：判定链是
`SetClaudeCodeClientContext` → 用 `ParsedRequest` **重建**的 bodyMap 喂校验器 → 写 ctx →
`resolveGatewayGroup` 读 ctx。校验器测试只覆盖中间一步。

补齐的方式是先用本地抓包服务器拿到真 CC 的**实际线上字节**（完整请求头 + body），
再逐门单独判：UA / system 相似度 / X-App / anthropic-beta / anthropic-version /
metadata.user_id，六道门全过，`IsClaudeCodeClient = true`。

实抓关键事实：
- UA 是 `claude-cli/2.1.272 (external, sdk-cli)` —— 后缀是 `sdk-cli` 不是旧版的 `cli`；
  UA 正则只锚 `^claude-cli/\d+\.\d+\.\d+`，所以不受影响。
- `metadata.user_id` 是 JSON 串，`account_uuid` 为**空串**，但 `ParseMetadataUserID` 只要求
  `device_id` 与 `session_id` 非空，故通过。
- `system` 是 2 块数组，block[0] 是 94 字符身份句独占一个 `cache_control` 断点。

## 回归锁

`backend/internal/handler/claude_code_only_ctx_test.go`
- `TestClaudeCodeOnlyGateAcceptsRealClaudeCode`：走完整生产链路（含 bodyMap 重建），
  断言 `IsClaudeCodeClient == true` 且版本号写进了 ctx。
- `TestClaudeCodeOnlyGateStillRejectsPlainAPIClient`：差分阴性 —— 同一 body 只换 UA，必须判 false。
  没有这条，把判定改成无条件 true 也能让上一条转绿。

## 当前线上状态

`claude_code_only` 保持**开启**（group_id=2）。
