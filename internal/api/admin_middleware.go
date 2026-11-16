package api

import (
	"context"
	"net/http"
	"strings"
)

type adminCtxKey int

const adminUserKey adminCtxKey = 1

// requireAdmin wraps an admin handler with JWT bearer authentication (DESIGN §16.1).
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		token := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
		if token == "" || authz == token { // missing or no "Bearer " prefix
			writeAdminError(w, http.StatusUnauthorized, "缺少或非法的令牌")
			return
		}
		user, err := s.control.VerifyToken(token)
		if err != nil {
			writeAdminError(w, http.StatusUnauthorized, "令牌无效或已过期")
			return
		}
		ctx := context.WithValue(r.Context(), adminUserKey, user)
		next(w, r.WithContext(ctx))
	}
}

func adminOperator(r *http.Request) string {
	if v, ok := r.Context().Value(adminUserKey).(string); ok {
		return v
	}
	return ""
}
