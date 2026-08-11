#!/usr/bin/env bash
# 构建 agent 实例镜像（内置 QwenPaw ACP 内核），可选推送到本地/私有 registry。
#
# 用法：
#   ./scripts/agent-image.sh                          # 仅构建 cloude-agent:local
#   REGISTRY=localhost:5001 ./scripts/agent-image.sh  # 构建后推送 2.0.1 / latest
#
# 环境变量：
#   QPW_VERSION  qwenpaw 版本（默认 2.0.1，ACP --runtime-provider 需要 >=2.0）
#   REGISTRY     目标 registry，如 localhost:5001；不设置则只构建不推送
set -euo pipefail
cd "$(dirname "$0")/.."

QPW_VERSION="${QPW_VERSION:-2.0.1}"
REGISTRY="${REGISTRY:-}"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

step "构建 cloude-agent:local（qwenpaw=${QPW_VERSION}）"
docker build --build-arg "QPW_VERSION=${QPW_VERSION}" \
  -t cloude-agent:local -f agent-image/Dockerfile .

if [ -n "$REGISTRY" ]; then
  REG="${REGISTRY%/}"
  step "推送镜像到 ${REG}"
  docker tag cloude-agent:local "${REG}/cloude-agent:${QPW_VERSION}"
  docker tag cloude-agent:local "${REG}/cloude-agent:latest"
  docker push "${REG}/cloude-agent:${QPW_VERSION}"
  docker push "${REG}/cloude-agent:latest"
  echo "已推送: ${REG}/cloude-agent:${QPW_VERSION} 和 ${REG}/cloude-agent:latest"
else
  echo "未设置 REGISTRY，跳过推送。可用: REGISTRY=localhost:5001 $0"
fi
