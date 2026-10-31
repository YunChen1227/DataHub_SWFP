package memory

import (
	"context"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.PayloadRepository ---

// AppendPayload 与 pg 同口径：请求行按 requestId、源行按 (requestId, label) 幂等。
func (s *Store) AppendPayload(_ context.Context, p *model.RequestPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.payloads == nil {
		s.payloads = make(map[string]*model.RequestPayload)
	}
	cur, ok := s.payloads[p.RequestID]
	if !ok {
		cp := *p
		cp.Sources = nil
		cur = &cp
		s.payloads[p.RequestID] = cur
	}
	have := make(map[string]bool, len(cur.Sources))
	for _, u := range cur.Sources {
		have[u.SourceLabel] = true
	}
	for _, u := range p.Sources {
		if u == nil || have[u.SourceLabel] {
			continue
		}
		cp := *u
		cur.Sources = append(cur.Sources, &cp)
		have[u.SourceLabel] = true
	}
	return nil
}

// Payload 返回某请求的留存数据（测试与报表用）；不存在返回 nil。
func (s *Store) Payload(requestID string) *model.RequestPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.payloads[requestID]
	if p == nil {
		return nil
	}
	cp := *p
	cp.Sources = append([]*model.UpstreamPayload(nil), p.Sources...)
	return &cp
}
