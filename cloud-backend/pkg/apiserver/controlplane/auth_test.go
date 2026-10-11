package controlplane

import "testing"

func TestDerivedToken(t *testing.T) {
	a := NewAuthenticator("cloude-agent", "admin-secret")
	tok := a.DerivedToken("u-1001")
	if tok == "" || len(tok) != 64 {
		t.Fatalf("派生 token 应为 64 位 hex，实际 %q", tok)
	}
	if a.DerivedToken("u-1001") != tok {
		t.Fatal("同一用户派生 token 应稳定")
	}
	if a.DerivedToken("u-1002") == tok {
		t.Fatal("不同用户派生 token 不应相同")
	}
}

func TestAuthorization(t *testing.T) {
	a := NewAuthenticator("ns", "admin-secret")
	if !a.AuthorizeAdmin("Bearer admin-secret") {
		t.Fatal("管理 token 应通过")
	}
	if a.AuthorizeAdmin("Bearer wrong") {
		t.Fatal("错误 token 不应通过")
	}
	derived := a.DerivedToken("u-1")
	if !a.AuthorizeInstance("u-1", derived) {
		t.Fatal("派生 token 应通过实例鉴权")
	}
	if a.AuthorizeInstance("u-1", a.DerivedToken("u-2")) {
		t.Fatal("他人派生 token 不应通过")
	}
}
