package domain

import "time"

// Review 对应 code_reviews 表，由控制面的 CodeReview Worker 异步执行。
type Review struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Repo      string    `json:"repo"`
	PRNumber  int       `json:"pr_number"`
	Status    string    `json:"status"` // pending/running/completed/failed
	Model     string    `json:"model"`
	Findings  []Finding `json:"findings,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Finding 是本地 CodeReview 的实现产物（离线扫描 TODO/FIXME 等）。
type Finding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Level   string `json:"level"` // info/warning/error
	Message string `json:"message"`
}
