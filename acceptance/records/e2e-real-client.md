# e2e-real-client

状态: **通过**

判定日期: 2026-09-16
判定人: snakesammy（取证由 Claude Opus 5 执行）

## 核对内容

用**真实 Claude Code 客户端**（不是 curl 手拼 body）指向 sub2 的 URL，
用后台生成的 key（不是管理员 token）发起一次真实会话。

## 怎么做的

```
claude --settings '{"env":{"ANTHROPIC_BASE_URL":"http://124.221.206.59:6699",
                           "ANTHROPIC_AUTH_TOKEN":"<后台生成的 e2e-test key>"}}' \
       -p "reply with exactly: PONG" --model claude-haiku-4-5
```

- 客户端：**真实 Claude Code 2.1.272**（UA 实抓为 `claude-cli/2.1.272 (external, sdk-cli)`）
- URL：`http://124.221.206.59:6699`（线上实例，非本地）
- key：后台 `api_keys` 表里 `name='e2e-test'` 的那一条，不是管理员 token
- **线上分组 `claude_code_only` 处于开启状态**（`groups.claude_code_only=t`，group_id=2），
  所以这次往返同时证明了「真 CC 过得了那道门」

## 观察到什么

- 客户端输出：`PONG`，退出码 0
- sub2api 日志：请求带 `group_id: 2` 通过 CC 门进入账号选择
  （`sticky.selecting_account` → `sticky.account_selected`），无 `ErrClaudeCodeOnly`
- 同批次的机器化 E2E（`mirasim_e2e_live_test.go`，用同一把 key 同一条 URL）实测：
  - 生成的 key → **HTTP 200**，19.3s
  - 只换 key 值、其余一字不差 → **HTTP 401** `INVALID_API_KEY`
  - 同一把 key 打管理面 → **HTTP 401** `INVALID_ADMIN_KEY`（证明它不是管理员凭据）
  - 返回体：`type=message role=assistant model=claude-haiku-4-5-20251001
    stop=end_turn content="pong"`
  - **usage: input_tokens=68, output_tokens=5**（两者均非零）

## 结论

三条判据全部通过。

## 为什么这一条仍然需要人判

`mirasim_e2e_live_test.go` 覆盖了后两条（key 是客户面的、content 与 usage 是真的），
但它**恰恰是手拼 body** —— 它证明不了「客户端是真的」。那一条只能由这份记录承担：
上面那条 `claude ... -p` 命令是真实客户端跑的，这件事没有任何机器证据能替代。

## 机器核不到的那一半

「这把 key 由后台生成、名叫 e2e-test」这个**来源**事实，测试够不到（没有能查 key
来源的接口）。机器能给的最强证据是「它是客户面 key 且不是管理员凭据」。
