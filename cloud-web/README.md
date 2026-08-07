# cloud-web（CloudeAgent 前端控制台）

Vite + React + TypeScript 前端，对接 **cloud-backend** 四个核心接口：
创建 Agent、连接 Agent（WebSocket 流式对话）、持久化对话历史、新建会话，
以及模型热切换、休眠/唤醒/删除。

## 运行

```bash
npm install
npm run dev        # http://localhost:5173
```

开发代理把 `/v1` 转发到 `http://127.0.0.1:18080`（cloud-backend 控制面），
可用环境变量 `VITE_API_BASE` 覆盖。backend 在 kind 集群内时先：

```bash
kubectl port-forward -n cloude-control svc/control-plane-svc 18080:8080
```

登录后填入管理 Token（默认 `dev-admin-token`）即可使用。

## 架构

```
前端(cloud-web) → backend(cloud-backend) → gateway(cloud-gateway) → agent Pod
```
