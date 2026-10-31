package api

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/datahub/relay/internal/domain/admin"
	"github.com/datahub/relay/internal/domain/report"
)

// --- 出账/对账导出 ---

func (s *Server) adminReportVendors(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminFor(w, r); !ok {
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"vendors": report.Vendors})
}

func (s *Server) adminDownstreamReport(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	rep, err := svc.DownstreamReport(r.Context(), q.Get("customer"), q.Get("from"), q.Get("to"))
	writeReport(w, rep, err)
}

func (s *Server) adminUpstreamReport(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.adminFor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	rep, err := svc.UpstreamReport(r.Context(), q.Get("vendor"), q.Get("from"), q.Get("to"))
	writeReport(w, rep, err)
}

// writeReport 先把工作簿完整写进内存再发出：生成中途出错时仍能返回 JSON 错误，
// 而不是一个半截的文件。
func writeReport(w http.ResponseWriter, rep *admin.Report, err error) {
	switch {
	case errors.Is(err, admin.ErrValidation), errors.Is(err, report.ErrTooLarge):
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, admin.ErrReportsUnavailable):
		writeAdminError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var buf bytes.Buffer
	if err := rep.Workbook.Write(&buf); err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", "attachment; filename=\"report.xlsx\"; filename*=UTF-8''"+url.PathEscape(rep.Filename))
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
