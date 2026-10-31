package memory

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.ReportRepository ---（与 pg 同口径：主表为审计/逐源明细，payload 可缺）

func (s *Store) DownstreamRows(_ context.Context, f model.ReportFilter) ([]*model.DownstreamRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.DownstreamRow
	for _, a := range s.audits {
		if !a.FoundData || a.Version != f.Version || a.CreatedAt.Before(f.From) || !a.CreatedAt.Before(f.To) {
			continue
		}
		if len(f.AppKeys) > 0 && !containsStr(f.AppKeys, a.AppKey) {
			continue
		}
		r := &model.DownstreamRow{
			CreatedAt: a.CreatedAt, RequestID: a.RequestID, Reqid: a.Reqid, AppKey: a.AppKey,
			CustomerName: s.nameForAppKeyLocked(a.AppKey), CreditCode: a.CreditCode,
			ReqScope: a.ReqScope, DataScope: a.DataScope, FeeStandard: a.FeeStandard,
			ChargeState: string(a.ChargeState), Providers: s.okProvidersLocked(a.RequestID),
		}
		if p := s.payloads[a.RequestID]; p != nil {
			r.HasPayload = true
			r.ZeroVerdict = p.ZeroVerdict
			r.CompanyName = companyName(p.ResponseRange)
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) UpstreamCalls(_ context.Context, f model.ReportFilter) ([]*model.UpstreamCallRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	codes := map[string]string{}
	for _, a := range s.audits {
		codes[a.RequestID] = a.CreditCode
	}
	var out []*model.UpstreamCallRow
	for _, c := range s.upstreamCalls {
		if c.Status == model.CallSkipped || c.Version != f.Version || c.Provider != f.Provider ||
			c.CreatedAt.Before(f.From) || !c.CreatedAt.Before(f.To) {
			continue
		}
		r := &model.UpstreamCallRow{
			CreatedAt: c.CreatedAt, RequestID: c.RequestID, Reqid: c.Reqid, AppKey: c.AppKey,
			CustomerName: s.nameForAppKeyLocked(c.AppKey), CreditCode: codes[c.RequestID],
			Label: c.Label, SourceName: c.Source, Provider: c.Provider, Status: c.Status,
			Code: c.Code, Msg: c.Msg, UID: c.UID, LogID: c.LogID,
			DimInvoice: c.Dims.Invoice, DimTax: c.Dims.Tax,
		}
		if p := s.payloads[c.RequestID]; p != nil {
			for _, u := range p.Sources {
				if u.SourceLabel == c.Label {
					r.HasPayload = true
					r.ZeroVerdict = u.ZeroVerdict
				}
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) okProvidersLocked(requestID string) string {
	seen := map[string]bool{}
	var ps []string
	for _, c := range s.upstreamCalls {
		if c.RequestID == requestID && c.Status == model.CallOK && !seen[c.Provider] {
			seen[c.Provider] = true
			ps = append(ps, c.Provider)
		}
	}
	sort.Strings(ps)
	return strings.Join(ps, ",")
}

// companyName 取返回给下游的 nsrjbxx.nsrmc（发票段优先）。
func companyName(resp json.RawMessage) string {
	var out map[string]struct {
		Nsrjbxx struct {
			Nsrmc string `json:"nsrmc"`
		} `json:"nsrjbxx"`
	}
	if json.Unmarshal(resp, &out) != nil {
		return ""
	}
	for _, seg := range []string{"发票数据聚合", "税务数据聚合"} {
		if n := out[seg].Nsrjbxx.Nsrmc; n != "" {
			return n
		}
	}
	return ""
}
