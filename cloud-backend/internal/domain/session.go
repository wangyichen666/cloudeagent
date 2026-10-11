package domain

// ChatRequest 是会话输入。
type ChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"`
}

// ChatResponse 是控制面与实例之间的会话应答（REST 与 WS done 消息共用）。
type ChatResponse struct {
	Reply        string `json:"reply"`
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	Mock         bool   `json:"mock"`
	MessageIndex int    `json:"message_index"`
	SessionID    string `json:"session_id"`
}
