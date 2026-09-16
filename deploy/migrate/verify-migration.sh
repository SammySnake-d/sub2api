#!/usr/bin/env bash
# =============================================================================
# 迁移后数据对照:证明新环境与旧环境**等价**,而不只是"跑起来了"
# =============================================================================
#
# 用法（在能同时 ssh 到两端的机器上跑，例如你的本机）:
#
#   OLD_SUB2API=agcn OLD_RESIN=loon-resin \
#   NEW_HOST='ssh -p 2222 -o IdentitiesOnly=yes -i ~/.ssh/server2_ed25519 root@169.58.222.148' \
#   bash verify-migration.sh
#
# 退出码 0 = 逐项一致；非 0 = 有差异，差异逐条打印。
#
# ## 为什么需要这个脚本，而 import.sh 里的核对不够
#
# import.sh 只核对**行数**（accounts/users/api_keys）。行数一致完全可能内容已经漂了，
# 而最容易漂的恰恰是**配置**：
#
#   1. **sub2api 的配置有两个来源，DB 那份优先。** config.yaml 只是启动时的兜底，
#      运维在管理后台改的每一项都落在 `settings` 表里，读取时 DB 存值覆盖文件值
#      （setting_service.go:326）。只对照 config.yaml 会漏掉全部 UI 改动 ——
#      包括那个 `trust_forwarded_ip_for_api_key_acl`。
#   2. **resin 的平台配置不在文件里**，在它自己的 SQLite 里。订阅源白名单
#      （regex_filters）和 region_filters 丢了不会报错，只会让出口池悄悄退回
#      "什么源都用"，然后坏节点重新把账号钉住。
#   3. **账号的代理绑定**是迁移时唯一必须被改写的东西，所以也是唯一可能被改错的。
#
# 这三项的共同特征:**丢了都不报错**，只是性能或安全悄悄退化。
# =============================================================================
set -uo pipefail

OLD_SUB2API="${OLD_SUB2API:-agcn}"
OLD_RESIN="${OLD_RESIN:-loon-resin}"
NEW_HOST="${NEW_HOST:?NEW_HOST 必须设置，例如 'ssh -p 2222 -i ~/.ssh/k root@1.2.3.4'}"
PG_DB="${PG_DB:-sub2api}"
STACK_DIR="${STACK_DIR:-/opt/sub2api-stack}"

DIFFS=0
note_diff() { DIFFS=$((DIFFS+1)); echo "  [差异] $*"; }
ok()        { echo "  [一致] $*"; }

# 旧环境：裸机 systemd 部署，psql 走 sudo -u postgres
old_psql() { ssh -o ConnectTimeout=30 "$OLD_SUB2API" "sudo -u postgres psql -d $PG_DB -t -A -c \"$1\"" 2>/dev/null; }
# 新环境：容器部署，psql 走 compose exec
new_psql() { $NEW_HOST "cd $STACK_DIR && docker compose exec -T postgres psql -U sub2api -d $PG_DB -t -A -c \"$1\"" 2>/dev/null; }

old_resin_api() { ssh -o ConnectTimeout=30 "$OLD_RESIN" "curl -s -m 25 -H \"Authorization: Bearer \$(systemctl cat resin | grep -oP 'RESIN_ADMIN_TOKEN=\K\S+' | head -1)\" 'http://127.0.0.1:2260$1'" 2>/dev/null; }
new_resin_api() { $NEW_HOST "cd $STACK_DIR && curl -s -m 25 -H \"Authorization: Bearer \$(grep -oP '^RESIN_ADMIN_TOKEN=\K.*' .env)\" 'http://127.0.0.1:2260$1'" 2>/dev/null; }

echo "=============================================================="
echo " 1. 行数对照（表级）"
echo "=============================================================="
for t in accounts users api_keys settings groups subscriptions redeem_codes; do
  o=$(old_psql "select count(*) from $t")
  n=$(new_psql "select count(*) from $t")
  if [ -z "$o" ] || [ -z "$n" ]; then
    note_diff "$t: 读不到（旧='$o' 新='$n'）—— 读不到不等于一致，按差异计"
  elif [ "$o" = "$n" ]; then
    ok "$t = $o"
  else
    note_diff "$t: 旧 $o vs 新 $n"
  fi
done

echo
echo "=============================================================="
echo " 2. settings 表逐项对照（**DB 存值优先于 config.yaml，这里是重点**）"
echo "=============================================================="
# 按 key 排序后整表哈希，先看整体；不同再逐项列差异。
# 只比 key/value，不比 id/时间戳 —— 后者迁移后必然不同且无意义。
# 排序在 shell 侧用 `sort` 做，**不用 SQL 的 order by**。
# 踩过的坑：旧环境是裸机 PG(en_US.UTF-8 collation)、新环境是 alpine 容器(C collation)，
# 同一批数据 `order by key` 出来的顺序不同，diff 就把一份逐字相同的配置报成"全表差异"。
# 那是假阳性，而假阳性比漏报更糟——它会让人学会忽略这个门。
OLD_SET=$(old_psql "select key||'='||coalesce(value,'') from settings" | LC_ALL=C sort)
NEW_SET=$(new_psql "select key||'='||coalesce(value,'') from settings" | LC_ALL=C sort)
if [ -z "$OLD_SET" ] && [ -z "$NEW_SET" ]; then
  note_diff "两边 settings 都读不到 —— 无法证明一致"
elif [ "$OLD_SET" = "$NEW_SET" ]; then
  ok "settings 全表逐项一致（$(echo "$OLD_SET" | wc -l | tr -d ' ') 项）"
else
  note_diff "settings 有差异，逐条如下（< 旧 / > 新）:"
  diff <(echo "$OLD_SET") <(echo "$NEW_SET") | grep -E '^[<>]' | head -30 | sed 's/^/        /'
fi

echo
echo "=============================================================="
echo " 3. 账号身份完整性（credentials 的**键**，不打印值）"
echo "=============================================================="
# 比的是每个账号有哪些凭据键、以及 device seed / session id 是否都在。
# 不打印值：这些是设备根与令牌，进了终端就进了 scrollback。
for q in \
  "select count(*) from accounts where credentials ? 'mirasim_device_seed'" \
  "select count(*) from accounts where extra ? 'mirasim_session_id'" \
  "select count(*) from accounts where credentials ? 'access_token'" \
  "select count(*) from accounts where proxy_id is not null" ; do
  o=$(old_psql "$q"); n=$(new_psql "$q")
  label=$(echo "$q" | grep -oE "'[a-z_]+'|proxy_id is not null" | head -1)
  if [ "$o" = "$n" ] && [ -n "$o" ]; then ok "$label: $o"; else note_diff "$label: 旧 $o vs 新 $n"; fi
done

# device seed 的**指纹**对照：证明是同一批设备根，而不只是"数量一样"。
# 用 md5 聚合，绝不导出明文。
o=$(old_psql "select md5(string_agg(md5(credentials->>'mirasim_device_seed'), ',' order by id)) from accounts where credentials ? 'mirasim_device_seed'")
n=$(new_psql "select md5(string_agg(md5(credentials->>'mirasim_device_seed'), ',' order by id)) from accounts where credentials ? 'mirasim_device_seed'")
if [ "$o" = "$n" ] && [ -n "$o" ]; then
  ok "设备根指纹一致（${o}）—— 是同一批设备，不只是数量相同"
else
  note_diff "设备根指纹不一致: 旧 $o vs 新 $n —— 上游会认为这是一批全新设备"
fi

echo
echo "=============================================================="
echo " 4. 代理绑定改写（迁移唯一必须变的东西，所以也是唯一可能改错的）"
echo "=============================================================="
OLD_PROXY=$(old_psql "select host||':'||port from proxies group by 1 order by count(*) desc limit 3" | tr '\n' ' ')
NEW_PROXY=$(new_psql "select host||':'||port from proxies group by 1 order by count(*) desc limit 3" | tr '\n' ' ')
echo "  旧: $OLD_PROXY"
echo "  新: $NEW_PROXY"
if echo "$NEW_PROXY" | grep -q 'resin:2260'; then
  STALE=$(new_psql "select count(*) from proxies where host <> 'resin'")
  if [ "${STALE:-1}" = "0" ]; then
    ok "全部指向容器内 resin:2260，无遗留旧地址"
  else
    note_diff "$STALE 个代理仍指向旧地址 —— 这些账号的请求会打回旧机器，而且不会报错"
  fi
else
  note_diff "新环境的代理没有指向 resin:2260 —— import.sh 第 5 步没生效"
fi

# 用户名/口令**必须**保持不变（粘性身份 + 出口认证）
o=$(old_psql "select md5(string_agg(coalesce(username,'')||':'||coalesce(password,''), ',' order by id)) from proxies")
n=$(new_psql "select md5(string_agg(coalesce(username,'')||':'||coalesce(password,''), ',' order by id)) from proxies")
if [ "$o" = "$n" ] && [ -n "$o" ]; then
  ok "代理身份（username/password）指纹一致 —— 粘性身份与出口认证未变"
else
  note_diff "代理身份指纹不一致 —— 出口会 407，或粘性身份全部改变"
fi

echo
echo "=============================================================="
echo " 5. resin 配置（不在文件里，丢了不报错）"
echo "=============================================================="
for path in "/api/v1/system/config" "/api/v1/platforms"; do
  o=$(old_resin_api "$path")
  n=$(new_resin_api "$path")
  if [ -z "$o" ] || [ -z "$n" ]; then
    note_diff "$path: 读不到（旧 $(echo -n "$o" | wc -c) 字节 / 新 $(echo -n "$n" | wc -c) 字节）"
    continue
  fi
  # 逐键比较，忽略随时间变化的字段（节点数、更新时间）
  python3 - "$o" "$n" "$path" <<'PY'
import json, sys
volatile = {"routable_node_count", "updated_at", "created_at", "node_count", "healthy_node_count"}
def norm(x):
    if isinstance(x, dict):
        return {k: norm(v) for k, v in sorted(x.items()) if k not in volatile}
    if isinstance(x, list):
        return [norm(v) for v in x]
    return x
try:
    a, b, path = json.loads(sys.argv[1]), json.loads(sys.argv[2]), sys.argv[3]
except Exception as e:
    print(f"  [差异] {sys.argv[3]}: 返回不是合法 JSON ({e})"); sys.exit(0)
na, nb = norm(a), norm(b)
if na == nb:
    print(f"  [一致] {path}")
else:
    print(f"  [差异] {path}:")
    ka = json.dumps(na, sort_keys=True, ensure_ascii=False, indent=1).splitlines()
    kb = json.dumps(nb, sort_keys=True, ensure_ascii=False, indent=1).splitlines()
    import difflib
    for line in list(difflib.unified_diff(ka, kb, "旧", "新", lineterm="", n=1))[:40]:
        print("        " + line)
PY
done

echo
echo "=============================================================="
if [ "$DIFFS" -eq 0 ]; then
  echo " 全部一致。"
  echo
  echo " 注意：一致 ≠ 可以立刻切流量。仍需人工确认的两件事："
  echo "   - 挑一个账号点「查询」额度，秒回（<2s）才说明出口真的通了"
  echo "   - 旧机先停服务别删，跑满一天再清理"
  exit 0
else
  echo " 发现 $DIFFS 处差异（见上）。**不要在差异消除前切流量。**"
  exit 1
fi
