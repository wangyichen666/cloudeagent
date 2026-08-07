package store

import (
	"context"
	"testing"
	"time"

	"cloude-agent/internal/models"
)

func TestMemoryInstanceCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	inst := &models.Instance{
		UserID:       "u-1",
		InstanceName: "agent-u-1",
		Status:       models.StatusRunning,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if err := s.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInstance(ctx, "u-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusRunning {
		t.Fatalf("status: %s", got.Status)
	}
	if _, err := s.GetInstance(ctx, "u-404"); err != ErrNotFound {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
	inst.Status = models.StatusSuspended
	if err := s.UpdateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteInstance(ctx, "u-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInstance(ctx, "u-1"); err != ErrNotFound {
		t.Fatalf("删除后应 ErrNotFound，实际 %v", err)
	}
}

func TestMemoryActivity(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	if err := s.TouchActivity(ctx, "u-1"); err != nil {
		t.Fatal(err)
	}
	act, err := s.GetActivity(ctx, "u-1")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(act.LastActiveAt) > time.Minute {
		t.Fatal("last_active_at 应接近当前时间")
	}
	if err := s.SetWSConnections(ctx, "u-1", 2); err != nil {
		t.Fatal(err)
	}
	act, _ = s.GetActivity(ctx, "u-1")
	if act.WSConnections != 2 {
		t.Fatalf("ws_connections: %d", act.WSConnections)
	}
}
