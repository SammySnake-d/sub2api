#!/usr/bin/env bash
# =============================================================================
# 迁移导入:在新机器上把整套跑起来
# =============================================================================
#
# 在**新机器**上跑,同目录下要有 export.sh 产出的 migrate-bundle-*.tar.gz。
#
#   bash import.sh migrate-bundle-20260916-153000.tar.gz
#
# ## 这个脚本最重要的一步不是恢复数据,是第 5 步
#
# 每个 mirasim 账号的出口代理地址**存在数据库里**,现在指向旧部署的公网地址
# (例如 45.205.28.160:2260)。迁移后 resin 与 sub2api 同在一个 compose 网络里,
# 地址必须变成 `resin:2260`。
#
# 不做这一步的后果特别有欺骗性:**服务全部正常启动、健康检查全绿、面板一切正常**,
# 但每个账号的请求都在往一台已经不该再用的旧机器上打 —— 而旧机器可能还活着,
# 于是连报错都没有,只是延迟变得莫名其妙地高,或者在你关掉旧机的那一刻全线崩掉。
#
# ## 幂等
#
# 脚本可以重复跑。PG 恢复走「先建库再导入」,已存在则要求显式 FORCE_RESTORE=1,
# 免得一次手滑把刚跑起来的生产库清空。
# =============================================================================
set -euo pipefail

BUNDLE="${1:-}"
[ -n "$BUNDLE" ] && [ -r "$BUNDLE" ] || { echo "用法: bash import.sh <migrate-bundle-*.tar.gz>" >&2; exit 1; }

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "[1/7] 解包并核对 MANIFEST ..."
tar -C "$WORK" -xzf "$BUNDLE"
SRC="$(find "$WORK" -maxdepth 1 -type d -name 'migrate-bundle-*' | head -1)"
[ -n "$SRC" ] || { echo "包里没有 migrate-bundle-* 目录" >&2; exit 1; }
[ -r "$SRC/MANIFEST.txt" ] || { echo "包里没有 MANIFEST.txt,拒绝导入未经校验的数据" >&2; exit 1; }

# shellcheck disable=SC1090
eval "$(grep -E '^[a-z0-9_]+=' "$SRC/MANIFEST.txt")"

GOT_SHA=$(sha256sum "$SRC/postgres.sql.gz" | cut -d' ' -f1)
[ "$GOT_SHA" = "$postgres_sha256" ] || {
  echo "数据库转储校验和不符 —— 包在传输中损坏了" >&2
  echo "  期望 $postgres_sha256" >&2
  echo "  实际 $GOT_SHA" >&2
  exit 1
}
echo "      校验通过: accounts=$accounts users=$users api_keys=$api_keys"

# ---------------------------------------------------------------------------
echo "[2/7] 准备 .env ..."
cd "$HERE"
if [ ! -f .env ]; then
  cp .env.example .env
  # resin 的两个 token 直接从包里继承。**必须继承而不是重新生成**:
  # RESIN_PROXY_TOKEN 是账号 credentials 里那串代理口令的来源,
  # 换掉它等于让 143 个账号的出口认证全部失效。
  if [ -s "$SRC/resin.env" ]; then
    while IFS='=' read -r k v; do
      [ -n "$k" ] || continue
      sed -i.bak "s|^${k}=.*|${k}=${v}|" .env && rm -f .env.bak
    done < "$SRC/resin.env"
  fi
  # 数据库口令新生成（它只在容器网络内部使用,不需要与旧环境一致）
  sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 24)|" .env && rm -f .env.bak
  chmod 600 .env
  echo "      已生成 .env（resin token 继承自旧环境,数据库口令新生成）"
else
  echo "      .env 已存在,保留不动"
fi
set -a; . ./.env; set +a

# ---------------------------------------------------------------------------
echo "[3/7] 启动 postgres / redis / resin ..."
docker compose up -d postgres redis resin
for i in $(seq 1 60); do
  docker compose exec -T postgres pg_isready -U "${POSTGRES_USER:-sub2api}" >/dev/null 2>&1 && break
  [ "$i" = 60 ] && { echo "postgres 起不来" >&2; docker compose logs --tail=40 postgres >&2; exit 1; }
  sleep 2
done

# ---------------------------------------------------------------------------
echo "[4/7] 恢复数据库 ..."
EXISTING=$(docker compose exec -T postgres psql -U "${POSTGRES_USER:-sub2api}" -d "${POSTGRES_DB:-sub2api}" \
  -t -A -c "select count(*) from information_schema.tables where table_schema='public'" 2>/dev/null || echo 0)
if [ "${EXISTING:-0}" -gt 0 ] && [ "${FORCE_RESTORE:-0}" != "1" ]; then
  echo "      目标库已有 $EXISTING 张表,跳过恢复（要覆盖请用 FORCE_RESTORE=1）"
else
  [ "${FORCE_RESTORE:-0}" = "1" ] && docker compose exec -T postgres psql -U "${POSTGRES_USER:-sub2api}" \
      -d "${POSTGRES_DB:-sub2api}" -c 'drop schema public cascade; create schema public;' >/dev/null
  gunzip -c "$SRC/postgres.sql.gz" \
    | docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-sub2api}" -d "${POSTGRES_DB:-sub2api}" >/dev/null
fi

# 行数核对。恢复「成功」但少了一半数据是完全可能的 —— 这是唯一能发现它的地方。
GOT_ACCOUNTS=$(docker compose exec -T postgres psql -U "${POSTGRES_USER:-sub2api}" -d "${POSTGRES_DB:-sub2api}" -t -A -c 'select count(*) from accounts')
[ "$GOT_ACCOUNTS" = "$accounts" ] || {
  echo "账号数不符: 期望 $accounts 实际 $GOT_ACCOUNTS —— 恢复不完整,拒绝继续" >&2
  exit 1
}
echo "      恢复完成,账号数核对通过: $GOT_ACCOUNTS"

# ---------------------------------------------------------------------------
echo "[5/7] 改写账号的出口代理地址（迁移最关键的一步）..."
#
# 见文件头的说明:不做这一步,一切看起来都正常,但请求全打在旧机器上。
#
# 只改 host/port,**不动 username/password** —— 那两个是账号的粘性身份
# (Paid.mirasim-N + 代理口令),改了等于换了一批出口身份。
BEFORE=$(docker compose exec -T postgres psql -U "${POSTGRES_USER:-sub2api}" -d "${POSTGRES_DB:-sub2api}" -t -A -c \
  "select coalesce(host,'')||':'||coalesce(port::text,'') from proxies group by 1 order by count(*) desc limit 5" | tr '\n' ' ')
echo "      改写前的代理地址分布: $BEFORE"

docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-sub2api}" -d "${POSTGRES_DB:-sub2api}" <<SQL >/dev/null
update proxies
   set host = 'resin', port = 2260
 where host is distinct from 'resin';
SQL

AFTER=$(docker compose exec -T postgres psql -U "${POSTGRES_USER:-sub2api}" -d "${POSTGRES_DB:-sub2api}" -t -A -c \
  "select host||':'||port::text||' x'||count(*) from proxies group by 1,2" | tr '\n' ' ')
echo "      改写后: $AFTER"

# ---------------------------------------------------------------------------
echo "[6/7] 恢复 resin 配置并改写 sub2api 配置 ..."
# resin 的配置走 API 灌回去，不是拷 SQLite —— 理由见 export.sh 第 3 步的注释：
# 热拷贝拿不到一致快照，而且旧机测出来的节点延迟数据在新机是**有害**的
# （新机位置不同，搬过去会让 P2C 按错误的数字选点）。
# 节点与延迟让新环境自己重新探测；这里只灌人的决策。
RESIN_TOKEN=$(grep -oE '^RESIN_ADMIN_TOKEN=.*' .env | cut -d= -f2-)
resin_api() { docker compose exec -T resin wget -qO- --header="Authorization: Bearer $RESIN_TOKEN" "$@" 2>/dev/null; }

for i in $(seq 1 45); do
  docker compose exec -T resin wget -qO- http://127.0.0.1:2260/healthz >/dev/null 2>&1 && break
  [ "$i" = 45 ] && { echo "resin 起不来" >&2; docker compose logs --tail=30 resin >&2; exit 1; }
  sleep 2
done

# 订阅源必须**先于**平台恢复:平台的 regex_filters 匹配的是 `<订阅源名>/<节点名>`，
# 源还没建时那条规则会匹配到零个节点，而 routable_node_count 会显示 0 —— 看起来像
# 过滤器写错了，实际是顺序错了。
#
# 只 POST **可写**字段。id / created_at / node_count / healthy_node_count 这些是
# 服务端派生的，带上去要么被拒，要么更糟：把一个旧的 healthy_node_count 写进新环境，
# 让面板显示一个从未在这台机器上测到过的数字。
if [ -s "$SRC/resin-config/subscriptions.json" ]; then
  echo "      恢复订阅源 ..."
  python3 - "$SRC/resin-config/subscriptions.json" > "$WORK/subs.ndjson" <<'PY'
import json, sys
WRITABLE = {"name", "url", "source_type", "enabled", "update_interval",
            "ephemeral", "ephemeral_node_evict_delay", "incremental_alive_nodes", "content"}
d = json.load(open(sys.argv[1]))
items = d.get("items") if isinstance(d, dict) else d
for s in (items or []):
    print(json.dumps({k: v for k, v in s.items() if k in WRITABLE and v not in (None, "")}))
PY
  SUB_OK=0; SUB_SKIP=0; SUB_FAIL=0
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    name=$(printf '%s' "$line" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("name",""))')
    # 幂等:已存在同名源就跳过,不重复创建(重复创建会让同一批节点被计两次)
    if docker compose exec -T resin wget -qO- --header="Authorization: Bearer $RESIN_TOKEN" \
         http://127.0.0.1:2260/api/v1/subscriptions 2>/dev/null \
       | grep -qF "\"name\":\"$name\""; then
      SUB_SKIP=$((SUB_SKIP+1)); continue
    fi
    if printf '%s' "$line" | docker compose exec -T resin sh -c \
        "wget -qO- --post-data=\"\$(cat)\" \
         --header='Authorization: Bearer $RESIN_TOKEN' \
         --header='content-type: application/json' \
         http://127.0.0.1:2260/api/v1/subscriptions" >/dev/null 2>&1; then
      SUB_OK=$((SUB_OK+1))
    else
      SUB_FAIL=$((SUB_FAIL+1)); echo "        失败: $name" >&2
    fi
  done < "$WORK/subs.ndjson"
  echo "      订阅源: 新建 $SUB_OK / 已存在跳过 $SUB_SKIP / 失败 $SUB_FAIL"
  [ "$SUB_FAIL" -eq 0 ] || echo "      警告: 有订阅源没恢复，出口容量会不足" >&2
fi

# 系统配置与平台配置直接 PATCH（它们是更新而非创建）
if [ -s "$SRC/resin-config/system-config.json" ]; then
  python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
# 只保留可写键，剔除只读/派生字段
print(json.dumps({k:v for k,v in d.items() if not k.startswith("_")}))' \
    "$SRC/resin-config/system-config.json" > /tmp/resin-sys.json
  docker compose exec -T resin sh -c "wget -qO- --method=PATCH \
    --header='Authorization: Bearer $RESIN_TOKEN' \
    --header='content-type: application/json' \
    --body-data=\"\$(cat)\" http://127.0.0.1:2260/api/v1/system/config" < /tmp/resin-sys.json >/dev/null 2>&1 \
    && echo "      系统配置已灌入" || echo "      警告: 系统配置灌入失败，需手工用 deploy/apply-resin-runtime.sh" >&2
  rm -f /tmp/resin-sys.json
fi

echo "      提醒: 平台配置(订阅源白名单 regex_filters / region_filters)请用"
echo "            deploy/apply-resin-runtime.sh 应用并回读校验 —— 它是那份配置的真源，"
echo "            比从旧环境导出的快照更可信。"

# sub2api 的 config.yaml:把数据库/redis 指向容器网络里的服务名。
python3 - "$SRC/config.yaml" "$HERE/config.rendered.yaml" <<'PY'
import re, sys
src, dst = sys.argv[1], sys.argv[2]
text = open(src, encoding='utf-8').read()
# 只替换主机名,保留其余字段(口令由 .env 注入的环境变量覆盖,见 compose)
text = re.sub(r'(?m)^(\s*host:\s*).*(#.*)?$', r'\1postgres', text, count=1)
open(dst, 'w', encoding='utf-8').write(text)
print(f"      配置已渲染: {dst}（请人工复核数据库/redis 主机名与口令）")
PY

# ---------------------------------------------------------------------------
echo "[7/7] 启动 sub2api ..."
docker compose up -d sub2api
sleep 10
docker compose ps

cat <<'NEXT'

导入完成。**在把流量切过来之前**,至少确认这三件事:

  1. 出口真的走了新 resin —— 随便挑一个账号点「查询」额度,看它是不是秒回。
     如果要 60~90 秒,说明第 5 步没生效或 resin 状态没恢复。
  2. resin 的平台配置还在 —— 订阅源白名单(regex_filters)和 region_filters
     是随 resin 状态目录过来的,用 deploy/apply-resin-runtime.sh 回读校验一遍。
  3. 旧机器先**停服务但别删**。确认新环境跑满一天再清理;在那之前旧机若还活着,
     任何漏改的地址都会静默地继续打过去,你不会收到任何报错。

NEXT
