package memory

import (
	"context"
	"sort"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.UpstreamAllowlistRepository ---

func (s *Store) GetSetting(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings == nil {
		return "", nil
	}
	return s.settings[key], nil
}

func (s *Store) SetSetting(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings == nil {
		s.settings = map[string]string{}
	}
	s.settings[key] = value
	return nil
}

func (s *Store) ListAllowlist(_ context.Context) ([]*model.UpstreamAllowEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.UpstreamAllowEntry, 0, len(s.allowlist))
	for _, e := range s.allowlist {
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreditCode != out[j].CreditCode {
			return out[i].CreditCode < out[j].CreditCode
		}
		return out[i].Provider < out[j].Provider
	})
	return out, nil
}

func (s *Store) ReplaceAllowlist(_ context.Context, entries []*model.UpstreamAllowEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowlist = make(map[string]*model.UpstreamAllowEntry, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		cp := *e
		if cp.CallTime.IsZero() {
			cp.CallTime = time.Time{}
		}
		key := cp.CreditCode + "|" + cp.Provider
		if old, ok := s.allowlist[key]; ok {
			cp.Tax = cp.Tax || old.Tax
			cp.Invoice = cp.Invoice || old.Invoice
			if old.CallTime.After(cp.CallTime) {
				cp.CallTime = old.CallTime
			}
		}
		s.allowlist[key] = &cp
	}
	return nil
}
