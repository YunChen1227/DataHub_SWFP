package postgres

import (
	"context"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.ReportRepository ---
//
// 以 audit_log / upstream_call 为主表 LEFT JOIN 原始数据表：上线前的历史请求没有
// payload 行，照样出现在报表里（HasPayload=false），不会漏。

func (s *Store) DownstreamRows(ctx context.Context, f model.ReportFilter) ([]*model.DownstreamRow, error) {
	q := `SELECT a.created_at, a.request_id, COALESCE(a.reqid,''), a.app_key, COALESCE(l.name,''),
		COALESCE(a.credit_code,''),
		COALESCE(NULLIF(p.response_range #>> '{发票数据聚合,nsrjbxx,nsrmc}',''),
		         NULLIF(p.response_range #>> '{税务数据聚合,nsrjbxx,nsrmc}',''), ''),
		COALESCE(a.req_scope,''), COALESCE(a.data_scope,''), COALESCE(a.fee_standard,''),
		COALESCE(a.charge_state,''),
		COALESCE((SELECT string_agg(DISTINCT c.provider, ',') FROM upstream_call c
		          WHERE c.request_id = a.request_id AND c.status = 'ok'), ''),
		p.request_id IS NOT NULL, p.inv_amt_12m::text, p.tax_amt_12m::text, p.inv_zero, p.tax_zero,
		COALESCE(p.rule_version,'')
		FROM audit_log a
		LEFT JOIN license l ON l.app_key = a.app_key
		LEFT JOIN request_payload p ON p.request_id = a.request_id
		WHERE a.found_data AND a.version = $1 AND a.created_at >= $2 AND a.created_at < $3`
	args := []any{f.Version, f.From, f.To}
	if len(f.AppKeys) > 0 {
		q += " AND a.app_key = ANY($4)"
		args = append(args, f.AppKeys)
	}
	q += " ORDER BY a.created_at, a.id"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.DownstreamRow
	for rows.Next() {
		var r model.DownstreamRow
		if err := rows.Scan(&r.CreatedAt, &r.RequestID, &r.Reqid, &r.AppKey, &r.CustomerName,
			&r.CreditCode, &r.CompanyName, &r.ReqScope, &r.DataScope, &r.FeeStandard, &r.ChargeState,
			&r.Providers, &r.HasPayload, &r.InvAmt12m, &r.TaxAmt12m, &r.InvZero, &r.TaxZero,
			&r.RuleVersion); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *Store) UpstreamCalls(ctx context.Context, f model.ReportFilter) ([]*model.UpstreamCallRow, error) {
	q := `SELECT c.created_at, c.request_id, COALESCE(c.reqid,''), c.app_key, COALESCE(l.name,''),
		COALESCE((SELECT a.credit_code FROM audit_log a WHERE a.request_id = c.request_id LIMIT 1),''),
		c.source_label, c.source_name, c.provider, c.status, c.upstream_code, c.upstream_msg,
		c.upstream_uid, c.upstream_logid, c.dim_invoice, c.dim_tax,
		u.request_id IS NOT NULL, u.inv_amt_12m::text, u.tax_amt_12m::text, u.inv_zero, u.tax_zero,
		COALESCE(u.rule_version,'')
		FROM upstream_call c
		LEFT JOIN license l ON l.app_key = c.app_key
		LEFT JOIN upstream_payload u ON u.request_id = c.request_id AND u.source_label = c.source_label
		WHERE c.status <> 'skipped' AND c.version = $1 AND c.provider = $2
		  AND c.created_at >= $3 AND c.created_at < $4
		ORDER BY c.created_at, c.request_id, c.seq, c.source_label`
	rows, err := s.pool.Query(ctx, q, f.Version, f.Provider, f.From, f.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.UpstreamCallRow
	for rows.Next() {
		var r model.UpstreamCallRow
		if err := rows.Scan(&r.CreatedAt, &r.RequestID, &r.Reqid, &r.AppKey, &r.CustomerName, &r.CreditCode,
			&r.Label, &r.SourceName, &r.Provider, &r.Status, &r.Code, &r.Msg, &r.UID, &r.LogID,
			&r.DimInvoice, &r.DimTax, &r.HasPayload, &r.InvAmt12m, &r.TaxAmt12m, &r.InvZero, &r.TaxZero,
			&r.RuleVersion); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
