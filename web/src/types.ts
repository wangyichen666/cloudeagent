export type InstanceStatus = 'none' | 'running' | 'suspended' | 'failed'

export interface AgentInstance {
  user_id: string
  instance_name: string
  status: InstanceStatus
  workspace: string
  endpoint: string
  error?: string
  updated_at?: string
}

export interface KernelInfo {
  name: string
  enabled: boolean
  connected: boolean
  reason?: string
  agent?: string
  version?: string
  lastRestart?: string
}

export interface ConnectInfo {
  user_id: string
  status: InstanceStatus
  endpoint: string
  workspace: string
  kernel: KernelInfo
  token: string
  ws_url: string
}

export interface ModelConfig {
  base_url: string
  api_key?: string
  model: string
  models?: string[]
  provider?: string
}

export interface HistoryMessage {
  ts: string
  role: string
  content: string
  model?: string
  index: number
}

export interface ChatMessage {
  id: number
  role: 'user' | 'assistant' | 'system' | 'error'
  content: string
  ts: string
  model?: string
  streaming?: boolean
}
