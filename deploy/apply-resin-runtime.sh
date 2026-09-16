#!/usr/bin/env bash
# 把 deploy/resin-runtime.json（系统级）与 deploy/resin-platforms.json（平台级）
# 应用到本机 resin 的运行时配置。
#
# 这个脚本存在的理由和那两份 json 一样：出口池的几个关键判决（延迟靶子指哪、
# 一个出口 IP 上压几个号、允许哪些订阅源）是踩过坑才定下来的,只活在 resin 数据库里
# 等于没留。有了它,「线上现在是什么配置」这个问题的答案是仓库里的文件,而不是一次 curl。
#
# 用法（在部署了 resin 的机器上）：bash deploy/apply-resin-runtime.sh
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONF="$DIR/resin-runtime.json"
PLATCONF="$DIR/resin-platforms.json"
ENV_FILE="${RESIN_ENV_FILE:-/etc/resin.env}"
BASE="${RESIN_ADMIN_BASE:-http://127.0.0.1:2260}"

[ -r "$CONF" ] || { echo "读不到 $CONF" >&2; exit 1; }

# token 从环境文件里取，绝不落在命令行参数上（ps 能看见别人的命令行）。
TOKEN="${RESIN_ADMIN_TOKEN:-}"
if [ -z "$TOKEN" ]; then
  [ -r "$ENV_FILE" ] || { echo "读不到 $ENV_FILE，且 RESIN_ADMIN_TOKEN 未设置" >&2; exit 1; }
  TOKEN="$(grep -E '^RESIN_ADMIN_TOKEN=' "$ENV_FILE" | cut -d= -f2-)"
fi
[ -n "$TOKEN" ] || { echo "RESIN_ADMIN_TOKEN 为空" >&2; exit 1; }

# 剥掉下划线开头的说明性键 —— 它们是给人读的，控制面不认。
PAYLOAD="$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
json.dump({k:v for k,v in d.items() if not k.startswith("_")}, sys.stdout)
' "$CONF")"

echo "$PAYLOAD" | curl -sS -X PATCH \
  -H "Authorization: Bearer $TOKEN" \
  -H 'content-type: application/json' \
  --data @- "$BASE/api/v1/system/config" >/dev/null

# 回读校验：PATCH 返回 200 不等于值真的生效了。
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/system/config" \
  | python3 -c '
import json,sys
live=json.load(sys.stdin)
want={k:v for k,v in json.load(open(sys.argv[1])).items() if not k.startswith("_")}
bad=[(k,want[k],live.get(k)) for k in want if live.get(k)!=want[k]]
if bad:
    for k,w,g in bad: print("不一致 %s: 期望 %r 实际 %r" % (k,w,g), file=sys.stderr)
    sys.exit(1)
print("resin 运行时配置已应用并回读校验通过（%d 项）" % len(want))
' "$CONF"

# ---------------------------------------------------------------------------
# 平台级配置。单独一轮,因为它是另一个端点、而且每个平台一次 PATCH。
#
# 刻意**不**在这里创建平台:id 是线上生成的,真源里写死 id 只是为了让「这份文件
# 管的是哪个平台」有个确定答案。id 对不上就应该失败,而不是悄悄新建一个平台。
# ---------------------------------------------------------------------------
[ -r "$PLATCONF" ] || { echo "读不到 $PLATCONF（跳过平台级配置）" >&2; exit 0; }

python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for p in d.get("platforms",[]):
    pid=p.get("id")
    if not pid:
        print("平台缺 id,拒绝应用", file=sys.stderr); sys.exit(1)
    body={k:v for k,v in p.items() if not k.startswith("_") and k!="id"}
    print("%s\t%s" % (pid, json.dumps(body)))
' "$PLATCONF" | while IFS=$'\t' read -r pid body; do
  echo "$body" | curl -sS -X PATCH \
    -H "Authorization: Bearer $TOKEN" \
    -H 'content-type: application/json' \
    --data @- "$BASE/api/v1/platforms/$pid" >/dev/null

  # 回读校验。PATCH 返回 200 不代表值真的落了 —— 平台配置尤其如此:
  # 一个语法非法的正则会被编译阶段拒掉,而 HTTP 层可能已经回了成功。
  curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/platforms/$pid" \
    | python3 -c '
import json,sys
live=json.load(sys.stdin)
want=json.loads(sys.argv[1])
bad=[(k,want[k],live.get(k)) for k in want if live.get(k)!=want[k]]
if bad:
    for k,w,g in bad: print("平台 %s 不一致 %s: 期望 %r 实际 %r" % (sys.argv[2],k,w,g), file=sys.stderr)
    sys.exit(1)
print("平台 %s 配置已应用并回读校验通过;当前可路由节点 %s 个"
      % (live.get("name", sys.argv[2]), live.get("routable_node_count")))
' "$body" "$pid"
done

# ---------------------------------------------------------------------------
# 收紧订阅源白名单**不会**让已有的 sticky 租约迁走 —— resin 的 LeaseCleaner 只按
# TTL(168h)回收,不看节点是否还符合平台过滤器。实测 2026-09-16:改完过滤器后
# 196 个租约仍指向已被过滤掉的节点,一个都没自动迁移。
#
# 所以改过 regex_filters / region_filters 之后必须手工释放失格租约,否则这次改动
# 对存量账号完全没有效果(而 routable_node_count 会显示得很成功,极具误导性)。
# 释放方式:
#   DELETE /api/v1/platforms/{id}/leases/{account}   单个
#   DELETE /api/v1/platforms/{id}/leases             全部
#
# 这里刻意**不**自动释放:释放会让账号换出口 IP,那对上游是可见事件,不该由一个
# 「应用配置」脚本静默触发。
# ---------------------------------------------------------------------------
echo "提醒:若本次改动了 regex_filters / region_filters,存量 sticky 租约不会自动迁移,需手工释放失格租约。"
