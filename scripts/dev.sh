#!/usr/bin/env bash
# Docker 后端开发模式：构建 agent 镜像，用每用户容器 + 命名卷模拟 StatefulSet+PVC。
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

step "构建 agent 实例镜像 cloude-agent:local"
docker build -q -f agent-image/Dockerfile -t cloude-agent:local .

step "构建控制面"
mkdir -p bin
if command -v go >/dev/null 2>&1; then
  go build -o bin/control-plane ./cmd/control-plane
else
  case "$(uname -s)" in Darwin) goos=darwin ;; *) goos=linux ;; esac
  case "$(uname -m)" in arm64|aarch64) goarch=arm64 ;; *) goarch=amd64 ;; esac
  docker run --rm -v "$(pwd)":/app -w /app golang:1.24-alpine \
    sh -c "CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -o bin/control-plane ./cmd/control-plane"
fi

step "启动控制面（--backend docker，Ctrl+C 退出）"
exec ./bin/control-plane \
  --listen "127.0.0.1:8080" \
  --backend docker \
  --store memory \
  --agent-image cloude-agent:local \
  --data-dir ./data \
  "$@"
