#!/usr/bin/env bash
# 本地一键演示：控制面 + 进程后端 + mock LLM + 内存存储，零外部依赖。
# 覆盖：创建 -> 会话 -> 休眠 -> 唤醒(数据不丢) -> 模型热切换 -> 代码评审 -> WS 流式 -> 删除。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${PORT:-18080}"
ADMIN_TOKEN="${ADMIN_TOKEN:-dev-admin-token}"
USER_ID="${USER_ID:-u-demo}"
NAMESPACE="${NAMESPACE:-cloude-agent}"
QPW_BIN="${QPW_BIN:-}"            # 设置后启用 QwenPaw ACP 真实内核（如 QPW_BIN=/usr/local/bin/qwenpaw）
DATA_DIR="$(pwd)/data/demo"
BIN_DIR="$(pwd)/bin"
CP_PID=""

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
api()  { # api METHOD PATH [BODY] [AUTH]
  local auth="${4:-Bearer $ADMIN_TOKEN}"
  curl -s -X "$1" -H "Authorization: $auth" -H "Content-Type: application/json" \
    "http://127.0.0.1:${PORT}$2" ${3:+-d "$3"}
}

cleanup() {
  [ -n "$CP_PID" ] && kill "$CP_PID" 2>/dev/null || true
  pkill -f "bin/agent-runtime --listen" 2>/dev/null || true
  rm -rf "$DATA_DIR"
}
trap cleanup EXIT

build() {
  mkdir -p "$BIN_DIR" "$DATA_DIR"
  if command -v go >/dev/null 2>&1; then
    go build -o "$BIN_DIR/control-plane" ./cmd/control-plane
    go build -o "$BIN_DIR/agent-runtime" ./cmd/agent-runtime
  else
    # 无 Go 时用容器交叉编译出宿主机可运行的二进制（macOS/Linux 通用）
    local goos goarch
    case "$(uname -s)" in
      Darwin) goos=darwin ;;
      Linux)  goos=linux ;;
      *)      goos=linux ;;
    esac
    case "$(uname -m)" in
      arm64|aarch64) goarch=arm64 ;;
      *)             goarch=amd64 ;;
    esac
    docker run --rm -v "$(pwd)":/app -w /app golang:1.24-alpine \
      sh -c "mkdir -p bin && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -o bin/control-plane ./cmd/control-plane && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -o bin/agent-runtime ./cmd/agent-runtime"
  fi
  ok "编译 control-plane / agent-runtime"
}

port_free() {
  python3 - "$PORT" <<'PY' 2>/dev/null
import socket, sys
s = socket.socket()
s.bind(("127.0.0.1", int(sys.argv[1])))
s.close()
PY
}

derive_token() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c "import hashlib,sys;print(hashlib.sha256((sys.argv[1]+':'+sys.argv[2]).encode()).hexdigest())" "$NAMESPACE" "$USER_ID"
  else
    printf '%s:%s' "$NAMESPACE" "$USER_ID" | shasum -a 256 | awk '{print $1}'
  fi
}

wait_ready() {
  for _ in $(seq 1 50); do
    curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "控制面未就绪" >&2
  exit 1
}

echo "==============================================================="
echo " CloudeAgent 本地方案演示（进程后端 + mock LLM + 内存存储）"
echo " 控制面: http://127.0.0.1:${PORT}  admin token: ${ADMIN_TOKEN}"
echo "==============================================================="

KERNEL_MODE="mock LLM（零外部依赖）"
if [ -n "${QWENPAW_BIN:-}" ] || { [ -n "$QPW_BIN" ] && command -v "$QPW_BIN" >/dev/null 2>&1; }; then
  QPW_BIN="${QWENPAW_BIN:-$QPW_BIN}"
  KERNEL_MODE="QwenPaw ACP 内核（$QPW_BIN）"
  export QWENPAW_BIN="$QPW_BIN"
fi
echo " 内核模式: ${KERNEL_MODE}"
echo "==============================================================="

if ! port_free; then
  echo "端口 ${PORT} 已被占用，请用 PORT=<其他端口> ./scripts/demo.sh" >&2
  exit 1
fi

build

step "启动控制面"
"$BIN_DIR/control-plane" \
  --listen "127.0.0.1:${PORT}" \
  --backend process \
  --store memory \
  --agent-bin "$BIN_DIR/agent-runtime" \
  --data-dir "$DATA_DIR" \
  --admin-token "$ADMIN_TOKEN" \
  --namespace "$NAMESPACE" >"$DATA_DIR/control-plane.log" 2>&1 &
CP_PID=$!
wait_ready
ok "控制面已就绪"

DERIVED="$(derive_token)"
echo "  实例派生 token: ${DERIVED:0:16}...（sha256(namespace:userID)，用于实例级会话）"

step "1. 创建用户实例（None -> Running）"
api POST "/v1/users" "{\"id\":\"${USER_ID}\"}" | jq '{user_id, instance_name, status, endpoint}'

# 真实内核模式：注入模型配置（凭证仅进实例内存，不落盘）
if [ -n "${QWENPAW_BIN:-}" ] && [ -n "${OPENAI_BASE_URL:-}" ]; then
  step "1b. 注入真实模型配置（QwenPaw ACP 内核通过 env 消费，凭证不落盘）"
  api POST "/v1/users/${USER_ID}/models" \
    "{\"base_url\":\"${OPENAI_BASE_URL}\",\"api_key\":\"${OPENAI_API_KEY}\",\"model\":\"${OPENAI_MODEL}\",\"provider\":\"openai\"}" \
    | jq '{ok, model}'
fi

step "2. 通过控制面统一入口对话（前端只连控制面，见文档 5.2 方案 A）"
api POST "/v1/users/${USER_ID}/chat" '{"message":"你好，介绍一下你自己"}' "Bearer $DERIVED" | jq '{reply, model, message_index, session_id}'
api POST "/v1/users/${USER_ID}/chat" '{"message":"今天天气如何？"}' "Bearer $DERIVED" | jq '{reply, message_index}'

step "3. 查看实例工作区（会话历史持久化在 .agent/conversation.jsonl）"
api GET "/v1/users/${USER_ID}/workspace" | jq '{workspace, kernel, files: [.files[].name], recent: (.recent | length)}'

step "4. 休眠实例（Running -> Suspended，工作区保留）"
api POST "/v1/users/${USER_ID}/suspend" | jq '{status, workspace}'

step "5. 唤醒实例（Suspended -> Running，挂回同一工作区）"
api POST "/v1/users/${USER_ID}/wake" | jq '{status, endpoint}'

step "6. 唤醒后再对话（消息序号连续增长 => 工作区数据在休眠/唤醒间不丢）"
api POST "/v1/users/${USER_ID}/chat" '{"message":"我上一句话是什么？"}' "Bearer $DERIVED" | jq '{reply, message_index}'

step "7. 模型配置热切换（无需重启实例）"
api POST "/v1/users/${USER_ID}/models" '{"base_url":"mock://","api_key":"sk-demo-switch","model":"mock-claude-sonnet","provider":"demo-hot-switch"}' | jq '{ok, model}'
api POST "/v1/users/${USER_ID}/chat" '{"message":"现在用哪个模型回答？"}' "Bearer $DERIVED" | jq '{reply, model, provider}'
echo "  当前模型配置（api_key 已掩码）:"
api GET "/v1/users/${USER_ID}/models" | jq .

step "8. 异步代码评审（CodeReview Worker）"
REPO_DIR="$DATA_DIR/demo-repo"
mkdir -p "$REPO_DIR/src"
printf 'package main\n\n// TODO: 这里缺少错误处理\nfunc main() {}\n// FIXME: 硬编码端口\n' >"$REPO_DIR/src/main.go"
printf '# TODO: 补充单元测试\n' >"$REPO_DIR/README.md"
REVIEW_ID=$(api POST "/v1/users/${USER_ID}/reviews" "{\"repo\":\"${REPO_DIR}\",\"pr_number\":42}" | jq -r '.id')
echo "  评审任务: $REVIEW_ID"
sleep 1
api GET "/v1/users/${USER_ID}/reviews/${REVIEW_ID}" | jq '{status, findings, model}'

step "9. WebSocket 流式会话（控制面按 userID 路由并转发）"
if command -v node >/dev/null 2>&1; then
  node scripts/ws-demo.js "ws://127.0.0.1:${PORT}/v1/users/${USER_ID}/session?token=${DERIVED}" | tail -n +1
else
  echo "  (未找到 node，跳过 WS 演示)"
fi

step "10. 删除实例（Running -> None，工作区一并销毁）"
api DELETE "/v1/users/${USER_ID}" | jq .
api GET "/v1/users" | jq '{total}'

step "清理"
ok "演示完成。控制面日志: $DATA_DIR/control-plane.log"
