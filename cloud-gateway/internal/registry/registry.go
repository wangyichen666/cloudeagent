// Package registry 维护 userID -> agent endpoint 的路由表。
// backend 在创建/唤醒实例后注册 endpoint，删除实例后注销；
// gateway 转发请求时在此解析目标地址，业务层不感知 Pod DNS 等细节。
package registry

import (
	"sync"
)

// Registry 是线程安全的内存路由表。
type Registry struct {
	mu        sync.RWMutex
	endpoints map[string]string
}

func New() *Registry {
	return &Registry{endpoints: make(map[string]string)}
}

// Register 注册（或更新）一个 userID 的 agent endpoint。
func (r *Registry) Register(userID, endpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoints[userID] = endpoint
}

// Unregister 删除一个 userID 的路由（实例删除/销毁时调用）。
func (r *Registry) Unregister(userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.endpoints, userID)
}

// Resolve 解析 userID 对应的 agent endpoint。
func (r *Registry) Resolve(userID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ep, ok := r.endpoints[userID]
	return ep, ok
}

// Snapshot 返回当前全部路由（运维/观测用）。
func (r *Registry) Snapshot() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.endpoints))
	for k, v := range r.endpoints {
		out[k] = v
	}
	return out
}
