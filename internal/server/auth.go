// auth.go — API Key 认证中间件。
package server

import (
	"net/http"
	"strings"
)

// extractKey 从 Authorization: Bearer <key> 中提取 key。
// 同时兼容 x-api-key 头（部分客户端习惯用它）。
func extractKey(r *http.Request) string {
	if key := r.Header.Get("X-Api-Key"); key != "" {
		return strings.TrimSpace(key)
	}
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
		return strings.TrimSpace(authz[7:])
	}
	return ""
}

// requireAPIKey 校验请求携带的 key 与配置一致。
// apiKey 为空表示不启用认证（仅限内网自用）。
func requireAPIKey(apiKey string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" {
			next(w, r)
			return
		}
		if extractKey(r) != apiKey {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key",
				"missing or invalid API key")
			return
		}
		next(w, r)
	}
}
