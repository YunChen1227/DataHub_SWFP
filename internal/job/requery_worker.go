// Package job hosts the异步复查 worker that drives every PENDING ledger to a
// terminal BILLED/UNBILLED state (DESIGN §7.3 / §7.6).
package job

import (
	"context"
	"log/slog"
	"time"

	"github.com/datahub/relay/internal/domain/billing"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/port"
	"github.com/datahub/relay/internal/domain/quota"
)

// RequeryWorker periodically re-queries PENDING ledgers by reqid (DESIGN §7.3).
type RequeryWorker struct {
	ledger   port.LedgerRepository
	licenses port.LicenseRepository
	upstream port.UpstreamPort
	billing  *billing.Service
	quota    *quota.Service
	interval time.Duration
	batch    int
	log      *slog.Logger
}

func NewRequeryWorker(
	ledger port.LedgerRepository,
	licenses port.LicenseRepository,
	upstream port.UpstreamPort,
	bill *billing.Service,
	q *quota.Service,
	interval time.Duration,
	log *slog.Logger,
) *RequeryWorker {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &RequeryWorker{
		ledger: ledger, licenses: licenses, upstream: upstream,
		billing: bill, quota: q, interval: interval, batch: 100, log: log,
	}
}

// Run blocks until ctx is cancelled, scanning PENDING ledgers each tick.
func (w *RequeryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("requery worker stopped")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *RequeryWorker) tick(ctx context.Context) {
	pending, err := w.ledger.ListByState(ctx, model.StatePending, w.batch)
	if err != nil {
		w.log.Error("list pending failed", "err", err)
		return
	}
	for _, l := range pending {
		w.resolve(ctx, l)
	}
}

func (w *RequeryWorker) resolve(ctx context.Context, l *model.Ledger) {
	log := w.log.With("requestId", l.RequestID, "reqid", l.Reqid)

	rr, err := w.upstream.Requery(ctx, l.Reqid)
	if err != nil || rr == nil || !rr.Reachable {
		// still unreachable → leave for the reconciliation job.
		return
	}

	lic, err := w.licenses.FindByAppKey(ctx, l.AppKey)
	if err != nil || lic == nil {
		log.Error("cannot resolve license for pending ledger", "appKey", l.AppKey)
		return
	}

	decision := w.billing.FromRequery(rr)
	token := &quota.ReserveToken{LicenseID: lic.LicenseID, Route: l.Version, LedgerID: l.ID, Reqid: l.Reqid}
	// 复查只回答「上游那笔是否成交」，不含维度信息，故无 Sources、不定档计费标准：
	// 台账维持原有的 fee_standard/amount，只推进状态与计数。
	if err := w.quota.Settle(ctx, token, quota.SettleInput{Decision: decision, BusiCode: l.BusiCode}); err != nil {
		log.Error("settle from requery failed", "err", err)
		return
	}
	log.Info("pending resolved via requery", "resolved", decision.Resolved, "returned", decision.Returned)
}
