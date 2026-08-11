#!/usr/bin/env bash
# 本地镜像仓库：启动 registry 容器，并把 cloude 三件套镜像推入。
# 5000 端口常被系统服务占用，默认映射到 5001。
#
# 用法：
#   ./scripts/registry.sh                    # 启动 127.0.0.1:5001 并推送全部镜像
#   REGISTRY=192.168.1.10:5000 ./scripts/registry.sh   # 自定义 registry 地址
set -euo pipefail
cd "$(dirname "$0")/.."

REGISTRY="${REGISTRY:-localhost:5001}"
CONTAINER="${REGISTRY_CONTAINER:-cloude-registry}"
PORT="${REGISTRY_PORT:-5001}"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

step "启动本地 registry（${CONTAINER} -> 127.0.0.1:${PORT}）"
if docker inspect "$CONTAINER" >/dev/null 2>&1; then
  if ! docker inspect -f '{{.State.Running}}' "$CONTAINER" | grep -q true; then
    docker start "$CONTAINER"
  fi
  echo "registry 容器已存在并运行: $CONTAINER"
else
  docker run -d --name "$CONTAINER" -p "${PORT}:5000" \
    --restart unless-stopped registry:2
fi

# 控制面 / 网关镜像：推送 latest。
for img in cloude-control cloude-gateway; do
  if docker image inspect "${img}:local" >/dev/null 2>&1; then
    step "推送 ${img}:local -> ${REGISTRY}/${img}:latest"
    docker tag "${img}:local" "${REGISTRY}/${img}:latest"
    docker push "${REGISTRY}/${img}:latest"
  else
    echo "跳过 ${img}:local（本机没有该镜像）"
  fi
done

# agent 镜像：推送版本标签 + latest（版本号与 agent-image.sh 保持一致，可用 QPW_VERSION 覆盖）。
QPW_VERSION="${QPW_VERSION:-2.0.1}"
if docker image inspect cloude-agent:local >/dev/null 2>&1; then
  step "推送 cloude-agent:local -> ${REGISTRY}/cloude-agent:${QPW_VERSION} / :latest"
  docker tag cloude-agent:local "${REGISTRY}/cloude-agent:${QPW_VERSION}"
  docker tag cloude-agent:local "${REGISTRY}/cloude-agent:latest"
  docker push "${REGISTRY}/cloude-agent:${QPW_VERSION}"
  docker push "${REGISTRY}/cloude-agent:latest"
else
  echo "本机没有 cloude-agent:local，请先运行 ./scripts/agent-image.sh" >&2
  exit 1
fi

step "完成。仓库地址: ${REGISTRY}"
