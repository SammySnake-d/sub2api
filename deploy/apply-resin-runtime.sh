#!/usr/bin/env bash
# 把 deploy/resin-runtime.json 应用到本机 resin 的运行时配置。
#
# 这个脚本存在的理由和那份 json 一样：出口池的几个关键判决（延迟靶子指哪、
# 一个出口 IP 上压几个号）是踩过坑才定下来的，只活在 resin 数据库里等于没留。
# 有了它，「线上现在是什么配置」这个问题的答案是仓库里的一个文件，而不是一次 curl。
#
# 用法（在部署了 resin 的机器上）：bash deploy/apply-resin-runtime.sh
set -euo pipefail

CONF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/resin-runtime.json"
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
