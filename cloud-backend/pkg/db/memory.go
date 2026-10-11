package db

import (
	"context"
	"sort"
	"sync"
	"time"

)

// Memory 是零依赖的内存存储，用于本地开发与演示。
// 注意：ModelConfig 从不经由此处持久化（文档 6.2 的凭证安全约束）。
type Memory struct {
	mu        sync.RWMutex
	instances map[string]*Instance
	activity  map[string]*Activity
	reviews   map[string]*Review
	vcs       map[string]*VCSToken
}

func NewMemory() *Memory {
	return &Memory{
		instances: make(map[string]*Instance),
		activity:  make(map[string]*Activity),
		reviews:   make(map[string]*Review),
		vcs:       make(map[string]*VCSToken),
	}
}

func (m *Memory) CreateInstance(ctx context.Context, inst *Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *inst
	m.instances[inst.UserID] = &cp
	return nil
}

func (m *Memory) GetInstance(ctx context.Context, userID string) (*Instance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[userID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *inst
	return &cp, nil
}

func (m *Memory) ListInstances(ctx context.Context) ([]*Instance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		cp := *inst
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

func (m *Memory) UpdateInstance(ctx context.Context, inst *Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.instances[inst.UserID]; !ok {
		return ErrNotFound
	}
	cp := *inst
	m.instances[inst.UserID] = &cp
	return nil
}

func (m *Memory) DeleteInstance(ctx context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instances, userID)
	delete(m.activity, userID)
	return nil
}

func (m *Memory) TouchActivity(ctx context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	act := m.activity[userID]
	if act == nil {
		act = &Activity{UserID: userID}
	}
	act.LastActiveAt = time.Now()
	m.activity[userID] = act
	return nil
}

func (m *Memory) SetWSConnections(ctx context.Context, userID string, n int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	act := m.activity[userID]
	if act == nil {
		act = &Activity{UserID: userID, LastActiveAt: time.Now()}
	}
	act.WSConnections = n
	act.LastActiveAt = time.Now()
	m.activity[userID] = act
	return nil
}

func (m *Memory) GetActivity(ctx context.Context, userID string) (*Activity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if act, ok := m.activity[userID]; ok {
		cp := *act
		return &cp, nil
	}
	// 允许无活动记录（等价于从未活跃）
	return &Activity{UserID: userID, LastActiveAt: time.Now()}, nil
}

func (m *Memory) CreateReview(ctx context.Context, r *Review) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *r
	m.reviews[r.ID] = &cp
	return nil
}

func (m *Memory) GetReview(ctx context.Context, userID, reviewID string) (*Review, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.reviews[reviewID]
	if !ok || r.UserID != userID {
		return nil, ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (m *Memory) ListReviews(ctx context.Context, userID string) ([]*Review, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Review, 0)
	for _, r := range m.reviews {
		if r.UserID == userID {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) UpdateReview(ctx context.Context, r *Review) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.reviews[r.ID]; !ok {
		return ErrNotFound
	}
	cp := *r
	m.reviews[r.ID] = &cp
	return nil
}

func (m *Memory) SaveVCSToken(ctx context.Context, t *VCSToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.vcs[t.UserID+"|"+t.Platform] = &cp
	return nil
}

func (m *Memory) GetVCSToken(ctx context.Context, userID, platform string) (*VCSToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if t, ok := m.vcs[userID+"|"+platform]; ok {
		cp := *t
		return &cp, nil
	}
	return nil, ErrNotFound
}

func (m *Memory) Close() error { return nil }
