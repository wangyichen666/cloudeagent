# cloudeagent（Monorepo）

多租户 AI 编码 Agent 平台，单一仓库下三个独立项目：

| 项目 | 目录 | 职责 |
| --- | --- | --- |
| **cloud-backend** | [`cloud-backend/`](cloud-backend/README.md) | 业务系统：控制面 + Pod 内实例运行时（创建/休眠/唤醒、鉴权、评审、前端 API） |
| **cloud-gateway** | [`cloud-gateway/`](cloud-gateway/README.md) | 数据面网关：backend 与 Pod 内 Agent 之间唯一的连接层（路由注册 + REST/WS 转发） |
| **cloud-web** | [`cloud-web/`](cloud-web/README.md) | 前端控制台（Vite + React + TS） |

```
前端(cloud-web) → backend(cloud-backend) → gateway(cloud-gateway) → agent Pod（内置 QwenPaw）
```

## 快速开始

### cloud-backend

```bash
cd cloud-backend
./scripts/demo.sh          # 一键演示（自动构建并启动 gateway）
```

### cloud-gateway

```bash
cd cloud-gateway
go build -o bin/gateway ./cmd/gateway
./bin/gateway --listen 127.0.0.1:18500
```

### cloud-web

```bash
cd cloud-web
npm install
npm run dev                # http://localhost:5173
```

## 架构要点

- **backend 不直连 Pod**：创建/唤醒实例后把 endpoint 注册到 gateway，对话/会话/历史/配置全部经 gateway 转发；
- **凭证不落盘**：模型配置只驻留 backend 缓存与 agent 内存（emptyDir），休眠即丢、唤醒重新注入；
- 每个 Agent 一个 StatefulSet Pod，Pod 内为 agent-runtime + QwenPaw ACP 内核。

详细设计见 [`cloud-backend/docs/architecture.md`](cloud-backend/docs/architecture.md)。
