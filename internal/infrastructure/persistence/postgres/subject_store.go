package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.SubjectBillingRepository ---
//
// 主体年度计费的持久化实现 (设计_主体年度计费.md §5)。

// coverageUpsert 是整个主体计费的地基：单类目的「判定 + 开窗/续期/记免费命中」合并成
// 一条语句。并发安全性来自 Postgres 的行级锁——两个事务同时打同一个不存在的主体时，
// 先到者 INSERT，后到者在唯一索引上阻塞，解锁后走 DO UPDATE 分支并看到已生效的窗口，
// 于是判为免费。恰好一次计费，不需要应用层锁，也不需要分布式锁。
//
// 「本次是否计费」的判据用 charge_reqid = $6，而不是比较 charged_at 与 now()：
// reqid 每请求唯一，所以「这一行当前记录的计费请求就是我」当且仅当本次由我计费
// （无论是 INSERT 开窗还是到期续期）。这个判据不依赖 RETURNING 对新旧行值的解析
// 规则，也不依赖事务时间戳的稳定性，比时间比较更难出错。
//
// first_ever 不用 xmax 系统列判断，而由调用方按 charged && charge_count == 1 推出
// ——charge_count 插入时为 1、仅续期时 +1，语义等价且不依赖系统列。
//
// $5 是 Postgres 日历 interval（如 '1 year'）：周年制下 2026-12-28 计费应免到
// 2027-12-28，用固定小时数会在闰年差一天。
const coverageUpsert = `
INSERT INTO billing_coverage AS c
    (license_id, route, credit_code, category,
     first_charged_at, charged_at, expires_at,
     charge_count, free_hits, last_hit_at, charge_reqid, charge_request_id)
VALUES ($1, $2, $3, $4,
        now(), now(), now() + $5::interval,
        1, 0, now(), $6, $7)
ON CONFLICT (license_id, route, credit_code, category) DO UPDATE SET
    charged_at        = CASE WHEN c.expires_at <= now() THEN now()                ELSE c.charged_at        END,
    expires_at        = CASE WHEN c.expires_at <= now() THEN now() + $5::interval ELSE c.expires_at        END,
    charge_count      = c.charge_count + CASE WHEN c.expires_at <= now() THEN 1 ELSE 0 END,
    free_hits         = c.free_hits    + CASE WHEN c.expires_at <= now() THEN 0 ELSE 1 END,
    charge_reqid      = CASE WHEN c.expires_at <= now() THEN $6 ELSE c.charge_reqid      END,
    charge_request_id = CASE WHEN c.expires_at <= now() THEN $7 ELSE c.charge_request_id END,
    last_hit_at       = now(),
    updated_at        = now()
RETURNING (charge_reqid = $6), expires_at, charge_count`

// ApplyCoverage 逐类目判定并推进免费期窗口。两个类目放在同一个事务里：要么两个窗口
// 都推进，要么都不推进，不会出现「发票收了钱但税务的窗口没开」这种半成品状态。
// 事务内只有 1~2 条主键定位的语句，锁持有时间在微秒级。
func (s *Store) ApplyCoverage(ctx context.Context, req model.CoverageRequest) (*model.CoverageResult, error) {
	cats := model.CategoriesOf(req.Got)
	if len(cats) == 0 {
		return &model.CoverageResult{}, nil // 查无/失败：不动窗口，也不开事务
	}
	window := req.Window
	if window == "" {
		window = model.DefaultFreeWindow
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // 提交后为 no-op

	out := &model.CoverageResult{Verdicts: make([]model.CategoryVerdict, 0, len(cats))}
	for _, cat := range cats {
		v := model.CategoryVerdict{Category: cat}
		if err := tx.QueryRow(ctx, coverageUpsert,
			req.LicenseID, req.Route, req.CreditCode, string(cat), window, req.Reqid, req.RequestID,
		).Scan(&v.Charged, &v.ExpiresAt, &v.ChargeCount); err != nil {
			return nil, err
		}
		v.FirstEver = v.Charged && v.ChargeCount == 1
		out.Verdicts = append(out.Verdicts, v)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// RecordCharge 追加一条计费事件。按 (license_id, route, reqid) 幂等：重放/补记时
// ON CONFLICT DO NOTHING，绝不产生第二笔应收记录。
func (s *Store) RecordCharge(ctx context.Context, c *model.SubjectCharge) error {
	if c == nil {
		return nil
	}
	const q = `INSERT INTO billing_charge
		(license_id, app_key, route, credit_code, reqid, request_id, ledger_id,
		 charged_invoice, charged_tax, covered_invoice, covered_tax,
		 fee_standard, amount_fen, upstream_cost_fen, kind, window_to)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (license_id, route, reqid) DO NOTHING`
	var ledgerID any
	if c.LedgerID > 0 {
		ledgerID = c.LedgerID
	}
	var windowTo any
	if !c.WindowTo.IsZero() {
		windowTo = c.WindowTo
	}
	_, err := s.pool.Exec(ctx, q,
		c.LicenseID, c.AppKey, c.Route, c.CreditCode, c.Reqid, c.RequestID, ledgerID,
		c.Charged.Invoice, c.Charged.Tax, c.Covered.Invoice, c.Covered.Tax,
		string(c.FeeStandard), c.AmountFen, c.UpstreamCostFen, c.Kind, windowTo)
	return err
}

// SubjectCoverage 读某主体两个类目的免费期状态。缺行即从未计费过（Covered=false，
// 时间字段留零值）。
func (s *Store) SubjectCoverage(ctx context.Context, licenseID, route, creditCode string) (*model.SubjectCoverage, error) {
	const q = `SELECT category, charged_at, expires_at, charge_count, free_hits
		FROM billing_coverage
		WHERE license_id=$1 AND route=$2 AND credit_code=$3`
	rows, err := s.pool.Query(ctx, q, licenseID, route, creditCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &model.SubjectCoverage{CreditCode: creditCode}
	now := time.Now()
	for rows.Next() {
		var cat string
		var c model.CategoryCoverage
		if err := rows.Scan(&cat, &c.ChargedAt, &c.ExpiresAt, &c.ChargeCount, &c.FreeHits); err != nil {
			return nil, err
		}
		c.Covered = c.ExpiresAt.After(now)
		switch model.Category(cat) {
		case model.CatInvoice:
			out.Invoice = c
		case model.CatTax:
			out.Tax = c
		}
	}
	return out, rows.Err()
}

// CountActiveCoverage 统计该客户当前处于免费期的主体数（按企业去重，两个类目算一家）。
func (s *Store) CountActiveCoverage(ctx context.Context, licenseID, route string) (int64, error) {
	const q = `SELECT COUNT(DISTINCT credit_code) FROM billing_coverage
		WHERE license_id=$1 AND route=$2 AND expires_at > now()`
	var n int64
	err := s.pool.QueryRow(ctx, q, licenseID, route).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
