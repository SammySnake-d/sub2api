#!/usr/bin/env bash
# =============================================================================
# 迁移导出:把旧部署的全部状态打成一个包
# =============================================================================
#
# 在**旧机器**上跑。产出 migrate-bundle-<时间戳>.tar.gz,里面是:
#
#   postgres.sql.gz     sub2api 全库（账号/凭据/用量/审计）
#   resin-data/         resin 状态目录（cache.db / metrics.db / country.mmdb）
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

mkdir -p "$OUT/resin-data"
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
# 3. resin 状态目录
#
# 里面是 SQLite（cache.db 有 -wal/-shm 伴生文件）。直接 tar 整个目录,
# 让 WAL 一起过去 —— 只拷 .db 会丢掉尚未 checkpoint 的写入。
# ---------------------------------------------------------------------------
echo "[3/4] 导出 resin 状态 ..."
on "$RESIN_HOST" "sudo tar -C '$(dirname "$RESIN_DATA")' -cf - '$(basename "$RESIN_DATA")'" \
  | tar -C "$OUT/resin-data" -xf -

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
resin_data_bytes=$(du -sk "$OUT/resin-data" | cut -f1)
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
