package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/datahub/relay/internal/domain/model"
)

// --- port.UpstreamAllowlistRepository ---

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	const q = `SELECT value FROM system_setting WHERE key=$1`
	var v string
	err := s.pool.QueryRow(ctx, q, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	const q = `INSERT INTO system_setting (key, value, updated_at)
		VALUES ($1,$2,now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`
	_, err := s.pool.Exec(ctx, q, key, value)
	return err
}

func (s *Store) ListAllowlist(ctx context.Context) ([]*model.UpstreamAllowEntry, error) {
	const q = `SELECT credit_code, provider, tax, invoice, call_time, source_file
		FROM upstream_allowlist ORDER BY credit_code, provider`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.UpstreamAllowEntry
	for rows.Next() {
		var e model.UpstreamAllowEntry
		var callTime *time.Time
		if err := rows.Scan(&e.CreditCode, &e.Provider, &e.Tax, &e.Invoice, &callTime, &e.SourceFile); err != nil {
			return nil, err
		}
		if callTime != nil {
			e.CallTime = *callTime
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *Store) ReplaceAllowlist(ctx context.Context, entries []*model.UpstreamAllowEntry) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `DELETE FROM upstream_allowlist`); err != nil {
		return err
	}
	const q = `INSERT INTO upstream_allowlist
		(credit_code, provider, tax, invoice, call_time, source_file, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,now())`
	for _, e := range entries {
		if e == nil {
			continue
		}
		var callTime any
		if !e.CallTime.IsZero() {
			callTime = e.CallTime
		}
		if _, err := tx.Exec(ctx, q, e.CreditCode, e.Provider, e.Tax, e.Invoice, callTime, e.SourceFile); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
