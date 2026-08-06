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
{"type": "done", "session_id": "...", "message_index": 7, "model": "mock-gpt-4o", "mock": true}
{"type": "error", "message": "..."}
```

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
