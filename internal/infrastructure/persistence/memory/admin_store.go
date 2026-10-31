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
			LicenseID:    d.LicenseID,
			AppKey:       d.AppKey,
			ClientUUID:   d.ClientUUID,
			Status:       d.Status,
			Rates:        d.Rates,
			AllowInvoice: d.AllowInvoice,
			AllowTax:     d.AllowTax,
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

func (s *Store) SetDimSwitch(_ context.Context, licenseID string, invoice, tax bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.licenses[licenseID]; rec != nil {
		rec.view.AllowInvoice = invoice
		rec.view.AllowTax = tax
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
		AllowInvoice:    rec.view.AllowInvoice,
		AllowTax:        rec.view.AllowTax,
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
		if !matchAudit(a, f) {
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

func (s *Store) CountAudits(_ context.Context, f model.AuditFilter) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, a := range s.audits {
		if matchAudit(a, f) {
			n++
		}
	}
	return n, nil
}

func matchAudit(a *model.AuditRecord, f model.AuditFilter) bool {
	switch {
	case f.Version != "" && a.Version != f.Version,
		f.AppKey != "" && a.AppKey != f.AppKey,
		len(f.AppKeys) > 0 && !containsStr(f.AppKeys, a.AppKey),
		f.BusiCode != nil && a.BusiCode != *f.BusiCode,
		f.Reqid != "" && a.Reqid != f.Reqid && a.RequestID != f.Reqid,
		f.CreditCode != "" && a.CreditCode != f.CreditCode:
		return false
	}
	return true
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// --- §16.4 usage stats ---

// cstZone 是北京时间固定时区 (+08:00)。用固定偏移而非 time.LoadLocation，避免
// Windows 上缺 tzdata 时加载失败；中国自 1991 年起无夏令时，固定 +08:00 与
// Postgres 侧 'Asia/Shanghai' 归档结果一致。
var cstZone = time.FixedZone("CST", 8*3600)

// UsageStats aggregates per (app_key, 时间桶) request/success counts (§16.4)。
func (s *Store) UsageStats(_ context.Context, f model.StatsFilter) ([]*model.UsageStat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type key struct{ appKey, bucket string }
	agg := map[key]*model.UsageStat{}
	for _, a := range s.audits {
		if f.Version != "" && a.Version != f.Version {
			continue
		}
		if len(f.AppKeys) > 0 && !containsStr(f.AppKeys, a.AppKey) {
			continue
		}
		if !f.From.IsZero() && a.CreatedAt.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && !a.CreatedAt.Before(f.To) {
			continue
		}
		bucket := bucketLabel(a.CreatedAt.In(cstZone), f.Granularity)
		k := key{a.AppKey, bucket}
		st := agg[k]
		if st == nil {
			st = &model.UsageStat{Bucket: bucket, AppKey: a.AppKey, Name: s.nameForAppKeyLocked(a.AppKey)}
			agg[k] = st
		}
		st.Total++
		if a.FoundData {
			st.Success++
		}
	}
	out := make([]*model.UsageStat, 0, len(agg))
	for _, v := range agg {
		out = append(out, v)
	}
	// bucket 降序 (新在前)，同桶按 appKey 升序，与 Postgres 侧一致。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bucket != out[j].Bucket {
			return out[i].Bucket > out[j].Bucket
		}
		return out[i].AppKey < out[j].AppKey
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// bucketLabel 按粒度格式化时间桶标签 (入参已是北京时间)。
func bucketLabel(t time.Time, g model.StatsGranularity) string {
	switch g {
	case model.GranularityYear:
		return t.Format("2006")
	case model.GranularityMonth:
		return t.Format("2006-01")
	default:
		return t.Format("2006-01-02")
	}
}

// nameForAppKeyLocked 反查 appKey 对应的用户名称；caller MUST hold s.mu。
func (s *Store) nameForAppKeyLocked(appKey string) string {
	if id, ok := s.appKeyIndex[appKey]; ok {
		if rec := s.licenses[id]; rec != nil {
			return rec.name
		}
	}
	return ""
}
