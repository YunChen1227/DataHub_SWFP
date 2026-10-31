package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.PayloadRepository (migrations/0010_payload.sql) ---

// AppendPayload 在一个事务里写请求行与各源行；两张表都按唯一键 ON CONFLICT DO
// NOTHING，spool 重放整条记录不会写重复。JSON/金额参数统一以文本传入再在 SQL 里
// 转型，避免依赖驱动对 jsonb/numeric 的类型推断。
func (s *Store) AppendPayload(ctx context.Context, p *model.RequestPayload) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO request_payload
			(request_id, app_key, version, reqid, credit_code, request_body, response_range,
			 inv_amt_12m, tax_amt_12m, inv_zero, tax_zero, rule_version, created_at)
			VALUES ($1,$2,$3,$4,$5,$6::text::jsonb,$7::text::jsonb,$8::text::numeric,$9::text::numeric,$10,$11,$12,$13)
			ON CONFLICT (request_id) DO NOTHING`,
			p.RequestID, p.AppKey, p.Version, p.Reqid, p.CreditCode,
			jsonText(p.RequestBody), jsonText(p.ResponseRange),
			p.InvAmt12m, p.TaxAmt12m, p.InvZero, p.TaxZero, p.RuleVersion, p.CreatedAt,
		); err != nil {
			return err
		}
		for _, u := range p.Sources {
			if _, err := tx.Exec(ctx, `INSERT INTO upstream_payload
				(request_id, app_key, version, reqid, source_label, source_name, provider, status, raw_data,
				 inv_amt_12m, tax_amt_12m, inv_zero, tax_zero, rule_version, created_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::text::jsonb,$10::text::numeric,$11::text::numeric,$12,$13,$14,$15)
				ON CONFLICT (request_id, source_label) DO NOTHING`,
				u.RequestID, u.AppKey, u.Version, u.Reqid, u.SourceLabel, u.SourceName, u.Provider, u.Status,
				jsonText(u.RawData), u.InvAmt12m, u.TaxAmt12m, u.InvZero, u.TaxZero, u.RuleVersion, u.CreatedAt,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// jsonText 把 JSON 片段转成可空文本参数；空即 NULL。
func jsonText(raw json.RawMessage) *string {
	if len(raw) == 0 {
		return nil
	}
	s := string(raw)
	return &s
}
