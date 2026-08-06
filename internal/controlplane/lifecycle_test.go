package controlplane

import (
	"context"
	"testing"
	"time"

	"cloude-agent/internal/models"
	"cloude-agent/internal/store"
)

func newTestManager(t *testing.T, bk *fakeBackend) (*Manager, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	cfgCache := store.NewMemoryCache(time.Hour)
	seats := NewMockSeatService()
	return NewManager(st, cfgCache, bk, seats), st
}

func TestLifecycleStateMachine(t *testing.T) {
	ctx := context.Background()
	bk := newFakeBackend()
	agent := fakeAgentServer(t)
	defer agent.Close()
	bk.setEndpoint("u-1001", agent.URL)

	mgr, st := newTestManager(t, bk)

	// None -> Running
	inst, err := mgr.Create(ctx, "u-1001")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inst.Status != models.StatusRunning {
		t.Fatalf("期望 running，实际 %s", inst.Status)
	}
	if !bk.hasCall("create:u-1001") {
		t.Fatal("后端未收到 create 调用")
	}

	// 幂等：再次 Create 不应重复创建
	if _, err := mgr.Create(ctx, "u-1001"); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	creates := 0
	for _, c := range bk.calls {
		if c == "create:u-1001" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("期望 create 只调用一次，实际 %d", creates)
	}

	// Running -> Suspended（工作区保留，缓存丢弃）
	inst, err = mgr.Suspend(ctx, "u-1001")
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if inst.Status != models.StatusSuspended {
		t.Fatalf("期望 suspended，实际 %s", inst.Status)
	}
	if !bk.hasCall("stop:u-1001") {
		t.Fatal("后端未收到 stop 调用")
	}
	if _, err := mgr.cache.Get(ctx, "u-1001"); err == nil {
		t.Fatal("休眠后模型配置缓存应被丢弃（凭证不落盘）")
	}

	// Suspended -> Running（唤醒并重新注入）
	inst, err = mgr.Wake(ctx, "u-1001")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if inst.Status != models.StatusRunning {
		t.Fatalf("期望 running，实际 %s", inst.Status)
	}
	if !bk.hasCall("start:u-1001") {
		t.Fatal("后端未收到 start 调用")
	}
	// 唤醒后应可从席位服务重新解析配置
	cfg, err := mgr.GetModelConfig(ctx, "u-1001")
	if err != nil || cfg.Model == "" {
		t.Fatalf("唤醒后配置应可解析: %v", err)
	}

	// 状态应已落库
	persisted, err := st.GetInstance(ctx, "u-1001")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if persisted.Status != models.StatusRunning {
		t.Fatalf("store 中状态应为 running，实际 %s", persisted.Status)
	}

	// Running/Suspended -> None
	if err := mgr.Delete(ctx, "u-1001"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetInstance(ctx, "u-1001"); err != store.ErrNotFound {
		t.Fatalf("删除后应不存在，实际 %v", err)
	}
}

func TestSuspendRequiresRunning(t *testing.T) {
	ctx := context.Background()
	bk := newFakeBackend()
	mgr, _ := newTestManager(t, bk)
	if _, err := mgr.Suspend(ctx, "u-404"); err == nil {
		t.Fatal("不存在的实例休眠应报错")
	}
}

func TestModelHotSwitch(t *testing.T) {
	ctx := context.Background()
	bk := newFakeBackend()
	agent := fakeAgentServer(t)
	defer agent.Close()
	bk.setEndpoint("u-2002", agent.URL)

	mgr, _ := newTestManager(t, bk)
	if _, err := mgr.Create(ctx, "u-2002"); err != nil {
		t.Fatalf("create: %v", err)
	}
	cfg := &models.ModelConfig{
		BaseURL:  "https://api.example.com",
		APIKey:   "sk-real-secret-1234",
		Model:    "gpt-4.1",
		Models:   []string{"gpt-4.1"},
		Provider: "example",
	}
	if _, err := mgr.SetModelConfig(ctx, "u-2002", cfg); err != nil {
		t.Fatalf("hot switch: %v", err)
	}
	got, err := mgr.GetModelConfig(ctx, "u-2002")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if got.Model != "gpt-4.1" {
		t.Fatalf("期望模型已切换为 gpt-4.1，实际 %s", got.Model)
	}
	if got.APIKey == "sk-real-secret-1234" || got.APIKey == "" {
		t.Fatalf("APIKey 应被掩码，实际 %q", got.APIKey)
	}
}
