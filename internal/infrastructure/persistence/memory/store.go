// Package memory is an in-process implementation of the persistence ports for
// local development and tests. Production MUST swap in Redis+Lua for the quota
// counters and a relational DB for the ledger/audit (DESIGN §7.5 / §11 / §16),
// using the migrations under /migrations.
//
// All mutations hold a single mutex, which makes them atomic and faithful to the
// "检查并预留" semantics — sufficient for a single-process dev server.
package memory

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

type quotaRow struct {
	serviceUsed int64 // 累计成功查得数（busiCode 10）
	totalCalls  int64 // 累计调用上游次数（CalledUpstream）
	// 计费口径（主体年度计费）：三档标准各自的计费笔数 + 累计应收。与上面两个
	// 调用口径的计数互不换算——免费期内的查询累加 serviceUsed 但不碰这些。
	billInvoice int64
	billTax     int64
	billBoth    int64
	amountFen   int64
}

// licenseRec is the store-internal aggregate for a普通用户 (DESIGN §7.1/§16.2).
type licenseRec struct {
	view            model.LicenseView
	name            string
	mobile          string
	secret          string // 客户 MD5 加签 secret（开发期明文; 生产加密, §11.4）
	secretCreatedAt time.Time
	validTo         time.Time
	createdAt       time.Time
}

// Store implements the persistence ports for license/quota/ledger plus the
// admin console ports (admin users / audit).
type Store struct {
	mu sync.Mutex

	licenses    map[string]*licenseRec // licenseID -> rec
	appKeyIndex map[string]string      // appKey -> licenseID
	quotas      map[string]*quotaRow   // licenseID|route -> quota (按路由独立计数)

	ledgerByReqid map[string]*model.Ledger // appKey|version|reqid
	ledgerByID    map[int64]*model.Ledger

	audits []*model.AuditRecord
	admins map[string]*model.AdminUser // username -> admin

	upstreamCalls    []*model.UpstreamCallRecord // 逐源明细（追加式）
	upstreamCallKeys map[string]struct{}         // appKey|version|reqid|label 去重

	// 主体年度计费（subject_store.go）：免费期窗口 + 计费事件流水。
	coverage   map[string]*coverageRec // licenseID|route|creditCode|category
	charges    []*model.SubjectCharge
	chargeKeys map[string]struct{} // licenseID|route|reqid 幂等去重

	seq      int64
	auditSeq int64
	adminSeq int64
}

// New returns an empty store.
func New() *Store {
	return &Store{
		licenses:      make(map[string]*licenseRec),
		appKeyIndex:   make(map[string]string),
		quotas:        make(map[string]*quotaRow),
		ledgerByReqid: make(map[string]*model.Ledger),
		ledgerByID:    make(map[int64]*model.Ledger),
		admins:        make(map[string]*model.AdminUser),

		upstreamCallKeys: make(map[string]struct{}),

		coverage:   make(map[string]*coverageRec),
		chargeKeys: make(map[string]struct{}),
	}
}

// SeedLicense registers a demo license with a bound secret (dev helper).
func (s *Store) SeedLicense(lic *model.LicenseView, secret, name, mobile string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.licenses[lic.LicenseID] = &licenseRec{
		view:            *lic,
		name:            name,
		mobile:          mobile,
		secret:          secret,
		secretCreatedAt: now,
		validTo:         now.AddDate(10, 0, 0),
		createdAt:       now,
	}
	s.appKeyIndex[lic.AppKey] = lic.LicenseID
	// 计数行 (licenseID|route) 由首次累加时按需创建。
}

// quotaKey scopes a counter row by (licenseID, route).
func quotaKey(licenseID, route string) string { return licenseID + "|" + route }

// quotaRowLocked returns (creating if needed) the counter row; caller holds s.mu.
func (s *Store) quotaRowLocked(licenseID, route string) *quotaRow {
	k := quotaKey(licenseID, route)
	q := s.quotas[k]
	if q == nil {
		q = &quotaRow{}
		s.quotas[k] = q
	}
	return q
}

// --- port.LicenseRepository ---

func (s *Store) FindByAppKey(_ context.Context, appKey string) (*model.LicenseView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	licenseID, ok := s.appKeyIndex[appKey]
	if !ok {
		return nil, nil
	}
	rec := s.licenses[licenseID]
	if rec == nil {
		return nil, nil
	}
	cp := rec.view
	return &cp, nil
}

// GetAppSecret backs the store-backed SecretProvider (DESIGN §16.2/§11.4).
func (s *Store) GetAppSecret(_ context.Context, licenseID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.licenses[licenseID]; rec != nil {
		return rec.secret, nil
	}
	return "", nil
}

// FindByAppKeyWithSecret 是 auth 鉴权的快路径（license 与 secret 同行一次取回，
// 见 auth.licenseWithSecretFinder）。查无返回 (nil, "", nil)。
func (s *Store) FindByAppKeyWithSecret(_ context.Context, appKey string) (*model.LicenseView, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	licenseID, ok := s.appKeyIndex[appKey]
	if !ok {
		return nil, "", nil
	}
	rec := s.licenses[licenseID]
	if rec == nil {
		return nil, "", nil
	}
	cp := rec.view
	return &cp, rec.secret, nil
}

// --- port.QuotaRepository ---

func (s *Store) ServiceUsed(_ context.Context, licenseID, route string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q := s.quotas[quotaKey(licenseID, route)]; q != nil {
		return q.serviceUsed, nil
	}
	return 0, nil
}

func (s *Store) IncServiceUsed(_ context.Context, licenseID, route string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotaRowLocked(licenseID, route).serviceUsed++
	return nil
}

func (s *Store) TotalCalls(_ context.Context, licenseID, route string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q := s.quotas[quotaKey(licenseID, route)]; q != nil {
		return q.totalCalls, nil
	}
	return 0, nil
}

func (s *Store) IncTotalCalls(_ context.Context, licenseID, route string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotaRowLocked(licenseID, route).totalCalls++
	return nil
}

func (s *Store) BillingCounters(_ context.Context, licenseID, route string) (model.BillingCounters, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.quotas[quotaKey(licenseID, route)]
	if q == nil {
		return model.BillingCounters{}, nil
	}
	return model.BillingCounters{
		ChargedInvoice: q.billInvoice,
		ChargedTax:     q.billTax,
		ChargedBoth:    q.billBoth,
		AmountFen:      q.amountFen,
	}, nil
}

func (s *Store) AddBilling(_ context.Context, licenseID, route string, standard model.FeeStandard, amountFen int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.quotaRowLocked(licenseID, route)
	switch standard {
	case model.FeeBoth:
		q.billBoth++
	case model.FeeInvoice:
		q.billInvoice++
	case model.FeeTax:
		q.billTax++
	default:
		return nil // none：不计费，不累加也不累计金额
	}
	q.amountFen += amountFen
	return nil
}

// --- port.LedgerRepository ---

func ledgerKey(appKey, version, reqid string) string { return appKey + "|" + version + "|" + reqid }

func (s *Store) FindByReqid(_ context.Context, appKey, route, reqid string) (*model.Ledger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.ledgerByReqid[ledgerKey(appKey, route, reqid)]
	if !ok {
		return nil, nil
	}
	cp := *l
	return &cp, nil
}

func (s *Store) Append(_ context.Context, l *model.Ledger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	l.ID = s.seq
	stored := *l
	s.ledgerByID[l.ID] = &stored
	s.ledgerByReqid[ledgerKey(l.AppKey, l.Version, l.Reqid)] = &stored
	return nil
}

func (s *Store) Settle(_ context.Context, id int64, st model.LedgerSettlement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ledgerByID[id]
	if l == nil {
		return nil
	}
	l.State = st.State
	l.CountedService = st.CountedService
	// 与 pg 的 Settle 一致：空值不覆盖。复查路径不带维度/成本信息，不能把首次
	// 结算写下的标准、金额、成本与源计数清零。
	if st.FeeStandard != "" {
		l.FeeStandard = st.FeeStandard
		l.AmountFen = st.AmountFen
	}
	if st.UpstreamCostFen > 0 {
		l.UpstreamCostFen = st.UpstreamCostFen
	}
	if st.SourceTotal > 0 {
		l.SourceTotal, l.SourceOK, l.SourceErr = st.SourceTotal, st.SourceOK, st.SourceErr
	}
	if st.BusiCode != 0 {
		l.BusiCode = st.BusiCode
	}
	if st.UpstreamCode != "" {
		l.UpstreamCode = st.UpstreamCode
	}
	if st.UpstreamUID != "" {
		l.UpstreamUID = st.UpstreamUID
	}
	if st.UpstreamLogID != "" {
		l.UpstreamLogID = st.UpstreamLogID
	}
	// 主体年度计费的四列，同样空值不覆盖。
	if st.CreditCode != "" {
		l.CreditCode = st.CreditCode
	}
	if st.ChargedScope != "" {
		l.ChargedScope = st.ChargedScope
	}
	if st.CoveredScope != "" {
		l.CoveredScope = st.CoveredScope
	}
	if st.ChargeState != "" {
		l.ChargeState = st.ChargeState
	}
	return nil
}

func (s *Store) ListByState(_ context.Context, state model.BillingState, limit int) ([]*model.Ledger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.Ledger, 0, limit)
	for _, l := range s.ledgerByID {
		if l.State == state {
			cp := *l
			out = append(out, &cp)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

var errAppKeyExists = errors.New("appKey 已存在")
