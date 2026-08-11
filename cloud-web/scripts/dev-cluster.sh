#!/usr/bin/env bash
# 前端开发入口：保证连接的是「集群里的控制面」，而不是本地控制面。
#
# 规则：
#   - 18080 空闲        -> 自动拉起 kubectl port-forward（退出前端时自动清理）
#   - 18080 已被 kubectl -> 复用现有隧道，直接启动前端
#   - 18080 被其他进程   -> 报错退出（可能是本地控制面，避免误连本地进程后端）
#
# 本地进程后端调试请用：npm run dev:local（VITE_API_BASE 也可覆盖代理目标）。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${PORT:-18080}"
NAMESPACE="${NAMESPACE:-cloude-backend}"
RESOURCE="${RESOURCE:-deploy/control-plane}"
PF_LOG="${PF_LOG:-/tmp/cloude-port-forward.log}"

listener() {
  lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $1}'
}

pid=""
cleanup() {
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
    echo "已停止本次启动的 port-forward（$pid）"
  fi
}
trap cleanup EXIT

L="$(listener)"
case "$L" in
  "")
    echo "==> 18080 空闲，自动启动隧道: kubectl port-forward -n $NAMESPACE $RESOURCE ${PORT}:8080"
    nohup kubectl port-forward -n "$NAMESPACE" "$RESOURCE" "${PORT}:8080" >"$PF_LOG" 2>&1 &
    pid=$!
    for _ in $(seq 1 60); do
      curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1 && break
      sleep 0.5
    done
    if ! curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
      echo "隧道启动失败，日志见 $PF_LOG" >&2
      cat "$PF_LOG" >&2 2>/dev/null || true
      exit 1
    fi
    ;;
  kubectl)
    echo "==> 18080 已是集群控制面隧道（kubectl），直接复用"
    ;;
  *)
    echo "==> 18080 被进程「$L」占用，不是集群隧道；请先停止本地控制面再运行 npm run dev" >&2
    exit 1
    ;;
esac

echo "==> 后端: 集群控制面 (http://127.0.0.1:${PORT})"
exec ./node_modules/.bin/vite "$@"
