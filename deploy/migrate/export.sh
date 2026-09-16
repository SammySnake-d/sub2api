#!/usr/bin/env bash
# =============================================================================
# 迁移导出:把旧部署的全部状态打成一个包
# =============================================================================
#
# 在**旧机器**上跑。产出 migrate-bundle-<时间戳>.tar.gz,里面是:
#
#   postgres.sql.gz     sub2api 全库（账号/凭据/用量/审计）
#   resin-config/       resin 配置（订阅源/平台/系统配置，走 API 导出的 JSON）
#   config.yaml         sub2api 生效的那一份
#   resin.env           resin 的 token（admin / proxy）
#   MANIFEST.txt        每份的大小、校验和、导出时刻的实测数字
#
# ## 为什么要有 MANIFEST
#
# 迁移最常见的失败不是「没导出来」,是「导出来了但少了一半而没人发现」。
# 导入侧会拿 MANIFEST 逐项核对行数与校验和,对不上就拒绝启动 —— 一个少了 100 个
# 账号的数据库能正常启动,而你会在几天后才发现。
#
# ## resin 与 sub2api 可能不在同一台机器
#
# 当前拓扑就是这样(sub2api 在国内、resin 在海外),所以两个来源分别用
# SUB2API_HOST / RESIN_HOST 指定,默认都是本机。跨机时通过 ssh 取。
#
# 用法:
#   SUB2API_HOST=agcn RESIN_HOST=loon-resin bash export.sh
#   bash export.sh                 # 全在本机
# =============================================================================
set -euo pipefail

TS="$(date +%Y%m%d-%H%M%S)"
OUT="${OUT_DIR:-.}/migrate-bundle-$TS"
SUB2API_HOST="${SUB2API_HOST:-}"
RESIN_HOST="${RESIN_HOST:-}"
PG_DB="${PG_DB:-sub2api}"
SUB2API_CONFIG="${SUB2API_CONFIG:-/app/data/config.yaml}"
RESIN_DATA="${RESIN_DATA:-/opt/resin/data}"

# 在指定主机上执行；主机为空则本地执行。
# 统一走这个函数是为了让「本机」和「远程」两条路径共用同一串命令,
# 否则两条路径会各自漂移,而只有其中一条会被测到。
on() {
  local host="$1"; shift
  if [ -z "$host" ]; then
    bash -c "$*"
  else
    ssh -o ConnectTimeout=30 "$host" "$*"
  fi
}

mkdir -p "$OUT"
echo "导出目标: $OUT"

# ---------------------------------------------------------------------------
# 1. PostgreSQL 全库
#
# 用 pg_dump 而不是拷 data 目录:跨大版本拷目录不可用,而 dump 是版本无关的。
# --no-owner / --no-privileges:新环境的角色名可能不同,带上属主会让恢复报错。
# ---------------------------------------------------------------------------
echo "[1/4] 导出 PostgreSQL ..."
on "$SUB2API_HOST" "sudo -u postgres pg_dump --no-owner --no-privileges '$PG_DB'" \
  | gzip -9 > "$OUT/postgres.sql.gz"

PG_ACCOUNTS=$(on "$SUB2API_HOST" "sudo -u postgres psql -d '$PG_DB' -t -A -c 'select count(*) from accounts'")
PG_USERS=$(on "$SUB2API_HOST" "sudo -u postgres psql -d '$PG_DB' -t -A -c 'select count(*) from users'")
PG_KEYS=$(on "$SUB2API_HOST" "sudo -u postgres psql -d '$PG_DB' -t -A -c 'select count(*) from api_keys'")

# ---------------------------------------------------------------------------
# 2. sub2api 生效的配置
#
# **注意取的是 /app/data/config.yaml,不是 /etc/sub2api/config.yaml。**
# viper 的搜索顺序是 DATA_DIR → /app/data → . → ./config → /etc/sub2api,
# 前者会静默盖掉后者。导错文件的话新环境会跑在一份看起来对、实际没生效过的配置上。
# ---------------------------------------------------------------------------
echo "[2/4] 导出 sub2api 配置 ..."
on "$SUB2API_HOST" "sudo cat '$SUB2API_CONFIG'" > "$OUT/config.yaml"

# ---------------------------------------------------------------------------
# 3. resin 配置（**走 API 导出 JSON，不拷 SQLite**）
#
# 第一版是 tar 整个状态目录，那是错的，两个独立的理由：
#
# 1. **热拷贝 SQLite 拿不到一致快照。** resin 一直在写（节点探测、租约、指标），
#    tar 当场报 `file changed as we read it`，而 -wal 与主库的错位不会有任何提示，
#    只会在新环境表现成"配置莫名其妙少了几项"。
#
# 2. **更重要：旧的节点数据搬过去是有害的，不是有用的。** cache.db 里存的是每个
#    节点的延迟 EWMA 与健康状态，而那是**在旧机器的位置上测出来的**。新机在法国，
#    到各出口节点的距离完全不同；把旧延迟搬过去，P2C 会按一组错误的数字选点，
#    直到探测慢慢把它们覆盖掉为止。让新环境重新探测才是对的。
#
# 所以这里只导出**配置**——那部分是人的决策，重建不出来：
#   订阅源列表 / 平台（含订阅源白名单 regex_filters 与 region_filters）/
#   系统配置（延迟天花板、每 IP 租约上限…）/ 端点 / 账号头规则
# 租约（leases）刻意不导：它是运行态，且迁移后本来就该按新的延迟重新分配。
# ---------------------------------------------------------------------------
echo "[3/4] 导出 resin 配置（API，不拷 SQLite）..."
RESIN_TOKEN=$(on "$RESIN_HOST" "systemctl cat resin 2>/dev/null | grep -oE 'RESIN_ADMIN_TOKEN=[^ ]+' | head -1 | cut -d= -f2-")
if [ -z "$RESIN_TOKEN" ]; then
  RESIN_TOKEN=$(on "$RESIN_HOST" "sudo grep -hE '^RESIN_ADMIN_TOKEN=' /etc/resin.env /opt/resin/resin.env 2>/dev/null | head -1 | cut -d= -f2-")
fi
[ -n "$RESIN_TOKEN" ] || { echo "拿不到 RESIN_ADMIN_TOKEN，无法导出 resin 配置" >&2; exit 1; }

mkdir -p "$OUT/resin-config"
for ep in system/config platforms subscriptions endpoints account-header-rules; do
  fname="$(echo "$ep" | tr '/' '-').json"
  body=$(on "$RESIN_HOST" "curl -s -m 30 -H 'Authorization: Bearer $RESIN_TOKEN' 'http://127.0.0.1:2260/api/v1/$ep'")
  # 逐个校验是 JSON。一个 404 的 "page not found" 存成 .json 文件不会有任何提示，
  # 而导入侧会当成"这一项本来就是空的"。
  if echo "$body" | python3 -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null; then
    echo "$body" > "$OUT/resin-config/$fname"
    echo "      $ep -> $(echo -n "$body" | wc -c | tr -d ' ') 字节"
  else
    echo "      警告: $ep 返回的不是 JSON，跳过（新环境该项需手工确认）" >&2
    echo 'null' > "$OUT/resin-config/$fname"
  fi
done

# country.mmdb 是 GeoIP 库，纯静态数据、与位置无关，拷过去省一次下载。
on "$RESIN_HOST" "sudo test -r '$RESIN_DATA/country.mmdb' && sudo cat '$RESIN_DATA/country.mmdb'" \
  > "$OUT/resin-config/country.mmdb" 2>/dev/null || rm -f "$OUT/resin-config/country.mmdb"

# resin 的 token 在 systemd unit 的 Environment= 里,不一定有 env 文件,
# 所以从 systemctl cat 抓,取不到再退回 env 文件。
echo "[4/4] 导出 resin 凭据 ..."
{
  on "$RESIN_HOST" "systemctl cat resin 2>/dev/null | grep -oE 'RESIN_(ADMIN|PROXY)_TOKEN=[^ ]+'" || true
  on "$RESIN_HOST" "sudo grep -hE '^RESIN_(ADMIN|PROXY)_TOKEN=' /etc/resin.env /opt/resin/resin.env 2>/dev/null" || true
} | sort -u > "$OUT/resin.env"

if ! grep -q 'RESIN_ADMIN_TOKEN=' "$OUT/resin.env"; then
  echo "警告: 没抓到 RESIN_ADMIN_TOKEN,导入时需要手工填进 .env" >&2
fi
chmod 600 "$OUT/resin.env" "$OUT/config.yaml"

# ---------------------------------------------------------------------------
# MANIFEST：导入侧据此核对。记的是**实测数字**,不是期望值。
# ---------------------------------------------------------------------------
cat > "$OUT/MANIFEST.txt" <<MANIFEST
# sub2api + resin 迁移包
# 导出时刻: $(date -Iseconds)
# 来源: sub2api=${SUB2API_HOST:-localhost} resin=${RESIN_HOST:-localhost}
#
# 导入侧会逐项核对下面这些数字。对不上就是包损坏或导出不完整 ——
# 一个少了一半账号的数据库能正常启动,而你会在几天后才发现。
accounts=$PG_ACCOUNTS
users=$PG_USERS
api_keys=$PG_KEYS
postgres_sha256=$(shasum -a 256 "$OUT/postgres.sql.gz" | cut -d' ' -f1)
postgres_bytes=$(wc -c < "$OUT/postgres.sql.gz" | tr -d ' ')
resin_config_files=$(ls -1 "$OUT/resin-config" | wc -l | tr -d " ")
resin_platforms_sha256=$(shasum -a 256 "$OUT/resin-config/platforms.json" 2>/dev/null | cut -d" " -f1)
config_sha256=$(shasum -a 256 "$OUT/config.yaml" | cut -d' ' -f1)
MANIFEST

tar -C "$(dirname "$OUT")" -czf "$OUT.tar.gz" "$(basename "$OUT")"
rm -rf "$OUT"
chmod 600 "$OUT.tar.gz"

echo
echo "导出完成: $OUT.tar.gz ($(du -h "$OUT.tar.gz" | cut -f1))"
echo "  accounts=$PG_ACCOUNTS  users=$PG_USERS  api_keys=$PG_KEYS"
echo
echo "这个包里有明文凭据（数据库、resin token、账号 credentials）。"
echo "传输用 scp,不要放进任何对象存储或聊天工具;导入完成后删掉。"
