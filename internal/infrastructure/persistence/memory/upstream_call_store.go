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

// SourceStats 按源汇总某用户某区间内的结果分布（与 pg 侧同口径：按 source_label
// 分组，skipped 也计入 calls）。
func (s *Store) SourceStats(_ context.Context, f model.SourceStatFilter) ([]*model.SourceStat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	agg := map[string]*model.SourceStat{}
	for _, c := range s.upstreamCalls {
		if f.Version != "" && c.Version != f.Version {
			continue
		}
		if f.AppKey != "" && c.AppKey != f.AppKey {
			continue
		}
		if !f.From.IsZero() && c.CreatedAt.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && !c.CreatedAt.Before(f.To) {
			continue
		}
		st := agg[c.Label]
		if st == nil {
			st = &model.SourceStat{Source: c.Source, Label: c.Label, Provider: c.Provider}
			agg[c.Label] = st
		}
		st.Calls++
		switch c.Status {
		case model.CallOK:
			st.OK++
		case model.CallEmpty:
			st.Empty++
		case model.CallError:
			st.Err++
		case model.CallSkipped:
			st.Skipped++
		}
	}
	out := make([]*model.SourceStat, 0, len(agg))
	for _, v := range agg {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

func upstreamCallKey(appKey, version, reqid, label string) string {
	return appKey + "|" + version + "|" + reqid + "|" + label
}
