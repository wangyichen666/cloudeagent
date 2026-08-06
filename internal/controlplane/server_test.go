package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloude-agent/internal/store"
)

func TestServerEndToEnd(t *testing.T) {
	bk := newFakeBackend()
	agent := fakeAgentServer(t)
	defer agent.Close()
	bk.setEndpoint("u-3001", agent.URL)

	st := store.NewMemory()
	manager := NewManager(st, store.NewMemoryCache(time.Hour), bk, NewMockSeatService())
	reaper := NewReaper(manager, 0, time.Minute)
	review := NewReviewWorker(st, manager.GetModelConfig)
	auth := NewAuthenticator("cloude-agent", "admin-token")
	srv := NewServer(Config{Listen: ":0", Manager: manager, Reaper: reaper, Review: review, Auth: auth})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	admin := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// 创建用户实例
	resp := admin(http.MethodPost, "/v1/users", `{"id":"u-3001"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 无 token 应 401
	resp = admin(http.MethodPost, "/v1/users", "")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/users", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未授权应 401，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 用派生 token 走实例级 chat（前端无需管理凭证）
	derived := auth.DerivedToken("u-3001")
	chatBody := `{"message":"你好"}`
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/users/u-3001/chat", bytes.NewReader([]byte(chatBody)))
	req.Header.Set("Authorization", "Bearer "+derived)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var chat struct {
		Reply string `json:"reply"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&chat); err != nil {
		t.Fatal(err)
	}
	if chat.Reply != "echo:你好" {
		t.Fatalf("reply: %s", chat.Reply)
	}

	// 休眠 -> 唤醒 -> 列表状态正确
	resp = admin(http.MethodPost, "/v1/users/u-3001/suspend", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend status: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = admin(http.MethodPost, "/v1/users/u-3001/wake", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wake status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = admin(http.MethodGet, "/v1/users", "")
	defer resp.Body.Close()
	var list struct {
		Instances []struct {
			Status string `json:"status"`
		} `json:"instances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Instances) != 1 || list.Instances[0].Status != "running" {
		t.Fatalf("列表状态: %+v", list.Instances)
	}

	// 删除
	resp = admin(http.MethodDelete, "/v1/users/u-3001", "")
	resp.Body.Close()
	resp = admin(http.MethodGet, "/v1/users", "")
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Instances) != 0 {
		t.Fatalf("删除后应无实例: %+v", list.Instances)
	}
}
