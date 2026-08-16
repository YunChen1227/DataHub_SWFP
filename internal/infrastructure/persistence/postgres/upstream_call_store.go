package postgres

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.UpstreamCallRepository (设计_多源计费与上游对账 §5.1) ---
//
// 每个上游子源一行：一次 swfp 查询产生多行，各自带该源的上游订单号/请求号与成本。
// 关联键用业务键 (app_key, version, reqid)（与台账唯一键同构，join 天然对齐），
// 再冗余 request_id 供 join audit_log 与后台按 requestId 下钻。

const upstreamCallCols = `app_key, version, reqid, request_id, seq, source_name, source_label,
	source_alias, provider, dim_invoice, dim_tax, status, upstream_code, upstream_msg,
	upstream_uid, upstream_logid, cost_fen, billable, latency_ms, skip_reason`

// AppendUpstreamCalls 批量插入逐源明细。唯一键带 source_label，故重试/重放幂等
// (ON CONFLICT DO NOTHING)。
func (s *Store) AppendUpstreamCalls(ctx context.Context, calls []*model.UpstreamCallRecord) error {
	if len(calls) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`INSERT INTO upstream_call (` + upstreamCallCols + `) VALUES `)
	args := make([]any, 0, len(calls)*20)
	for i, c := range calls {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(")
		for j := 0; j < 20; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString("$" + strconv.Itoa(len(args)+j+1))
		}
		b.WriteString(")")
		args = append(args,
			c.AppKey, c.Version, c.Reqid, c.RequestID, c.Seq, c.Source, c.Label,
			c.Alias, c.Provider, c.Dims.Invoice, c.Dims.Tax, c.Status, c.Code, clipRunes(c.Msg, 256),
			c.UID, c.LogID, c.CostFen, c.Billable, c.LatencyMs, clipRunes(c.Reason, 128),
		)
	}
	b.WriteString(` ON CONFLICT (app_key, version, reqid, source_label) DO NOTHING`)
	_, err := s.pool.Exec(ctx, b.String(), args...)
	return err
}

func (s *Store) ListUpstreamCalls(ctx context.Context, f model.UpstreamCallFilter) ([]*model.UpstreamCallRecord, error) {
	q := `SELECT app_key, COALESCE(version,''), COALESCE(reqid,''), request_id, seq,
		source_name, source_label, source_alias, provider, dim_invoice, dim_tax, status,
		upstream_code, upstream_msg, upstream_uid, upstream_logid, cost_fen, billable,
		COALESCE(latency_ms,0), skip_reason, created_at
		FROM upstream_call WHERE 1=1`
	args := []any{}
	n := 0
	add := func(cond string, v any) {
		n++
		q += " AND " + cond + "$" + strconv.Itoa(n)
		args = append(args, v)
	}
	if f.Version != "" {
		add("version=", f.Version)
	}
	if f.RequestID != "" {
		add("request_id=", f.RequestID)
	}
	if f.Reqid != "" {
		add("reqid=", f.Reqid)
	}
	if f.AppKey != "" {
		add("app_key=", f.AppKey)
	}
	q += " ORDER BY seq, source_label"
	if f.Limit > 0 {
		n++
		q += " LIMIT $" + strconv.Itoa(n)
		args = append(args, f.Limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUpstreamCalls(rows)
}

func scanUpstreamCalls(rows pgx.Rows) ([]*model.UpstreamCallRecord, error) {
	var out []*model.UpstreamCallRecord
	for rows.Next() {
		var r model.UpstreamCallRecord
		if err := rows.Scan(&r.AppKey, &r.Version, &r.Reqid, &r.RequestID, &r.Seq,
			&r.Source, &r.Label, &r.Alias, &r.Provider, &r.Dims.Invoice, &r.Dims.Tax, &r.Status,
			&r.Code, &r.Msg, &r.UID, &r.LogID, &r.CostFen, &r.Billable,
			&r.LatencyMs, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// clipRunes 按字符数截断到列宽，避免上游返回的长错误串把整批 INSERT 打回。
// 按 rune 而非 byte 截断：错误串多为中文，PostgreSQL 的 VARCHAR(n) 也按字符计长。
func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
