package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.AdminUserRepository (DESIGN §16.1) ---

func (s *Store) FindAdmin(ctx context.Context, username string) (*model.AdminUser, error) {
	const q = `SELECT id, username, password_hash, role, created_at FROM admin_user WHERE username=$1`
	var a model.AdminUser
	err := s.pool.QueryRow(ctx, q, username).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Role, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) PutAdmin(ctx context.Context, a *model.AdminUser) error {
	const q = `INSERT INTO admin_user (username, password_hash, role, created_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (username) DO UPDATE SET password_hash=EXCLUDED.password_hash, role=EXCLUDED.role
		RETURNING id`
	return s.pool.QueryRow(ctx, q, a.Username, a.PasswordHash, a.Role, a.CreatedAt).Scan(&a.ID)
}

// --- port.UserAdminRepository (DESIGN §16.2) ---

func (s *Store) ListUsers(ctx context.Context, route string) ([]*model.UserDetail, error) {
	const q = `SELECT license_id FROM license ORDER BY created_at DESC`
	return s.usersByQuery(ctx, route, q)
}

// SearchUsers matches appKey / name / mobile by case-insensitive substring.
func (s *Store) SearchUsers(ctx context.Context, keyword, route string) ([]*model.UserDetail, error) {
	if keyword == "" {
		return s.ListUsers(ctx, route)
	}
	const q = `SELECT license_id FROM license
	             WHERE app_key ILIKE $1 OR name ILIKE $1 OR mobile ILIKE $1
	             ORDER BY created_at DESC`
	return s.usersByQuery(ctx, route, q, "%"+keyword+"%")
}

// usersByQuery runs an id-selecting query then hydrates each UserDetail for route.
func (s *Store) usersByQuery(ctx context.Context, route, q string, args ...any) ([]*model.UserDetail, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*model.UserDetail, 0, len(ids))
	for _, id := range ids {
		d, err := s.GetUser(ctx, id, route)
		if err != nil {
			return nil, err
		}
		if d != nil {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *Store) GetUser(ctx context.Context, licenseID, route string) (*model.UserDetail, error) {
	const q = `SELECT license_id, app_key, COALESCE(name,''), COALESCE(mobile,''), status,
	             client_uuid, secret_created_at, valid_to, created_at,
	             COALESCE(rate_both_fen,0), COALESCE(rate_invoice_fen,0), COALESCE(rate_tax_fen,0)
	             FROM license WHERE license_id=$1`
	d := &model.UserDetail{}
	err := s.pool.QueryRow(ctx, q, licenseID).Scan(
		&d.LicenseID, &d.AppKey, &d.Name, &d.Mobile, &d.Status,
		&d.ClientUUID, &d.SecretCreatedAt, &d.ValidTo, &d.CreatedAt,
		&d.Rates.BothFen, &d.Rates.InvoiceFen, &d.Rates.TaxFen)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if d.ServiceUsed, err = s.ServiceUsedCount(ctx, licenseID, route); err != nil {
		return nil, err
	}
	if d.TotalCalls, err = s.TotalCallsCount(ctx, licenseID, route); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Store) CreateUser(ctx context.Context, d *model.UserDetail, secret string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// uniqueness on app_key surfaces as a constraint error.
	const insLicense = `INSERT INTO license
		(license_id, app_key, app_secret_enc, client_uuid, name, mobile, status,
		 valid_from, valid_to, secret_created_at,
		 rate_both_fen, rate_invoice_fen, rate_tax_fen)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now(), now() + interval '3650 days', now(), $8,$9,$10)`
	if _, err := tx.Exec(ctx, insLicense,
		d.LicenseID, d.AppKey, secret, d.ClientUUID, d.Name, d.Mobile, d.Status,
		d.Rates.BothFen, d.Rates.InvoiceFen, d.Rates.TaxFen); err != nil {
		return err
	}
	// 计数行 (license, route, dim) 由首次累加时 UPSERT 按需创建，无需在此预插。
	return tx.Commit(ctx)
}

func (s *Store) UpdateUser(ctx context.Context, licenseID, status, mobile string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE license SET status=$2, mobile=$3, updated_at=now() WHERE license_id=$1`,
		licenseID, status, mobile)
	return err
}

func (s *Store) SetRates(ctx context.Context, licenseID string, rates model.FeeRates) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE license SET rate_both_fen=$2, rate_invoice_fen=$3, rate_tax_fen=$4, updated_at=now()
		 WHERE license_id=$1`,
		licenseID, rates.BothFen, rates.InvoiceFen, rates.TaxFen)
	return err
}

func (s *Store) DeleteUser(ctx context.Context, licenseID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM quota WHERE license_id=$1`, licenseID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM license WHERE license_id=$1`, licenseID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RotateSecret(ctx context.Context, licenseID, secret string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE license SET app_secret_enc=$2, secret_created_at=now(), updated_at=now() WHERE license_id=$1`,
		licenseID, secret)
	return err
}

// --- port.AuditRepository (DESIGN §16.3) ---

func (s *Store) AppendAudit(ctx context.Context, rec *model.AuditRecord) error {
	const q = `INSERT INTO audit_log
		(request_id, version, app_key, trade_no, reqid, client_ip, called_upstream, found_data,
		 busi_code, busi_msg, upstream_code, upstream_uid, upstream_logid, billed,
		 latency_ms, name_mask, id_card_mask, mobile_mask, err_msg,
		 req_scope, data_scope, fee_standard, amount_fen, upstream_cost_fen,
		 credit_code, charged_scope, covered_scope, charge_state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,
		        $25,$26,$27,$28)
		RETURNING id, created_at`
	return s.pool.QueryRow(ctx, q,
		rec.RequestID, rec.Version, rec.AppKey, rec.TradeNo, rec.Reqid, rec.ClientIP, rec.CalledUpstream, rec.FoundData,
		rec.BusiCode, rec.BusiMsg, rec.UpstreamCode, rec.UpstreamUID, rec.UpstreamLogID, rec.Billed,
		rec.LatencyMs, rec.NameMask, rec.IDCardMask, rec.MobileMask, rec.ErrMsg,
		rec.ReqScope, rec.DataScope, rec.FeeStandard, rec.AmountFen, rec.UpstreamCostFen,
		rec.CreditCode, rec.ChargedScope, rec.CoveredScope, string(rec.ChargeState),
	).Scan(&rec.ID, &rec.CreatedAt)
}

func (s *Store) ListAudits(ctx context.Context, f model.AuditFilter) ([]*model.AuditRecord, error) {
	q := `SELECT id, request_id, COALESCE(version,''), app_key, COALESCE(trade_no,''), COALESCE(reqid,''),
		COALESCE(client_ip,''), called_upstream, found_data, COALESCE(busi_code,0),
		COALESCE(busi_msg,''), COALESCE(upstream_code,''), COALESCE(upstream_uid,''),
		COALESCE(upstream_logid,''), billed, COALESCE(latency_ms,0),
		COALESCE(name_mask,''), COALESCE(id_card_mask,''), COALESCE(mobile_mask,''),
		COALESCE(err_msg,''), created_at,
		COALESCE(req_scope,''), COALESCE(data_scope,''), COALESCE(fee_standard,''),
		COALESCE(amount_fen,0), COALESCE(upstream_cost_fen,0),
		COALESCE(credit_code,''), COALESCE(charged_scope,''), COALESCE(covered_scope,''),
		COALESCE(charge_state,'')
		FROM audit_log WHERE 1=1`
	args := []any{}
	n := 0
	if f.Version != "" {
		n++
		q += " AND version=$" + itoa(n)
		args = append(args, f.Version)
	}
	if f.AppKey != "" {
		n++
		q += " AND app_key=$" + itoa(n)
		args = append(args, f.AppKey)
	}
	if len(f.AppKeys) > 0 {
		n++
		q += " AND app_key = ANY($" + itoa(n) + ")"
		args = append(args, f.AppKeys)
	}
	if f.BusiCode != nil {
		n++
		q += " AND busi_code=$" + itoa(n)
		args = append(args, *f.BusiCode)
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		n++
		q += " LIMIT $" + itoa(n)
		args = append(args, f.Limit)
	}
	if f.Offset > 0 {
		n++
		q += " OFFSET $" + itoa(n)
		args = append(args, f.Offset)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.AuditRecord
	for rows.Next() {
		var r model.AuditRecord
		var chargeState string
		if err := rows.Scan(&r.ID, &r.RequestID, &r.Version, &r.AppKey, &r.TradeNo, &r.Reqid,
			&r.ClientIP, &r.CalledUpstream, &r.FoundData, &r.BusiCode,
			&r.BusiMsg, &r.UpstreamCode, &r.UpstreamUID, &r.UpstreamLogID, &r.Billed, &r.LatencyMs,
			&r.NameMask, &r.IDCardMask, &r.MobileMask, &r.ErrMsg, &r.CreatedAt,
			&r.ReqScope, &r.DataScope, &r.FeeStandard, &r.AmountFen, &r.UpstreamCostFen,
			&r.CreditCode, &r.ChargedScope, &r.CoveredScope, &chargeState); err != nil {
			return nil, err
		}
		r.ChargeState = model.ChargeState(chargeState)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// itoa is a tiny positive-int formatter for $N placeholders.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
