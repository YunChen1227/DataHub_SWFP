package memory

import (
	"context"
	"sort"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.UpstreamCallRepository ---
//
// 逐源明细的内存实现。memory 后端是 e2e 的默认后端，这里必须与 pg 同步实现，否则
// 「每源一行 + 成本」这条新链路在 e2e 里根本跑不到。

// AppendUpstreamCalls 追加逐源明细；(appKey, version, reqid, label) 已存在则忽略
// （与 pg 的 ON CONFLICT DO NOTHING 一致，保证重放幂等）。
func (s *Store) AppendUpstreamCalls(_ context.Context, calls []*model.UpstreamCallRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range calls {
		if c == nil {
			continue
		}
		k := upstreamCallKey(c.AppKey, c.Version, c.Reqid, c.Label)
		if _, ok := s.upstreamCallKeys[k]; ok {
			continue
		}
		cp := *c
		if cp.CreatedAt.IsZero() {
			cp.CreatedAt = time.Now()
		}
		s.upstreamCallKeys[k] = struct{}{}
		s.upstreamCalls = append(s.upstreamCalls, &cp)
	}
	return nil
}

func (s *Store) ListUpstreamCalls(_ context.Context, f model.UpstreamCallFilter) ([]*model.UpstreamCallRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.UpstreamCallRecord, 0, len(s.upstreamCalls))
	for _, c := range s.upstreamCalls {
		if f.Version != "" && c.Version != f.Version {
			continue
		}
		if f.RequestID != "" && c.RequestID != f.RequestID {
			continue
		}
		if f.Reqid != "" && c.Reqid != f.Reqid {
			continue
		}
		if f.AppKey != "" && c.AppKey != f.AppKey {
			continue
		}
		cp := *c
		out = append(out, &cp)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}

func upstreamCallKey(appKey, version, reqid, label string) string {
	return appKey + "|" + version + "|" + reqid + "|" + label
}
