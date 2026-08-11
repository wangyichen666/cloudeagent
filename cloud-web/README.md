# cloud-web（CloudeAgent 前端控制台）

Vite + React + TypeScript 前端，对接 **cloud-backend** 四个核心接口：
创建 Agent、连接 Agent（WebSocket 流式对话）、持久化对话历史、新建会话，
以及模型热切换、休眠/唤醒/删除。

## 运行

```bash
npm install
npm run dev        # http://localhost:5173（自动连接集群 backend）
npm run dev:local  # 连接本地控制面（本地进程后端，127.0.0.1:18080）
```

`npm run dev` 保证连的是 **kind 集群里的控制面**：18080 空闲时会自动拉起
`kubectl port-forward -n cloude-backend deploy/control-plane 18080:8080`，
已被 kubectl 隧道占用则直接复用；如果 18080 被本地控制面占用会报错提醒，
避免把 agent 误建到本地进程后端。前端退出时本次启动的隧道会自动清理。

开发代理把 `/v1` 转发到 `http://127.0.0.1:18080`，可用环境变量 `VITE_API_BASE` 覆盖。
集群内控制面的 NodePort Service（`control-plane-svc`，30080）见
`cloud-backend/deploy/k8s/09-control-plane-service.yaml`；kind 默认不把 NodePort
暴露到宿主机，所以本地开发走 port-forward 隧道。

登录后填入管理 Token（默认 `dev-admin-token`）即可使用。

## 架构

```
前端(cloud-web) → backend(cloud-backend) → gateway(cloud-gateway) → agent Pod
```
