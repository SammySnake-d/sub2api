#!/usr/bin/env bash
# =============================================================================
# 给 docker 映射的端口加 IP 白名单（DOCKER-USER 链）
# =============================================================================
#
# ## 为什么需要这个脚本：ufw 管不住 docker
#
# 这台机器 ufw 是 active 且 `INPUT policy DROP`，看起来一切都被默认拒绝了。
# **但 docker 映射到 0.0.0.0 的端口完全不受它约束**：docker 自己往 nat 表写
# DNAT 规则、并在 FORWARD 链上放行，流量根本不经过 ufw 所在的 INPUT 链。
#
# 实证（2026-09-16，本机）：`iptables -L INPUT` 是 DROP，ufw 里 2222 只放通了
# 12 个白名单 IP，而 `0.0.0.0:80`、`0.0.0.0:443`、`0.0.0.0:18082` 三个 docker
# 端口对全世界可达。`DOCKER-USER` 链当时是空的。
#
# `DOCKER-USER` 是 docker 专门留给管理员的钩子：它在 docker 自己的规则**之前**
# 被求值，且 docker 重启/重建容器时不会清空它。所以这是唯一正确的落点。
#
# ## 这个脚本管什么、不管什么
#
# 只管**本脚本显式列出的端口**。它不会碰 80/443（iCloud 服务需要对公众开放），
# 也不会碰别的项目的端口。规则按注释标记归属，重复执行只会先删自己的再加，
# 幂等，且不影响任何非本脚本写的规则。
#
# 用法：
#   bash ufw-docker-allowlist.sh apply     # 应用
#   bash ufw-docker-allowlist.sh show      # 查看当前规则
#   bash ufw-docker-allowlist.sh clear     # 只删本脚本加的，别人的不动
# =============================================================================
set -euo pipefail

# 受保护的端口 → 允许访问的来源。
# 留空表示"公网全开"，此时脚本不为它添加任何规则（docker 默认行为即全开）。
#
# resin 的管理面没有登录、只有一个静态 admin token，拿到它就能改订阅源、改平台
# 过滤、读出所有租约与出口 IP —— 所以它走白名单，而不是"反正有 token"。
PROTECTED_PORT="${PROTECTED_PORT:-2260}"

# 与 SSH 白名单同源。改这里之前先想清楚：多一个 IP 就多一份出口池控制权。
ALLOWED_IPS="${ALLOWED_IPS:-
203.175.14.54
5.34.216.86
203.175.14.32
58.152.118.247
203.175.14.58
103.175.98.76
202.8.9.242
163.61.198.12
36.227.251.89
103.152.113.90
203.10.99.42
}"

MARK="sub2api-stack-allowlist"

require_root() { [ "$(id -u)" -eq 0 ] || { echo "需要 root" >&2; exit 1; }; }

clear_rules() {
  # 只删带本脚本标记的规则。从后往前删，避免行号在删除过程中漂移。
  local n
  while read -r n; do
    [ -n "$n" ] && iptables -D DOCKER-USER "$n" 2>/dev/null || true
  done < <(iptables -L DOCKER-USER -n --line-numbers 2>/dev/null \
            | grep -F "$MARK" | awk '{print $1}' | sort -rn)
}

apply_rules() {
  require_root
  iptables -L DOCKER-USER -n >/dev/null 2>&1 || {
    echo "DOCKER-USER 链不存在 —— docker 没在跑？" >&2; exit 1; }

  clear_rules

  local count=0
  while read -r ip; do
    ip="$(echo "$ip" | tr -d '[:space:]')"
    [ -n "$ip" ] || continue
    # -I 插到最前面：DOCKER-USER 是顺序求值的，放行规则必须在兜底 DROP 之前。
    iptables -I DOCKER-USER 1 -p tcp --dport "$PROTECTED_PORT" -s "$ip" \
      -m comment --comment "$MARK allow $ip" -j RETURN
    count=$((count+1))
  done <<< "$ALLOWED_IPS"

  # 兜底 DROP 必须**最后插入到白名单之后**。这里用 -A（追加到链尾）而不是 -I，
  # 因为链尾之后是 docker 自己的规则，追加在那之前正好。
  iptables -A DOCKER-USER -p tcp --dport "$PROTECTED_PORT" \
    -m comment --comment "$MARK deny others" -j DROP

  echo "已为端口 $PROTECTED_PORT 应用白名单：$count 个 IP 放行，其余 DROP"
  echo
  echo "注意：iptables 规则**不持久**，重启后消失。要持久化："
  echo "  apt-get install -y iptables-persistent && netfilter-persistent save"
  echo "或把本脚本挂成开机执行的 systemd unit。"
}

show_rules() {
  echo "=== DOCKER-USER 链当前规则 ==="
  iptables -L DOCKER-USER -n -v --line-numbers 2>/dev/null || echo "（读不到，需要 root）"
  echo
  echo "=== 公网可达的 docker 端口（这些都绕过了 ufw）==="
  docker ps --format '{{.Names}}\t{{.Ports}}' 2>/dev/null | grep -E '0\.0\.0\.0|\[::\]' || echo "（无）"
}

case "${1:-show}" in
  apply) apply_rules ;;
  clear) require_root; clear_rules; echo "已清除本脚本添加的规则（其他规则未动）" ;;
  show)  show_rules ;;
  *)     echo "用法: $0 {apply|clear|show}" >&2; exit 1 ;;
esac
