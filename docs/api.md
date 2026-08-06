# API 参考

Base URL：`http://127.0.0.1:8080`（默认端口，可用 `--listen` 修改）

## 鉴权

| 级别 | 凭证 | 位置 |
| --- | --- | --- |
| 管理面（创建/休眠/唤醒/删除/模型/评审） | Admin Token | `Authorization: Bearer <admin-token>` |
| 实例级会话（chat / WS） | Admin Token **或** 派生 token | `Authorization: Bearer ...` 或 `?token=` |

派生 token = `hex(sha256(namespace + ":" + userID))`，默认 namespace 为 `cloude-agent`。

## 端点

| 方法 | 路径 | 说明 | 鉴权 |
| --- | --- | --- | --- |
| `POST` | `/v1/users` | 创建实例 `{"id":"u-1001"}`（None→Running，幂等） | Admin |
| `GET` | `/v1/users` | 列出全部实例 | Admin |
| `GET` | `/v1/users/{id}` | 实例详情（状态/地址/工作区） | Admin |
| `DELETE` | `/v1/users/{id}` | 删除实例与工作区 | Admin |
| `POST` | `/v1/users/{id}/suspend` | 休眠（工作区保留） | Admin |
| `POST` | `/v1/users/{id}/wake` | 唤醒（重新注入模型配置） | Admin |
| `POST` | `/v1/users/{id}/models` | 热切换模型 `{"base_url","api_key","model","provider"}` | Admin |
| `GET` | `/v1/users/{id}/models` | 当前模型配置（api_key 掩码） | Admin |
| `POST` | `/v1/users/{id}/chat` | 同步对话 `{"message":"..."}` | Admin / 实例 token |
| `GET` | `/v1/users/{id}/session` | WebSocket 流式会话（需 `?token=`） | 实例 token |
| `GET` | `/v1/users/{id}/workspace` | 工作区文件与会话历史摘要 | Admin |
| `POST` | `/v1/users/{id}/reviews` | 提交异步评审 `{"repo":"<路径>","pr_number":42}` | Admin |
| `GET` | `/v1/users/{id}/reviews` | 评审列表 | Admin |
| `GET` | `/v1/users/{id}/reviews/{review_id}` | 评审详情（含 findings） | Admin |
| `GET` | `/healthz` | 控制面存活探针 | 无 |

## 错误格式

```json
{"error": {"code": "suspend_failed", "message": "实例当前状态为 suspended，无法休眠"}}
```

常见状态码：`401` 鉴权失败、`404` 实例不存在、`409` 状态冲突（如对已休眠实例对话，需先 wake）、`500` 内部错误。

## WebSocket 协议（/v1/users/{id}/session）

客户端 → 服务端：

```json
{"type": "chat", "message": "你好", "session_id": "可选"}
```

服务端 → 客户端：

```json
{"type": "delta", "text": "流式片段"}
{"type": "thought", "text": "QwenPaw 思考片段"}          // ACP agent_thought_chunk
{"type": "tool_call", "toolCallId": "…", "title": "…", "kind": "…"}  // ACP tool_call / tool_call_update
{"type": "usage", "used": 120, "size": 8192}              // ACP usage_update
{"type": "done", "session_id": "...", "message_index": 7, "model": "mock-gpt-4o", "mock": true}
{"type": "error", "message": "..."}
```

真实内核（QwenPaw ACP）下 `delta` 为逐增量推送；`mock:false`。mock 回退时
`delta` 为本地分片模拟，`mock:true`。

## Agent 内核状态（实例侧）

`GET /v1/users/{id}/workspace` 与 `GET /health` 返回 `kernel` 字段：

```json
{
  "kernel": {
    "name": "qwenpaw-acp",
    "enabled": true,
    "reason": "",
    "connected": true,
    "agent": "qwenpaw",
    "version": "2.0.4",
    "lastRestart": "2026-08-06T21:00:00+08:00"
  }
}
```

| 字段 | 说明 |
| --- | --- |
| `enabled` | 是否具备启用 ACP 内核的条件（qwenpaw >=2.0 且配置了真实模型） |
| `reason` | 未启用/连接失败的具体原因（如版本过低、mock 配置） |
| `connected` | qwenpaw acp 子进程是否已 initialize 握手成功 |
| `agent` / `version` | 内核 initialize 声明的名称与版本 |
| `lastRestart` | 最近一次子进程启动/重启时间（自愈可观测） |

实例参数 `--qwenpaw-bin`（或环境变量 `QWENPAW_BIN`）指定内核可执行文件，
默认 `qwenpaw`；不可用或版本过低时自动回退 mock LLM。

## 完整示例

```bash
B=http://127.0.0.1:8080
AUTH="Authorization: Bearer dev-admin-token"

curl -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"id":"u-1001"}' "$B/v1/users"

curl -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"message":"写一个 hello world"}' "$B/v1/users/u-1001/chat"

curl -X POST -H "$AUTH" "$B/v1/users/u-1001/suspend"
curl -X POST -H "$AUTH" "$B/v1/users/u-1001/wake"

curl -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"base_url":"https://api.openai.com","api_key":"sk-xxx","model":"gpt-4.1","provider":"openai"}' \
  "$B/v1/users/u-1001/models"

curl -X DELETE -H "$AUTH" "$B/v1/users/u-1001"
```
