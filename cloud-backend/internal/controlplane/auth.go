package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Authenticator 实现文档「七、安全隔离」中的身份校验：
//   - 管理面操作要求 Admin Bearer Token；
//   - 会话/实例操作可按 sha256(ns:userID) 派生实例 token 放行，
//     前端无需管理凭证即可连自己的 Agent。
type Authenticator struct {
	namespace  string
	adminToken string
}

func NewAuthenticator(namespace, adminToken string) *Authenticator {
	return &Authenticator{namespace: namespace, adminToken: adminToken}
}

// DerivedToken 按文档派生：sha256(ns + userID)。
func (a *Authenticator) DerivedToken(userID string) string {
	h := sha256.Sum256([]byte(a.namespace + ":" + userID))
	return hex.EncodeToString(h[:])
}

func (a *Authenticator) Namespace() string { return a.namespace }

func (a *Authenticator) isAdminToken(token string) bool {
	return a.adminToken != "" && token == a.adminToken
}

// BearerToken 从 Authorization 头提取 token。
func BearerToken(header string) string {
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

// AuthorizeAdmin 校验管理面请求。
func (a *Authenticator) AuthorizeAdmin(authHeader string) bool {
	return a.isAdminToken(BearerToken(authHeader))
}

// AuthorizeInstance 校验实例级请求：管理 token 或派生 token 均可。
func (a *Authenticator) AuthorizeInstance(userID, token string) bool {
	return a.isAdminToken(token) || token == a.DerivedToken(userID)
}
