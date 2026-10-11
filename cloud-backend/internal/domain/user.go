package domain

// VCSToken 对应 user_vcs_tokens 表（生产环境加密存储）。
type VCSToken struct {
	UserID   string `json:"user_id"`
	Platform string `json:"platform"` // github/gitlab
	Token    string `json:"token"`
}
