// Package redis is the 成功查得数 counter adapter (DESIGN §7.5). v0.6 起取消额度
// 限制与维度②上游计数：Redis 仅保存 svc_used 计数器，write-through 到 durable
// PostgreSQL 镜像，Redis flush/restart 后按 key miss 重新 seed。
package redis

import (
	"context"
	"fmt"
	"sync"

	goredis "github.com/redis/go-redis/v9"

	"github.com/datahub/relay/internal/domain/model"
)

// Durable is the PostgreSQL mirror the quota repo reads per-route 计数 from and
// write-throughs mutations to (implemented by persistence/postgres.Store)。
type Durable interface {
	ServiceUsedCount(ctx context.Context, licenseID, route string) (svcUsed int64, err error)
	AddServiceUsed(ctx context.Context, licenseID, route string, delta int64) error
	TotalCallsCount(ctx context.Context, licenseID, route string) (calls int64, err error)
	AddTotalCalls(ctx context.Context, licenseID, route string, delta int64) error
	// 计费口径（主体年度计费）：三档笔数 + 累计应收。
	BillingCountersOf(ctx context.Context, licenseID, route string) (model.BillingCounters, error)
	AddBillingCount(ctx context.Context, licenseID, route string, standard model.FeeStandard, amountFen int64) error
}

// Options configures the Redis connection.
type Options struct {
	Addr     string
	Username string
	Password string
	DB       int
	PoolSize int
}

// Quota implements port.QuotaRepository on Redis + a durable PG mirror.
type Quota struct {
	rdb    *goredis.Client
	pg     Durable
	seeded sync.Map // licenseID -> struct{} (process-local seed guard)
}

// New dials Redis and verifies connectivity.
func New(ctx context.Context, opts Options, pg Durable) (*Quota, error) {
	rdb := goredis.NewClient(&goredis.Options{
		Addr:     opts.Addr,
		Username: opts.Username,
		Password: opts.Password,
		DB:       opts.DB,
		PoolSize: opts.PoolSize,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Quota{rdb: rdb, pg: pg}, nil
}

// Close releases the Redis client.
func (q *Quota) Close() { _ = q.rdb.Close() }

// 计数 key 按 (license, route) 独立：共享 license 的 v8/v9 互不干扰。
func kSvcUsed(lid, route string) string   { return "quota:" + lid + ":" + route + ":svc_used" }
func kCallTotal(lid, route string) string { return "quota:" + lid + ":" + route + ":call_total" }

// 计费口径的四个 key（主体年度计费）。与上面两个调用口径的计数器同管线、不同 key：
// 免费期内的查询累加 svc_used/call_total 却不碰这四个。
func kBillInvoice(lid, route string) string { return "quota:" + lid + ":" + route + ":bill_invoice" }
func kBillTax(lid, route string) string     { return "quota:" + lid + ":" + route + ":bill_tax" }
func kBillBoth(lid, route string) string    { return "quota:" + lid + ":" + route + ":bill_both" }
func kAmountFen(lid, route string) string   { return "quota:" + lid + ":" + route + ":amount_fen" }

// kBillStandard 返回某收费标准对应的笔数 key；none 无对应 key（不计费）。
func kBillStandard(lid, route string, standard model.FeeStandard) (string, bool) {
	switch standard {
	case model.FeeBoth:
		return kBillBoth(lid, route), true
	case model.FeeInvoice:
		return kBillInvoice(lid, route), true
	case model.FeeTax:
		return kBillTax(lid, route), true
	default:
		return "", false
	}
}

func seedKey(lid, route string) string { return lid + "|" + route }

// ensure lazily seeds 全部 Redis 计数器 (成功查得数 + 调用次数 + 四个计费口径) from the
// durable PG mirror (SETNX so a flushed Redis is rehydrated and concurrent processes
// don't clobber)。
func (q *Quota) ensure(ctx context.Context, licenseID, route string) error {
	if _, ok := q.seeded.Load(seedKey(licenseID, route)); ok {
		return nil
	}
	svcUsed, err := q.pg.ServiceUsedCount(ctx, licenseID, route)
	if err != nil {
		return err
	}
	if err := q.rdb.SetNX(ctx, kSvcUsed(licenseID, route), svcUsed, 0).Err(); err != nil {
		return err
	}
	calls, err := q.pg.TotalCallsCount(ctx, licenseID, route)
	if err != nil {
		return err
	}
	if err := q.rdb.SetNX(ctx, kCallTotal(licenseID, route), calls, 0).Err(); err != nil {
		return err
	}
	bill, err := q.pg.BillingCountersOf(ctx, licenseID, route)
	if err != nil {
		return err
	}
	for key, val := range map[string]int64{
		kBillInvoice(licenseID, route): bill.ChargedInvoice,
		kBillTax(licenseID, route):     bill.ChargedTax,
		kBillBoth(licenseID, route):    bill.ChargedBoth,
		kAmountFen(licenseID, route):   bill.AmountFen,
	} {
		if err := q.rdb.SetNX(ctx, key, val, 0).Err(); err != nil {
			return err
		}
	}
	q.seeded.Store(seedKey(licenseID, route), struct{}{})
	return nil
}

func (q *Quota) getCounter(ctx context.Context, key string) (int64, error) {
	v, err := q.rdb.Get(ctx, key).Int64()
	if err == goredis.Nil {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	return v, nil
}

// ServiceUsed returns the cumulative 成功查得数 for (license, route) (Redis, PG-mirrored).
func (q *Quota) ServiceUsed(ctx context.Context, licenseID, route string) (int64, error) {
	if err := q.ensure(ctx, licenseID, route); err != nil {
		return 0, err
	}
	return q.getCounter(ctx, kSvcUsed(licenseID, route))
}

// IncServiceUsed increments 成功查得数 by 1 for (license, route) (Redis) and mirrors to PG.
func (q *Quota) IncServiceUsed(ctx context.Context, licenseID, route string) error {
	if err := q.ensure(ctx, licenseID, route); err != nil {
		return err
	}
	if err := q.rdb.Incr(ctx, kSvcUsed(licenseID, route)).Err(); err != nil {
		return err
	}
	return q.pg.AddServiceUsed(ctx, licenseID, route, 1)
}

// TotalCalls returns the cumulative 调用次数 for (license, route) (Redis, PG-mirrored).
func (q *Quota) TotalCalls(ctx context.Context, licenseID, route string) (int64, error) {
	if err := q.ensure(ctx, licenseID, route); err != nil {
		return 0, err
	}
	return q.getCounter(ctx, kCallTotal(licenseID, route))
}

// IncTotalCalls increments 调用次数 by 1 for (license, route) (Redis) and mirrors to PG.
func (q *Quota) IncTotalCalls(ctx context.Context, licenseID, route string) error {
	if err := q.ensure(ctx, licenseID, route); err != nil {
		return err
	}
	if err := q.rdb.Incr(ctx, kCallTotal(licenseID, route)).Err(); err != nil {
		return err
	}
	return q.pg.AddTotalCalls(ctx, licenseID, route, 1)
}

// BillingCounters reads 该客户的计费统计 (Redis, PG-mirrored)。
func (q *Quota) BillingCounters(ctx context.Context, licenseID, route string) (model.BillingCounters, error) {
	if err := q.ensure(ctx, licenseID, route); err != nil {
		return model.BillingCounters{}, err
	}
	var out model.BillingCounters
	for _, p := range []struct {
		key string
		dst *int64
	}{
		{kBillInvoice(licenseID, route), &out.ChargedInvoice},
		{kBillTax(licenseID, route), &out.ChargedTax},
		{kBillBoth(licenseID, route), &out.ChargedBoth},
		{kAmountFen(licenseID, route), &out.AmountFen},
	} {
		n, err := q.getCounter(ctx, p.key)
		if err != nil {
			return model.BillingCounters{}, err
		}
		*p.dst = n
	}
	return out, nil
}

// AddBilling 累加一笔计费：档位笔数 +1、累计应收 += amountFen (Redis) 并镜像到 PG。
func (q *Quota) AddBilling(ctx context.Context, licenseID, route string, standard model.FeeStandard, amountFen int64) error {
	key, ok := kBillStandard(licenseID, route, standard)
	if !ok {
		return nil // none：本次不计费
	}
	if err := q.ensure(ctx, licenseID, route); err != nil {
		return err
	}
	if err := q.rdb.Incr(ctx, key).Err(); err != nil {
		return err
	}
	if amountFen != 0 {
		if err := q.rdb.IncrBy(ctx, kAmountFen(licenseID, route), amountFen).Err(); err != nil {
			return err
		}
	}
	return q.pg.AddBillingCount(ctx, licenseID, route, standard, amountFen)
}
