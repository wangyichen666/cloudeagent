// Package api 提供通用 HTTP 框架工具：JSON 响应、错误处理、中间件。
// 被各 apiserver 装配层复用，避免重复实现。
package api

import (
	"encoding/json"
	"net/http"
)

// JSON 写入 JSON 响应。
func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Error 写入标准错误响应：{"error": {"code": "...", "message": "..."}}。
func Error(w http.ResponseWriter, code int, errCode, message string) {
	JSON(w, code, map[string]any{"error": map[string]string{"code": errCode, "message": message}})
}
