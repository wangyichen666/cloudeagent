import { useCallback, useEffect, useRef, useState } from 'react'
import { api, openChatWS, type WSEvent } from './api'
import type {
  AgentInstance,
  ChatMessage,
  ConnectInfo,
  KernelInfo,
  ModelConfig,
} from './types'

const ADMIN_KEY = 'cloude-admin-token'

export default function App() {
  const [adminToken, setAdminToken] = useState(() => localStorage.getItem(ADMIN_KEY) || 'dev-admin-token')
  const [agents, setAgents] = useState<AgentInstance[]>([])
  const [selected, setSelected] = useState<string>('')
  const [newUserID, setNewUserID] = useState('')
  const [busy, setBusy] = useState<string>('')
  const [error, setError] = useState('')

  const refresh = useCallback(async () => {
    try {
      const data = await api.listAgents(adminToken)
      setAgents(data.instances)
      setError('')
    } catch (e) {
      setError((e as Error).message)
    }
  }, [adminToken])

  useEffect(() => {
    void refresh()
    const timer = setInterval(() => void refresh(), 5000)
    return () => clearInterval(timer)
  }, [refresh])

  const saveToken = (t: string) => {
    setAdminToken(t)
    localStorage.setItem(ADMIN_KEY, t)
  }

  const create = async () => {
    const id = newUserID.trim()
    if (!id) return
    setBusy('create')
    setError('')
    try {
      const inst = await api.createAgent(adminToken, id)
      setSelected(inst.user_id)
      setNewUserID('')
      await refresh()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy('')
    }
  }

  const act = async (action: string, fn: () => Promise<unknown>) => {
    setBusy(action)
    setError('')
    try {
      await fn()
      await refresh()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy('')
    }
  }

  const current = agents.find((a) => a.user_id === selected)

  return (
    <div className="app">
      <header className="topbar">
        <h1>CloudeAgent 控制台</h1>
        <label className="token">
          管理 Token
          <input
            type="password"
            value={adminToken}
            onChange={(e) => saveToken(e.target.value)}
            placeholder="dev-admin-token"
          />
        </label>
      </header>

      {error && <div className="banner error">{error}</div>}

      <div className="layout">
        <aside className="sidebar">
          <div className="create">
            <input
              value={newUserID}
              onChange={(e) => setNewUserID(e.target.value)}
              placeholder="用户 ID，如 u-1001"
            />
            <button onClick={create} disabled={busy === 'create' || !newUserID.trim()}>
              {busy === 'create' ? '创建中…' : '创建 Agent'}
            </button>
          </div>
          <div className="agent-list">
            {agents.length === 0 && <p className="empty">还没有 Agent，先创建一个吧。</p>}
            {agents.map((a) => (
              <div
                key={a.user_id}
                className={`agent-item ${a.user_id === selected ? 'active' : ''}`}
                onClick={() => setSelected(a.user_id)}
              >
                <div className="agent-name">{a.user_id}</div>
                <span className={`status status-${a.status}`}>{a.status}</span>
              </div>
            ))}
          </div>
        </aside>

        <main className="content">
          {!current ? (
            <div className="placeholder">
              <p>选择或创建一个 Agent，开始与 Pod 内的编码 Agent 对话。</p>
            </div>
          ) : (
            <AgentPanel
              key={current.user_id}
              agent={current}
              adminToken={adminToken}
              busy={busy}
              onError={setError}
              onAct={act}
              onDeleted={() => {
                setSelected('')
              }}
            />
          )}
        </main>
      </div>
    </div>
  )
}

function AgentPanel(props: {
  agent: AgentInstance
  adminToken: string
  busy: string
  onError: (msg: string) => void
  onAct: (action: string, fn: () => Promise<unknown>) => void
  onDeleted: () => void
}) {
  const { agent, adminToken, busy, onError, onAct, onDeleted } = props
  const [kernel, setKernel] = useState<KernelInfo | null>(null)
  const [connectInfo, setConnectInfo] = useState<ConnectInfo | null>(null)
  const [models, setModels] = useState<ModelConfig | null>(null)
  const [modelForm, setModelForm] = useState({ base_url: '', api_key: '', model: '', provider: '' })
  const [showModels, setShowModels] = useState(false)
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [input, setInput] = useState('')
  const [wsOpen, setWsOpen] = useState(false)
  const wsRef = useRef<WebSocket | null>(null)
  const chatBottom = useRef<HTMLDivElement>(null)

  const loadDetail = useCallback(async () => {
    try {
      const info = await api.connect(adminToken, agent.user_id)
      setConnectInfo(info)
      setKernel(info.kernel)
      const hist = await api.history(adminToken, agent.user_id)
      setMessages(
        hist.messages.map((m, i) => ({
          id: m.index || i,
          role: m.role === 'user' ? 'user' : m.role === 'error' ? 'error' : 'assistant',
          content: m.content,
          ts: m.ts,
          model: m.model,
        })),
      )
      onError('')
    } catch (e) {
      onError((e as Error).message)
    }
  }, [adminToken, agent.user_id, onError])

  useEffect(() => {
    void loadDetail()
  }, [loadDetail])

  useEffect(() => {
    chatBottom.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages])

  useEffect(() => {
    if (!connectInfo?.ws_url) return
    const ws = openChatWS(
      connectInfo.ws_url,
      (ev) => onWSEvent(ev),
      (open) => setWsOpen(open),
    )
    wsRef.current = ws
    return () => {
      ws.close()
      wsRef.current = null
    }
  }, [connectInfo?.ws_url]) // eslint-disable-line react-hooks/exhaustive-deps

  const onWSEvent = (ev: WSEvent) => {
    switch (ev.type) {
      case 'delta':
        setMessages((prev) => {
          const copy = [...prev]
          const last = copy[copy.length - 1]
          if (last && last.role === 'assistant' && last.streaming) {
            copy[copy.length - 1] = { ...last, content: last.content + ev.text }
          } else {
            copy.push({
              id: Date.now(),
              role: 'assistant',
              content: ev.text,
              ts: new Date().toISOString(),
              streaming: true,
            })
          }
          return copy
        })
        break
      case 'thought':
        // 思考内容逐 token 流式到达：累积进同一个思考块，而不是每条新建。
        setMessages((prev) => {
          const copy = [...prev]
          const last = copy[copy.length - 1]
          if (last && last.role === 'system' && last.streaming) {
            copy[copy.length - 1] = { ...last, content: last.content + ev.text }
          } else {
            copy.push({
              id: Date.now(),
              role: 'system',
              content: `思考：${ev.text}`,
              ts: new Date().toISOString(),
              streaming: true,
            })
          }
          return copy
        })
        break
      case 'tool_call':
        setMessages((prev) => [
          ...prev,
          {
            id: Date.now(),
            role: 'system',
            content: `工具调用：${ev.title || ev.toolCallId || '?'}`,
            ts: new Date().toISOString(),
          },
        ])
        break
      case 'done':
        setMessages((prev) => {
          const copy = [...prev]
          const last = copy[copy.length - 1]
          if (last && last.streaming) {
            copy[copy.length - 1] = { ...last, streaming: false }
          }
          return copy
        })
        break
      case 'error':
        setMessages((prev) => [
          ...prev,
          { id: Date.now(), role: 'error', content: ev.message, ts: new Date().toISOString() },
        ])
        break
    }
  }

  const send = () => {
    const text = input.trim()
    if (!text) return
    if (!wsRef.current || wsRef.current.readyState !== WebSocket.OPEN) {
      onError('尚未连接 Agent，请先点击「连接」')
      return
    }
    setMessages((prev) => [
      ...prev,
      { id: Date.now(), role: 'user', content: text, ts: new Date().toISOString() },
    ])
    wsRef.current.send(JSON.stringify({ type: 'chat', message: text }))
    setInput('')
  }

  const loadModels = async () => {
    try {
      const cfg = await api.getModels(adminToken, agent.user_id)
      setModels(cfg)
      setModelForm({
        base_url: cfg.base_url === 'mock://' ? '' : cfg.base_url,
        api_key: '',
        model: cfg.model,
        provider: cfg.provider || '',
      })
      setShowModels(true)
    } catch (e) {
      onError((e as Error).message)
    }
  }

  const saveModels = async () => {
    const cfg: ModelConfig = {
      base_url: modelForm.base_url.trim() || 'mock://',
      model: modelForm.model.trim(),
      provider: modelForm.provider.trim() || 'custom',
    }
    if (modelForm.api_key.trim()) cfg.api_key = modelForm.api_key.trim()
    if (!cfg.model) {
      onError('请填写模型名称')
      return
    }
    try {
      await api.setModels(adminToken, agent.user_id, cfg)
      await loadModels()
    } catch (e) {
      onError((e as Error).message)
    }
  }

  const doConnect = async () => {
    await onAct('connect', async () => {
      const info = await api.connect(adminToken, agent.user_id)
      setConnectInfo(info)
      setKernel(info.kernel)
    })
  }

  return (
    <div className="panel">
      <div className="agent-header">
        <div>
          <h2>{agent.user_id}</h2>
          <span className={`status status-${agent.status}`}>{agent.status}</span>
          <code className="endpoint">{agent.endpoint}</code>
        </div>
        <div className="actions">
          <button onClick={doConnect} disabled={busy === 'connect'}>
            {busy === 'connect' ? '连接中…' : '连接 Agent'}
          </button>
          <button
            disabled={agent.status !== 'suspended' || busy === 'wake'}
            onClick={() => onAct('wake', () => api.wake(adminToken, agent.user_id))}
          >
            唤醒
          </button>
          <button
            disabled={agent.status !== 'running' || busy === 'suspend'}
            onClick={() => onAct('suspend', () => api.suspend(adminToken, agent.user_id))}
          >
            休眠
          </button>
          <button
            className="danger"
            disabled={busy === 'delete'}
            onClick={() =>
              onAct('delete', async () => {
                await api.deleteAgent(adminToken, agent.user_id)
                onDeleted()
              })
            }
          >
            删除
          </button>
        </div>
      </div>

      <div className="kernel">
        <span>内核：{kernel?.connected ? '已连接' : '未连接'}</span>
        <span>
          {kernel?.agent || 'qwenpaw'} {kernel?.version || ''}
        </span>
        <span>WS：{wsOpen ? '已建立' : '未建立'}</span>
        {kernel?.reason && <span className="muted">（{kernel.reason}）</span>}
      </div>

      <div className="models">
        <button onClick={loadModels}>{showModels ? '刷新配置' : '模型配置'}</button>
        {models && (
          <span className="muted">
            当前模型：{models.model} @ {models.provider}
            {models.api_key ? ` · 密钥已保存 ${models.api_key}` : ' · 未配置密钥'}
          </span>
        )}
        {showModels && (
          <div className="model-form">
            <input
              placeholder="base_url（留空 = mock）"
              value={modelForm.base_url}
              onChange={(e) => setModelForm({ ...modelForm, base_url: e.target.value })}
            />
            <input
              placeholder="api_key"
              type="password"
              value={modelForm.api_key}
              onChange={(e) => setModelForm({ ...modelForm, api_key: e.target.value })}
            />
            {models?.api_key && (
              <span className="muted hint">已保存 {models.api_key}，留空则不修改</span>
            )}
            <input
              placeholder="model，如 qwen3-max"
              value={modelForm.model}
              onChange={(e) => setModelForm({ ...modelForm, model: e.target.value })}
            />
            <input
              placeholder="provider"
              value={modelForm.provider}
              onChange={(e) => setModelForm({ ...modelForm, provider: e.target.value })}
            />
            <button onClick={saveModels}>保存并热切换</button>
          </div>
        )}
      </div>

      <div className="chat">
        <div className="messages">
          {messages.length === 0 && <p className="empty">暂无对话。连接后发送消息，历史会自动保存在工作区。</p>}
          {messages.map((m) => {
            const isThought = m.role === 'system' && m.content.startsWith('思考：')
            return (
              <div key={m.id} className={`msg msg-${m.role}`}>
                <div className="msg-meta">
                  {m.role === 'user' ? '你' : m.role === 'assistant' ? 'Agent' : isThought ? '💭 思考' : '系统'}
                  {m.model ? ` · ${m.model}` : ''}
                </div>
                {isThought ? (
                  <details className="thought" open={m.streaming}>
                    <summary>{m.streaming ? '思考中…' : '思考'}</summary>
                    <div className="msg-body">
                      {m.content.slice(3)}
                      {m.streaming && <span className="cursor">▌</span>}
                    </div>
                  </details>
                ) : (
                  <div className="msg-body">
                    {m.content}
                    {m.streaming && <span className="cursor">▌</span>}
                  </div>
                )}
              </div>
            )
          })}
          <div ref={chatBottom} />
        </div>
        <div className="composer">
          <input
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && send()}
            placeholder="给 Pod 里的 Agent 发消息…"
            disabled={!wsOpen}
          />
          <button onClick={send} disabled={!wsOpen || !input.trim()}>
            发送
          </button>
        </div>
      </div>
    </div>
  )
}
