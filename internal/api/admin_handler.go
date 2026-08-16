package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/datahub/relay/internal/domain/admin"
	"github.com/datahub/relay/internal/domain/model"
)

// registerAdminRoutes mounts the admin console API + the SPA static files (§16).
// 登录与 JWT 校验走统一控制面 (s.control = x1)；用户/审计数据按版本作用域
// /admin/api/{ver}/... 路由到对应版本独立的后台服务 (各连独立数据库)。
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	if s.control == nil {
		return
	}
	mux.HandleFunc("POST /admin/api/login", s.adminLogin)

	mux.HandleFunc("GET /admin/api/{ver}/users", s.requireAdmin(s.adminListUsers))
	mux.HandleFunc("POST /admin/api/{ver}/users", s.requireAdmin(s.adminCreateUser))
	mux.HandleFunc("GET /admin/api/{ver}/users/{id}", s.requireAdmin(s.adminGetUser))
	mux.HandleFunc("PATCH /admin/api/{ver}/users/{id}", s.requireAdmin(s.adminUpdateUser))
	mux.HandleFunc("DELETE /admin/api/{ver}/users/{id}", s.requireAdmin(s.adminDeleteUser))
	mux.HandleFunc("POST /admin/api/{ver}/users/{id}/rotate-secret", s.requireAdmin(s.adminRotateSecret))

	mux.HandleFunc("GET /admin/api/{ver}/audits", s.requireAdmin(s.adminListAudits))
	mux.HandleFunc("GET /admin/api/{ver}/audits/{requestId}/calls", s.requireAdmin(s.adminListUpstreamCalls))

	if s.spaDir != "" {
		mux.HandleFunc("GET /admin/", s.serveSPA)
		mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin/", http.StatusFound)
		})
	}
}

// adminFor resolves the version-scoped admin service from the {ver} path value.
// Returns (nil,false) for an unknown version, in which case it has already
// written a 404 response.
func (s *Server) adminFor(w http.ResponseWriter, r *http.Request) (*admin.Service, bool) {
	ver := r.PathValue("ver")
	if !model.ValidVersion(ver) {
		writeAdminError(w, http.StatusNotFound, "未知版本: "+ver)
		return nil, false
	}
	st := s.stacks[ver]
	if st == nil || st.Admin == nil {
		writeAdminError(w, http.StatusNotFound, "版本未启用: "+ver)
		return nil, false
	}
	return st.Admin, true
}

// --- §16.1 login ---

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeAdminError(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	token, exp, err := s.control.Login(r.Context(), in.Username, in.Password)
	if err != nil {
		writeAdminError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"token": token, "expireAt": exp})
}

// --- §16.2 users ---

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	// ?q= 支持按 uuid(appKey)/名称/手机号检索 (DESIGN §16.2)。
	users, err := svc.SearchUsers(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) adminGetUser(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	u, err := svc.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, u)
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	var in struct {
		Name   string `json:"name"`
		Mobile string `json:"mobile"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeAdminError(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	res, err := svc.CreateUser(r.Context(), admin.CreateUserInput{
		Name:   in.Name,
		Mobile: in.Mobile,
	})
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, res)
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	var in struct {
		Status string `json:"status"`
		Mobile string `json:"mobile"`
		// rates 缺省(null) = 不改合同价；给出则整份覆盖（某档 0 = 该档走全局缺省费率）。
		Rates *model.FeeRates `json:"rates"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeAdminError(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	u, err := svc.UpdateUser(r.Context(), r.PathValue("id"), admin.UpdateUserInput{
		Status: in.Status,
		Mobile: in.Mobile,
		Rates:  in.Rates,
	})
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, u)
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	if err := svc.DeleteUser(r.Context(), r.PathValue("id")); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminRotateSecret(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	secret, err := svc.RotateSecret(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"secret": secret})
}

// --- §16.3 audits ---

func (s *Server) adminListAudits(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := model.AuditFilter{
		AppKey: q.Get("appKey"),
		Limit:  atoiDefault(q.Get("limit"), 100),
		Offset: atoiDefault(q.Get("offset"), 0),
	}
	if bc := q.Get("busiCode"); bc != "" {
		if n, err := strconv.Atoi(bc); err == nil {
			f.BusiCode = &n
		}
	}
	// ?q= 按 uuid(appKey)/名称/手机号过滤：先解析为匹配的 appKey 集合 (DESIGN §16.3)。
	if kw := strings.TrimSpace(q.Get("q")); kw != "" {
		matched, err := svc.SearchUsers(r.Context(), kw)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, err.Error())
			return
		}
		appKeys := make([]string, 0, len(matched))
		for _, u := range matched {
			appKeys = append(appKeys, u.AppKey)
		}
		if len(appKeys) == 0 {
			// 无匹配用户：直接返回空结果，避免退化为全量查询。
			writeAdminJSON(w, http.StatusOK, map[string]any{"audits": []*model.AuditRecord{}})
			return
		}
		f.AppKeys = appKeys
	}
	audits, err := svc.ListAudits(r.Context(), f)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"audits": audits})
}

// adminListUpstreamCalls 按 requestId 下钻某次请求的逐源调用明细：调了哪些源、各源
// 自己的上游订单号/请求号、耗时与成本，以及未调用的源与跳过原因——上游成本对账
// 与「为什么这单只按单发票收费」的一手证据。
func (s *Server) adminListUpstreamCalls(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	calls, err := svc.ListUpstreamCalls(r.Context(), r.PathValue("requestId"))
	if err != nil {
		if errors.Is(err, admin.ErrUpstreamCallsUnavailable) {
			writeAdminJSON(w, http.StatusOK, map[string]any{"calls": []*model.UpstreamCallRecord{}})
			return
		}
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"calls": calls})
}

// --- SPA static serving (§16.0) ---

func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/admin/")
	clean := filepath.Clean(rel)
	if clean == "." || clean == "/" || strings.HasPrefix(clean, "..") {
		clean = "index.html"
	}
	full := filepath.Join(s.spaDir, clean)
	if fi, err := os.Stat(full); err != nil || fi.IsDir() {
		// SPA fallback: serve index.html for client-side routes.
		full = filepath.Join(s.spaDir, "index.html")
	}
	http.ServeFile(w, r, full)
}

func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAdminError(w http.ResponseWriter, status int, msg string) {
	writeAdminJSON(w, status, map[string]any{"error": msg})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
