package controlplane

import (
	"context"
	"log"
	"time"

	"cloude-agent/internal/models"
)

// Reaper 对应文档 4.3 的 Idle Reaper：
// 后台扫描活跃度表，空闲超阈值自动 Suspend，释放算力（本地即释放进程/容器资源）。
type Reaper struct {
	manager     *Manager
	idleTimeout time.Duration
	interval    time.Duration
}

func NewReaper(m *Manager, idleTimeout, interval time.Duration) *Reaper {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &Reaper{manager: m, idleTimeout: idleTimeout, interval: interval}
}

func (r *Reaper) Run(ctx context.Context) {
	if r.idleTimeout <= 0 {
		log.Println("[reaper] 已禁用（--idle-timeout=0）")
		return
	}
	log.Printf("[reaper] 启动：空闲 %s 自动休眠，扫描间隔 %s", r.idleTimeout, r.interval)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			instances, err := r.manager.List(ctx)
			if err != nil {
				log.Printf("[reaper] list: %v", err)
				continue
			}
			for _, inst := range instances {
				if inst.Status != models.StatusRunning {
					continue
				}
				act, _ := r.manager.store.GetActivity(ctx, inst.UserID)
				if act.WSConnections > 0 {
					continue
				}
				if time.Since(act.LastActiveAt) >= r.idleTimeout {
					log.Printf("[reaper] 空闲超时，自动休眠 user=%s last_active=%s", inst.UserID, act.LastActiveAt.Format(time.RFC3339))
					if _, err := r.manager.Suspend(ctx, inst.UserID); err != nil {
						log.Printf("[reaper] suspend %s: %v", inst.UserID, err)
					}
				}
			}
		}
	}
}
