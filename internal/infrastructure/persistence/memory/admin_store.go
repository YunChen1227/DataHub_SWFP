package memory

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.AdminUserRepository (DESIGN §16.1) ---

func (s *Store) FindAdmin(_ context.Context, username string) (*model.AdminUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.admins[username]
	if !ok {
		return nil, nil
	}
	cp := *a
	return &cp, nil
}

func (s *Store) PutAdmin(_ context.Context, a *model.AdminUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminSeq++
	a.ID = s.adminSeq
	cp := *a
	s.admins[a.Username] = &cp
	return nil
}

// --- port.UserAdminRepository (DESIGN §16.2) ---

func (s *Store) ListUsers(_ context.Context, route string) ([]*model.UserDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.UserDetail, 0, len(s.licenses))
	for id := range s.licenses {
		out = append(out, s.userDetailLocked(id, route))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// SearchUsers matches appKey / name / mobile by case-insensitive substring.
func (s *Store) SearchUsers(_ context.Context, keyword, route string) ([]*model.UserDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kw := strings.ToLower(strings.TrimSpace(keyword))
	out := make([]*model.UserDetail, 0, len(s.licenses))
	for id, rec := range s.licenses {
		if kw != "" &&
			!strings.Contains(strings.ToLower(rec.view.AppKey), kw) &&
			!strings.Contains(strings.ToLower(rec.name), kw) &&
			!strings.Contains(strings.ToLower(rec.mobile), kw) {
			continue
		}
		out = append(out, s.userDetailLocked(id, route))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) GetUser(_ context.Context, licenseID, route string) (*model.UserDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.licenses[licenseID] == nil {
		return nil, nil
	}
	return s.userDetailLocked(licenseID, route), nil
}

func (s *Store) CreateUser(_ context.Context, d *model.UserDetail, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.appKeyIndex[d.AppKey]; ok {
		return errAppKeyExists
	}
	now := d.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	s.licenses[d.LicenseID] = &licenseRec{
		view: model.LicenseView{
			LicenseID:  d.LicenseID,
			AppKey:     d.AppKey,
			ClientUUID: d.ClientUUID,
			Status:     d.Status,
			Rates:      d.Rates,
		},
		name:            d.Name,
		mobile:          d.Mobile,
		secret:          secret,
		secretCreatedAt: now,
		validTo:         now.AddDate(10, 0, 0),
		createdAt:       now,
	}
	s.appKeyIndex[d.AppKey] = d.LicenseID
	// 计数行 (licenseID|route) 由首次累加时按需创建。
	return nil
}

func (s *Store) UpdateUser(_ context.Context, licenseID, status, mobile string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.licenses[licenseID]
	if rec == nil {
		return nil
	}
	rec.view.Status = status
	rec.mobile = mobile
	return nil
}

func (s *Store) SetRates(_ context.Context, licenseID string, rates model.FeeRates) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.licenses[licenseID]; rec != nil {
		rec.view.Rates = rates
	}
	return nil
}

func (s *Store) DeleteUser(_ context.Context, licenseID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.licenses[licenseID]; rec != nil {
		delete(s.appKeyIndex, rec.view.AppKey)
	}
	delete(s.licenses, licenseID)
	// 删除该 license 的所有路由计数行 (licenseID|route)。
	for k := range s.quotas {
		if strings.HasPrefix(k, licenseID+"|") {
			delete(s.quotas, k)
		}
	}
	return nil
}

func (s *Store) RotateSecret(_ context.Context, licenseID, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.licenses[licenseID]; rec != nil {
		rec.secret = secret
		rec.secretCreatedAt = time.Now()
	}
	return nil
}

// userDetailLocked builds a UserDetail with route-scoped 计数; caller MUST hold s.mu.
func (s *Store) userDetailLocked(licenseID, route string) *model.UserDetail {
	rec := s.licenses[licenseID]
	if rec == nil {
		return nil
	}
	q := s.quotas[quotaKey(licenseID, route)]
	if q == nil {
		q = &quotaRow{}
	}
	return &model.UserDetail{
		LicenseID:       licenseID,
		AppKey:          rec.view.AppKey,
		Name:            rec.name,
		Mobile:          rec.mobile,
		Status:          rec.view.Status,
		ClientUUID:      rec.view.ClientUUID,
		ServiceUsed:     q.serviceUsed,
		TotalCalls:      q.totalCalls,
		Rates:           rec.view.Rates,
		SecretCreatedAt: rec.secretCreatedAt,
		ValidTo:         rec.validTo,
		CreatedAt:       rec.createdAt,
	}
}

// --- port.AuditRepository (DESIGN §16.3) ---

func (s *Store) AppendAudit(_ context.Context, rec *model.AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditSeq++
	rec.ID = s.auditSeq
	cp := *rec
	s.audits = append(s.audits, &cp)
	return nil
}

func (s *Store) ListAudits(_ context.Context, f model.AuditFilter) ([]*model.AuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.AuditRecord, 0, f.Limit)
	skipped := 0
	// newest first.
	for i := len(s.audits) - 1; i >= 0; i-- {
		a := s.audits[i]
		if f.Version != "" && a.Version != f.Version {
			continue
		}
		if f.AppKey != "" && a.AppKey != f.AppKey {
			continue
		}
		if len(f.AppKeys) > 0 && !containsStr(f.AppKeys, a.AppKey) {
			continue
		}
		if f.BusiCode != nil && a.BusiCode != *f.BusiCode {
			continue
		}
		if skipped < f.Offset {
			skipped++
			continue
		}
		cp := *a
		out = append(out, &cp)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
