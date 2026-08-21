package memory

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.SubjectBillingRepository ---
//
// 主体年度计费的内存实现，语义与 postgres/subject_store.go 逐条对齐（含「免费命中也
// 累加 free_hits / last_hit_at」这种副作用），只是把 PG 的行级锁换成 Store 的单一
// 互斥锁——单进程下同样满足「并发恰好一次计费」。

// coverageRec 是一条免费期窗口（对应 billing_coverage 的一行）。
type coverageRec struct {
	firstChargedAt  time.Time
	chargedAt       time.Time
	expiresAt       time.Time
	chargeCount     int
	freeHits        int64
	lastHitAt       time.Time
	chargeReqid     string
	chargeRequestID string
}

func coverageKey(licenseID, route, creditCode string, cat model.Category) string {
	return licenseID + "|" + route + "|" + creditCode + "|" + string(cat)
}

func chargeKey(licenseID, route, reqid string) string {
	return licenseID + "|" + route + "|" + reqid
}

// ApplyCoverage 判定并推进免费期窗口。整个循环在一把锁内完成，等价于 PG 那条
// 原子 UPSERT 加事务：两个类目要么都推进、要么都不推进。
func (s *Store) ApplyCoverage(_ context.Context, req model.CoverageRequest) (*model.CoverageResult, error) {
	cats := model.CategoriesOf(req.Got)
	if len(cats) == 0 {
		return &model.CoverageResult{}, nil // 查无/失败：不动窗口
	}
	window := req.Window
	if window == "" {
		window = model.DefaultFreeWindow
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.coverage == nil {
		s.coverage = make(map[string]*coverageRec)
	}

	now := time.Now()
	out := &model.CoverageResult{Verdicts: make([]model.CategoryVerdict, 0, len(cats))}
	for _, cat := range cats {
		k := coverageKey(req.LicenseID, req.Route, req.CreditCode, cat)
		rec := s.coverage[k]
		v := model.CategoryVerdict{Category: cat}
		switch {
		case rec == nil: // 历史首次：开窗计费
			rec = &coverageRec{
				firstChargedAt: now, chargedAt: now,
				expiresAt:   addInterval(now, window),
				chargeCount: 1, lastHitAt: now,
				chargeReqid: req.Reqid, chargeRequestID: req.RequestID,
			}
			s.coverage[k] = rec
			v.Charged, v.FirstEver = true, true
		case !rec.expiresAt.After(now): // 窗口已过期：续期计费
			rec.chargedAt = now
			rec.expiresAt = addInterval(now, window)
			rec.chargeCount++
			rec.chargeReqid, rec.chargeRequestID = req.Reqid, req.RequestID
			rec.lastHitAt = now
			v.Charged = true
		default: // 窗口内：免费命中
			rec.freeHits++
			rec.lastHitAt = now
		}
		v.ExpiresAt, v.ChargeCount = rec.expiresAt, rec.chargeCount
		out.Verdicts = append(out.Verdicts, v)
	}
	return out, nil
}

// RecordCharge 追加计费事件，按 (licenseID, route, reqid) 幂等。
func (s *Store) RecordCharge(_ context.Context, c *model.SubjectCharge) error {
	if c == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chargeKeys == nil {
		s.chargeKeys = make(map[string]struct{})
	}
	k := chargeKey(c.LicenseID, c.Route, c.Reqid)
	if _, dup := s.chargeKeys[k]; dup {
		return nil
	}
	s.chargeKeys[k] = struct{}{}
	cp := *c
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	s.charges = append(s.charges, &cp)
	return nil
}

func (s *Store) SubjectCoverage(_ context.Context, licenseID, route, creditCode string) (*model.SubjectCoverage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := &model.SubjectCoverage{CreditCode: creditCode}
	for _, cat := range []model.Category{model.CatInvoice, model.CatTax} {
		rec := s.coverage[coverageKey(licenseID, route, creditCode, cat)]
		if rec == nil {
			continue
		}
		c := model.CategoryCoverage{
			Covered:     rec.expiresAt.After(now),
			ChargedAt:   rec.chargedAt,
			ExpiresAt:   rec.expiresAt,
			ChargeCount: rec.chargeCount,
			FreeHits:    rec.freeHits,
		}
		if cat == model.CatInvoice {
			out.Invoice = c
		} else {
			out.Tax = c
		}
	}
	return out, nil
}

func (s *Store) CountActiveCoverage(_ context.Context, licenseID, route string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	prefix := licenseID + "|" + route + "|"
	seen := make(map[string]struct{})
	for k, rec := range s.coverage {
		if !strings.HasPrefix(k, prefix) || !rec.expiresAt.After(now) {
			continue
		}
		// key = licenseID|route|creditCode|category，取第三段去重到企业。
		if parts := strings.Split(k, "|"); len(parts) == 4 {
			seen[parts[2]] = struct{}{}
		}
	}
	return int64(len(seen)), nil
}

// ListCharges 暴露计费流水供测试与开发态后台读取（追加顺序）。
func (s *Store) ListCharges(_ context.Context, licenseID string, limit int) ([]*model.SubjectCharge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.SubjectCharge, 0, len(s.charges))
	for _, c := range s.charges {
		if licenseID != "" && c.LicenseID != licenseID {
			continue
		}
		cp := *c
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// addInterval 按 Postgres interval 字面量推进时间，用 AddDate 实现**日历**语义
// （周年制：2026-12-28 + 1 year = 2027-12-28，闰年安全）。仅支持
// "N year|month|day" 这几种本项目会用到的写法，无法解析时退回 1 年。
func addInterval(t time.Time, spec string) time.Time {
	f := strings.Fields(strings.ToLower(strings.TrimSpace(spec)))
	n := 1
	unit := ""
	switch len(f) {
	case 1:
		unit = f[0]
	case 2:
		if v, err := strconv.Atoi(f[0]); err == nil {
			n = v
		}
		unit = f[1]
	}
	switch strings.TrimSuffix(unit, "s") {
	case "year":
		return t.AddDate(n, 0, 0)
	case "month":
		return t.AddDate(0, n, 0)
	case "day":
		return t.AddDate(0, 0, n)
	default:
		return t.AddDate(1, 0, 0)
	}
}
