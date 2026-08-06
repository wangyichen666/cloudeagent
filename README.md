# CloudeAgent 本地方案

> 一套**多租户、按需编排、有状态**的 AI 编码 Agent 平台的本地实现。
> 代码与设计完全对照需求文档的 7 大北极星：多租户隔离 / 按需编排 / 有状态 / 可路由 / 动态配置 / 安全 / 可观测+自愈。

本仓库把「Kubernetes + Go 控制面 + StatefulSet 每用户一实例」的云端架构，落成了一个**可以在这台机器上直接跑起来**的分层实现：

| 平面 | 云端设计（需求文档） | 本地方案 |
| --- | --- | --- |
| 控制面 | 无状态 Go 服务：API / 编排 / 鉴权 / 路由 | `cmd/control-plane`（Go，同一套逻辑） |
| 数据面 | K8s 每用户一个 StatefulSet Pod | 三选一：本地进程 / Docker 容器 / K8s StatefulSet |
| 状态面 | PostgreSQL + Redis | 内存（默认，零依赖）/ PostgreSQL + Redis（生产语义） |
| Agent 运行时 | 承载编码会话的 daemon | `cmd/agent-runtime`：健康检查、会话、配置热重载 |
| 模型接入 | 席位服务动态注入 baseURL/apiKey | mock LLM 开箱即用；任意 OpenAI 兼容端点可热切换 |

```
┌────────────────────────────┐
│  curl / 前端（只连控制面）   │
└─────────────┬──────────────┘
              │ REST + WebSocket（文档 5.2 方案 A：Pod 不暴露）
┌─────────────▼──────────────┐
│  控制面 control-plane       │
│  ├ 生命周期状态机（None↔Running↔Suspended）
│  ├ 按 userID 路由（稳定端口 / Pod DNS）
│  ├ 模型配置注入 + 热切换（凭证不落盘）
│  ├ Idle Reaper 自动休眠
│  ├ CodeReview Worker（异步评审）
│  └ 鉴权：Admin Token / sha256(ns:userID) 派生 token
└─────┬──────────────┬───────┘
      │ 编排          │ 元数据/热状态
┌─────▼──────┐  ┌─────▼──────┐
│  数据面     │  │  状态面     │
│ process /  │  │ memory /   │
│ docker /   │  │ postgres + │
│ k8s        │  │ redis      │
└────────────┘  └────────────┘
```

## 快速开始（零外部依赖，约 30 秒）

```bash
./scripts/demo.sh
```

脚本会自动完成：编译 → 启动控制面 → 创建用户实例 → 对话 → 查看工作区 → 休眠 → 唤醒（消息序号连续，数据不丢）→ 模型热切换 → 异步代码评审 → WebSocket 流式会话 → 删除实例。

没有安装 Go 也没关系：脚本会自动用 Docker 交叉编译出宿主机可运行的二进制（macOS/Linux）。

## 三种运行模式

| 模式 | 数据面 | 状态面 | 适合 |
| --- | --- | --- | --- |
| `make demo` | 本地进程 + 工作区目录 | 内存 | 快速体验、开发调试 |
| `./scripts/dev.sh` | Docker 容器 + 命名卷 | 内存 | 最接近「StatefulSet + PVC」语义的本地验证 |
| `control-plane-k8s` | K8s StatefulSet + PVC | PostgreSQL + Redis | 生产/预发（见 [K8s 生产路径](#k8s-生产路径)） |

手动启动控制面：

```bash
make build
./bin/control-plane \
  --backend process \                 # 或 docker
  --store memory \                    # 或 postgres --dsn "postgres://..."
  --redis-addr 127.0.0.1:6379 \       # 可选
  --agent-bin ./bin/agent-runtime \
  --admin-token dev-admin-token
```

启用 PostgreSQL + Redis 状态面（对应文档状态面设计）：

```bash
docker compose up -d postgres redis
./bin/control-plane --store postgres \
  --dsn "postgres://cloude:cloude@127.0.0.1:5432/cloude?sslmode=disable" \
  --redis-addr 127.0.0.1:6379
```

## API 速览（完整见 [docs/api.md](docs/api.md)）

所有管理面请求带 `Authorization: Bearer <admin-token>`；会话级请求可用每个用户派生的实例 token。

```bash
ADMIN="Authorization: Bearer dev-admin-token"

# 创建用户实例（None -> Running）
curl -X POST -H "$ADMIN" -H 'Content-Type: application/json' \
  -d '{"id":"u-1001"}' http://127.0.0.1:8080/v1/users

# 对话（前端唯一入口，控制面路由到该用户的实例）
curl -X POST -H "$ADMIN" -H 'Content-Type: application/json' \
  -d '{"message":"你好"}' http://127.0.0.1:8080/v1/users/u-1001/chat

# 休眠 / 唤醒 / 删除
curl -X POST -H "$ADMIN" http://127.0.0.1:8080/v1/users/u-1001/suspend
curl -X POST -H "$ADMIN" http://127.0.0.1:8080/v1/users/u-1001/wake
curl -X DELETE -H "$ADMIN" http://127.0.0.1:8080/v1/users/u-1001

# 模型热切换（任意 OpenAI 兼容端点，无需重启实例）
curl -X POST -H "$ADMIN" -H 'Content-Type: application/json' \
  -d '{"base_url":"https://api.example.com","api_key":"sk-...","model":"gpt-4.1","provider":"example"}' \
  http://127.0.0.1:8080/v1/users/u-1001/models

# WebSocket 流式会话（使用派生 token）
node scripts/ws-demo.js "ws://127.0.0.1:8080/v1/users/u-1001/session?token=<sha256(namespace:userID)>"
```

## 目录结构

```
cmd/control-plane/        控制面入口（backend/store 可切换）
cmd/agent-runtime/        Agent 实例运行时入口
internal/controlplane/    状态机、路由、鉴权、Reaper、评审 Worker、HTTP/WS
internal/backend/         数据面抽象 + process / docker 实现
internal/agent/           运行时：配置热重载、LLM 客户端 + mock、会话持久化
internal/store/           状态面抽象 + 内存 / PostgreSQL 实现 + 热状态缓存
internal/models/          数据模型（对齐文档八）
k8sbackend/               生产路径：K8s StatefulSet 后端 + control-plane-k8s 入口
agent-image/Dockerfile    数据面实例镜像
deploy/k8s/               命名空间 / Headless Service / NetworkPolicy / RBAC / Secret 占位
migrations/001_init.sql   状态面表结构（对应文档数据模型）
scripts/demo.sh           一键演示
scripts/dev.sh            Docker 后端开发模式
scripts/ws-demo.js        WebSocket 流式会话演示
```

## 安全设计要点（文档七的实现）

- **凭证不落盘**：apiKey 只在实例内存 / Redis 热缓存（带 TTL），休眠即丢弃、唤醒重新注入；工作区（PVC/目录/卷）与 PostgreSQL 里都没有明文凭证。
- **实例不对外暴露**：前端只连控制面，控制面按 userID 拼稳定地址路由（本地稳定端口 / K8s Pod DNS），文档 5.2 方案 A。
- **最小权限**：`deploy/k8s/05-rbac.yaml` 控制面 SA 只授 StatefulSet/Pod/PVC/exec 必要动词；`04-network-policy.yaml` 默认拒绝，只放行控制面→数据面与出站 DNS/模型 API。
- **容器加固**：非 root、`allowPrivilegeEscalation: false`、`drop: ALL`、只读根文件系统（见 k8sbackend 与部署清单）。
- **身份派生**：实例 token = `sha256(namespace:userID)`，前端凭它连自己的会话，无需管理凭证。

## K8s 生产路径

`k8sbackend/` 实现了文档 4.1 的推荐方案：**每用户一个 StatefulSet（replicas 0/1）+ Headless Service + PVC**，与本地后端实现完全相同的编排语义（Create/Suspend/Wake/Delete、等 Pod Ready、Pod DNS 路由）。

```bash
cd k8sbackend && go build -o ../bin/control-plane-k8s ./cmd/control-plane
./bin/control-plane-k8s \
  --kubeconfig ~/.kube/config \
  --agent-image cloude-agent:local \
  --namespace cloude-agent \
  --dsn "postgres://..." \
  --redis-addr 10.0.0.8:6379
```

集群侧清单见 `deploy/k8s/`：命名空间隔离、Headless Service、NetworkPolicy、RBAC、Secret 占位。实例镜像用 `docker build -f agent-image/Dockerfile -t cloude-agent:local .` 构建后推送到私有仓库（文档选型 Harbor）。

## 文档

- [架构设计：本地方案 ↔ 云端设计逐项映射](docs/architecture.md)
- [API 参考](docs/api.md)

## 已知边界（有意为之）

- CodeReview Worker 的本地实现扫描本地目录的 TODO/FIXME；GitHub/GitLab PR 评审需接入 VCS OAuth（数据表已预留 `user_vcs_tokens`）。
- mock LLM 默认开箱即用；连真实模型只需 `POST /models` 传入 OpenAI 兼容的 base_url/apiKey。
- 进程后端的多副本/分布式锁用进程内互斥替代；生产模式启用 Redis + PostgreSQL 后即为多副本就绪。

# cloudeagent
