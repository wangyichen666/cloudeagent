package store

import (
	"context"
	"errors"
	"time"

	"cloude-agent/internal/models"
)

// ErrNotFound 表示记录不存在（控制面据此区分 404 与 500）。
var ErrNotFound = errors.New("store: not found")

// Activity 对应 agent_activity 表。
type Activity struct {
	UserID        string
	LastActiveAt  time.Time
	WSConnections int
}

// Store 是状态面（State Plane）的抽象：
// 本地方案默认用内存实现（零依赖），生产路径用 PostgreSQL 实现，
// 二者都保持「用户 ↔ 实例 ↔ 状态 ↔ 评审记录」这套事实来源语义一致。
type Store interface {
	// 实例映射
	CreateInstance(ctx context.Context, inst *models.Instance) error
	GetInstance(ctx context.Context, userID string) (*models.Instance, error)
	ListInstances(ctx context.Context) ([]*models.Instance, error)
	UpdateInstance(ctx context.Context, inst *models.Instance) error
	DeleteInstance(ctx context.Context, userID string) error

	// 活跃度（idle reaper 的输入）
	TouchActivity(ctx context.Context, userID string) error
	SetWSConnections(ctx context.Context, userID string, n int) error
	GetActivity(ctx context.Context, userID string) (*Activity, error)

	// 代码评审记录
	CreateReview(ctx context.Context, r *models.Review) error
	GetReview(ctx context.Context, userID, reviewID string) (*models.Review, error)
	ListReviews(ctx context.Context, userID string) ([]*models.Review, error)
	UpdateReview(ctx context.Context, r *models.Review) error

	// VCS 授权 token（生产加密存储）
	SaveVCSToken(ctx context.Context, t *models.VCSToken) error
	GetVCSToken(ctx context.Context, userID, platform string) (*models.VCSToken, error)

	Close() error
}
