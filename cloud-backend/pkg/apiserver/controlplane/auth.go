// Package controlplane 是控制面的 HTTP/WS 接口层。
// 只负责协议适配（路由、鉴权、序列化、WebSocket 转发），
// 业务编排全部委托给 sandboxruntime 应用层。
package controlplane

import (
	"crypto/sha256"
	"encoding/hex"

	"cloude-agent/pkg/api"
)

// Authenticator 实现身份校验：
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

// DerivedToken 按 sha256(ns + userID) 派生实例 token。
func (a *Authenticator) DerivedToken(userID string) string {
	h := sha256.Sum256([]byte(a.namespace + ":" + userID))
	return hex.EncodeToString(h[:])
}

func (a *Authenticator) Namespace() string { return a.namespace }

func (a *Authenticator) isAdminToken(token string) bool {
	return a.adminToken != "" && token == a.adminToken
}

// AuthorizeAdmin 校验管理面请求。
func (a *Authenticator) AuthorizeAdmin(authHeader string) bool {
	return a.isAdminToken(api.BearerToken(authHeader))
}

// AuthorizeInstance 校验实例级请求：管理 token 或派生 token 均可。
func (a *Authenticator) AuthorizeInstance(userID, token string) bool {
	return a.isAdminToken(token) || token == a.DerivedToken(userID)
}
