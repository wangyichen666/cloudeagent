import type { AgentInstance, ConnectInfo, HistoryMessage, ModelConfig } from './types'

// 同源（Vite 代理）或通过 VITE_API_BASE 指定控制面地址。
const BASE = (import.meta.env.VITE_API_BASE as string | undefined) || ''

async function request<T>(
  method: string,
  path: string,
  token: string,
  body?: unknown,
): Promise<T> {
  const resp = await fetch(BASE + path, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (!resp.ok) {
    let message = `${method} ${path} -> ${resp.status}`
    try {
      const data = await resp.json()
      message = data?.error?.message || data?.message || message
    } catch {
      /* keep status message */
    }
    throw new Error(message)
  }
  return (await resp.json()) as T
}

export const api = {
  listAgents: (token: string) => request<{ instances: AgentInstance[] }>('GET', '/v1/users', token),
  createAgent: (token: string, id: string) =>
    request<AgentInstance>('POST', '/v1/users', token, { id }),
  getAgent: (token: string, id: string) => request<AgentInstance>('GET', `/v1/users/${id}`, token),
  deleteAgent: (token: string, id: string) => request<{ ok: boolean }>('DELETE', `/v1/users/${id}`, token),
  suspend: (token: string, id: string) => request<AgentInstance>('POST', `/v1/users/${id}/suspend`, token),
  wake: (token: string, id: string) => request<AgentInstance>('POST', `/v1/users/${id}/wake`, token),
  connect: (token: string, id: string) => request<ConnectInfo>('GET', `/v1/users/${id}/connect`, token),
  newSession: (token: string, id: string) =>
    request<{ ok: boolean; session_id: string }>('POST', `/v1/users/${id}/sessions`, token),
  getModels: (token: string, id: string) => request<ModelConfig>('GET', `/v1/users/${id}/models`, token),
  setModels: (token: string, id: string, cfg: ModelConfig) =>
    request<{ ok: boolean; model: string }>('POST', `/v1/users/${id}/models`, token, cfg),
  history: (token: string, id: string, limit = 200, sessionId?: string) =>
    request<{ messages: HistoryMessage[]; total: number; path: string }>(
      'GET',
      `/v1/users/${id}/history?limit=${limit}${sessionId ? `&session_id=${encodeURIComponent(sessionId)}` : ''}`,
      token,
    ),
  chat: (token: string, id: string, message: string) =>
    request<{ reply: string; mock: boolean; message_index: number }>(
      'POST',
      `/v1/users/${id}/chat`,
      token,
      { message },
    ),
}

export type WSEvent =
  | { type: 'delta'; text: string }
  | { type: 'thought'; text: string }
  | { type: 'tool_call'; toolCallId?: string; title?: string; status?: string }
  | { type: 'usage'; used?: number; size?: number }
  | { type: 'done'; session_id?: string; message_index?: number; model?: string; mock?: boolean }
  | { type: 'error'; message: string }

export function openChatWS(
  wsUrl: string,
  onEvent: (ev: WSEvent) => void,
  onState: (open: boolean) => void,
): WebSocket {
  const ws = new WebSocket(wsUrl)
  ws.onopen = () => onState(true)
  ws.onclose = () => onState(false)
  ws.onerror = () => onState(false)
  ws.onmessage = (ev) => {
    try {
      onEvent(JSON.parse(ev.data as string) as WSEvent)
    } catch {
      /* ignore non-JSON */
    }
  }
  return ws
}
