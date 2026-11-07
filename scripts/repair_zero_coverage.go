//go:build ignore

// repair_zero_coverage: 「近 12 个月金额为 0 不计费、不开免费期」上线前，零金额的查得照样
// 开了一年免费期（billing_coverage），之后同一年内有金额的查得被判为免费，永远收不到钱。
// 本脚本按新规则从历史重放主体年度计费，修正免费期窗口与各请求的计费结论。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/repair_zero_coverage.go          # 只统计不写库
//	CONFIG_FILE=config.aliyun.prod.yaml APPLY=1 go run ./scripts/repair_zero_coverage.go  # 写库
//
// 可选环境变量：
//
//	APPLY=1   真正写库；缺省只打印将要变化的主体与请求
//	ROUTE=swfp
//
// 重放口径（与线上 Bookkeeper 一致）：
//   - 只看 audit_log 里查得 (found_data) 且有计费判定的请求（CHARGED / FREE / ZERO），按时间
//     顺序逐类目重放。幂等重放（charge_state 为空）与 DEFERRED 不参与、不改动。
//   - 某类目在该请求的 request_payload 里判定为 0：不计费、不动窗口（ZERO）。
//   - 没有 payload 或判定为空（无从判定）：按有金额处理，与线上一致。
//   - 无窗口或已过期：计费并开窗一年（周年制）；窗口内：免费命中。
//   - 只修正存在零金额查得的主体，其余主体的历史不碰。
//
// 写库内容：billing_coverage（改窗口或删行）、audit_log / billing_ledger 的
// charge_state / charged_scope / covered_scope / fee_standard / amount_fen（audit 另改 billed），
// billing_charge（不再计费的删除、新计费的补写）。
//
// 计费计数器（quota 的 BILL_* / AMOUNT_FEN，Redis 为准、PG 镜像）不在本脚本里改：脚本
// 打印每个客户的计数差额，由运维按差额核对调整。
//
// 请在低峰期运行：重放与写库之间到达的新请求不在本次修正范围内。
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/datahub/relay/internal/domain/model"
)

type repairConfig struct {
	Billing struct {
		Rates struct {
			BothFen    int64 `yaml:"bothFen"`
			InvoiceFen int64 `yaml:"invoiceFen"`
			TaxFen     int64 `yaml:"taxFen"`
		} `yaml:"rates"`
	} `yaml:"billing"`
	Versions map[string]struct {
		Database struct {
			Host     string `yaml:"host"`
			Port     int    `yaml:"port"`
			Name     string `yaml:"name"`
			User     string `yaml:"user"`
			Password string `yaml:"password"`
			SSLMode  string `yaml:"sslmode"`
		} `yaml:"database"`
	} `yaml:"versions"`
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
	os.Exit(1)
}

// request 是参与重放的一次查得请求。
type request struct {
	id                       int64
	requestID, reqid, appKey string
	licenseID, creditCode    string
	at                       time.Time
	got                      model.DimSet
	invZero, taxZero         *bool
	rates                    model.FeeRates
	state                    string
	chargedScope             string
	coveredScope             string

	// 重放结果
	charged, covered, zero model.DimSet
	windowTo               time.Time
	kind                   string
}

// window 是某主体某类目重放后的免费期。
type window struct {
	firstChargedAt, chargedAt, expiresAt time.Time
	chargeCount                          int
	freeHits                             int64
	lastHitAt                            time.Time
	chargeReqid, chargeRequestID         string
}

type subjectKey struct{ licenseID, creditCode string }

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	apply := env("APPLY", "") == "1"
	route := env("ROUTE", "swfp")

	raw, err := os.ReadFile(configFile)
	if err != nil {
		fail("读取配置失败: %v", err)
	}
	var cfg repairConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		fail("解析配置失败: %v", err)
	}
	db := cfg.Versions[route].Database
	if db.Host == "" {
		fail("配置里没有 versions.%s.database", route)
	}
	if db.Port == 0 {
		db.Port = 5432
	}
	if db.SSLMode == "" {
		db.SSLMode = "disable"
	}
	defRates := model.FeeRates{BothFen: cfg.Billing.Rates.BothFen, InvoiceFen: cfg.Billing.Rates.InvoiceFen, TaxFen: cfg.Billing.Rates.TaxFen}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=15",
		db.Host, db.Port, db.User, db.Password, db.Name, db.SSLMode))
	if err != nil {
		fail("连接数据库失败: %v", err)
	}
	defer conn.Close(ctx)

	reqs, err := loadRequests(ctx, conn, route, defRates)
	if err != nil {
		fail("读取查得请求失败: %v", err)
	}
	bySubject := map[subjectKey][]*request{}
	var order []subjectKey
	for _, r := range reqs {
		k := subjectKey{r.licenseID, r.creditCode}
		if _, ok := bySubject[k]; !ok {
			order = append(order, k)
		}
		bySubject[k] = append(bySubject[k], r)
	}

	fmt.Printf("模式：%s；查得请求 %d 条，主体 %d 个\n", map[bool]string{true: "写库", false: "只统计"}[apply], len(reqs), len(order))

	deltas := map[string]*delta{}
	var subjects, changedReqs int
	for _, k := range order {
		rs := bySubject[k]
		if !hasZero(rs) {
			continue
		}
		windows := replay(rs)
		var changed []*request
		for _, r := range rs {
			newState := stateOf(r)
			if newState != r.state || r.charged.String() != r.chargedScope || r.covered.String() != r.coveredScope {
				changed = append(changed, r)
			}
		}
		if len(changed) == 0 {
			continue
		}
		subjects++
		changedReqs += len(changed)
		fmt.Printf("主体 license=%s 税号=%s：%d 条请求结论变化\n", k.licenseID, k.creditCode, len(changed))
		for _, r := range changed {
			oldStd, newStd := standardOfScope(r.chargedScope), model.StandardOf(r.charged)
			fmt.Printf("  %s %s  %s/%s -> %s/%s\n", r.at.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05"),
				r.requestID, r.state, r.chargedScope, stateOf(r), r.charged.String())
			d := deltas[r.licenseID]
			if d == nil {
				d = &delta{}
				deltas[r.licenseID] = d
			}
			countStd(d, oldStd, -1)
			countStd(d, newStd, +1)
			d.amount += r.rates.Of(newStd) - r.rates.Of(oldStd)
		}
		if apply {
			if err := write(ctx, conn, route, k, windows, changed); err != nil {
				fail("写库失败（该主体已回滚）license=%s 税号=%s: %v", k.licenseID, k.creditCode, err)
			}
		}
	}

	fmt.Printf("需修正主体 %d 个，请求 %d 条\n", subjects, changedReqs)
	if len(deltas) > 0 {
		fmt.Println("计费计数器差额（请按此核对调整 quota 的 BILL_* / AMOUNT_FEN）：")
		ids := make([]string, 0, len(deltas))
		for id := range deltas {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			d := deltas[id]
			fmt.Printf("  license=%s BILL_BOTH %+d BILL_INVOICE %+d BILL_TAX %+d AMOUNT_FEN %+d\n", id, d.both, d.inv, d.tax, d.amount)
		}
	}
	if !apply && changedReqs > 0 {
		fmt.Println("只统计未写库；确认无误后加 APPLY=1 重跑。")
	}
}

// delta 是某客户计费计数器的差额（新结论减旧结论）。
type delta struct{ both, inv, tax, amount int64 }

func countStd(d *delta, s model.FeeStandard, n int64) {
	switch s {
	case model.FeeBoth:
		d.both += n
	case model.FeeInvoice:
		d.inv += n
	case model.FeeTax:
		d.tax += n
	}
}

func standardOfScope(scope string) model.FeeStandard {
	switch scope {
	case "invoice+tax":
		return model.FeeBoth
	case "invoice":
		return model.FeeInvoice
	case "tax":
		return model.FeeTax
	}
	return model.FeeNone
}

func isZero(b *bool) bool { return b != nil && *b }

func hasZero(rs []*request) bool {
	for _, r := range rs {
		if (r.got.Invoice && isZero(r.invZero)) || (r.got.Tax && isZero(r.taxZero)) {
			return true
		}
	}
	return false
}

func stateOf(r *request) string {
	switch {
	case !r.charged.Empty():
		return string(model.ChargeCharged)
	case !r.covered.Empty():
		return string(model.ChargeFree)
	default:
		return string(model.ChargeZero)
	}
}

// replay 按时间顺序逐类目重放，回填每条请求的 charged/covered/zero，返回各类目的最终窗口。
func replay(rs []*request) map[model.Category]*window {
	windows := map[model.Category]*window{}
	for _, r := range rs {
		r.charged, r.covered, r.zero = model.DimSet{}, model.DimSet{}, model.DimSet{}
		var first, renew bool
		for _, cat := range model.CategoriesOf(r.got) {
			dim := model.NewDimSet(cat.Dim())
			zero := (cat == model.CatInvoice && isZero(r.invZero)) || (cat == model.CatTax && isZero(r.taxZero))
			if zero {
				r.zero = r.zero.Union(dim)
				continue
			}
			w := windows[cat]
			switch {
			case w == nil:
				w = &window{firstChargedAt: r.at, chargedAt: r.at, expiresAt: r.at.AddDate(1, 0, 0),
					chargeCount: 1, chargeReqid: r.reqid, chargeRequestID: r.requestID}
				windows[cat] = w
				r.charged = r.charged.Union(dim)
				first = true
			case !r.at.Before(w.expiresAt):
				w.chargedAt, w.expiresAt = r.at, r.at.AddDate(1, 0, 0)
				w.chargeCount++
				w.freeHits = 0
				w.chargeReqid, w.chargeRequestID = r.reqid, r.requestID
				r.charged = r.charged.Union(dim)
				renew = true
			default:
				w.freeHits++
				r.covered = r.covered.Union(dim)
			}
			w.lastHitAt = r.at
			if r.charged.Has(cat.Dim()) && w.expiresAt.After(r.windowTo) {
				r.windowTo = w.expiresAt
			}
		}
		switch {
		case first && renew:
			r.kind = "mixed"
		case first:
			r.kind = "first"
		case renew:
			r.kind = "renew"
		}
	}
	return windows
}

func loadRequests(ctx context.Context, conn *pgx.Conn, route string, def model.FeeRates) ([]*request, error) {
	rows, err := conn.Query(ctx, `
SELECT a.id, a.request_id, COALESCE(a.reqid,''), a.app_key, l.license_id, a.credit_code, a.created_at,
       COALESCE(a.data_scope,''), p.inv_zero, p.tax_zero,
       COALESCE(l.rate_both_fen,0), COALESCE(l.rate_invoice_fen,0), COALESCE(l.rate_tax_fen,0),
       a.charge_state, a.charged_scope, a.covered_scope
FROM audit_log a
JOIN license l ON l.app_key = a.app_key
LEFT JOIN request_payload p ON p.request_id = a.request_id
WHERE a.found_data AND a.version = $1 AND a.credit_code <> ''
  AND a.charge_state IN ('CHARGED','FREE','ZERO')
ORDER BY a.created_at, a.id`, route)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*request
	for rows.Next() {
		r := &request{}
		var scope string
		if err := rows.Scan(&r.id, &r.requestID, &r.reqid, &r.appKey, &r.licenseID, &r.creditCode, &r.at,
			&scope, &r.invZero, &r.taxZero, &r.rates.BothFen, &r.rates.InvoiceFen, &r.rates.TaxFen,
			&r.state, &r.chargedScope, &r.coveredScope); err != nil {
			return nil, err
		}
		r.got = model.DimSet{Invoice: scope == "invoice" || scope == "invoice+tax", Tax: scope == "tax" || scope == "invoice+tax"}
		r.rates = r.rates.OrDefault(def)
		out = append(out, r)
	}
	return out, rows.Err()
}

func write(ctx context.Context, conn *pgx.Conn, route string, k subjectKey, windows map[model.Category]*window, changed []*request) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for _, cat := range []model.Category{model.CatInvoice, model.CatTax} {
			w := windows[cat]
			if w == nil {
				if _, err := tx.Exec(ctx, `DELETE FROM billing_coverage WHERE license_id=$1 AND route=$2 AND credit_code=$3 AND category=$4`,
					k.licenseID, route, k.creditCode, string(cat)); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO billing_coverage (license_id, route, credit_code, category, first_charged_at, charged_at, expires_at,
                              charge_count, free_hits, last_hit_at, charge_reqid, charge_request_id, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now())
ON CONFLICT (license_id, route, credit_code, category) DO UPDATE SET
  first_charged_at=$5, charged_at=$6, expires_at=$7, charge_count=$8, free_hits=$9, last_hit_at=$10,
  charge_reqid=$11, charge_request_id=$12, updated_at=now()`,
				k.licenseID, route, k.creditCode, string(cat), w.firstChargedAt, w.chargedAt, w.expiresAt,
				w.chargeCount, w.freeHits, w.lastHitAt, w.chargeReqid, w.chargeRequestID); err != nil {
				return err
			}
		}
		for _, r := range changed {
			std := model.StandardOf(r.charged)
			amount := r.rates.Of(std)
			state := stateOf(r)
			if _, err := tx.Exec(ctx, `
UPDATE audit_log SET charge_state=$2, charged_scope=$3, covered_scope=$4, fee_standard=$5, amount_fen=$6, billed=$7
WHERE request_id=$1`, r.requestID, state, r.charged.String(), r.covered.String(), string(std), amount, !r.charged.Empty()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
UPDATE billing_ledger SET charge_state=$2, charged_scope=$3, covered_scope=$4, fee_standard=$5, amount_fen=$6
WHERE request_id=$1`, r.requestID, state, r.charged.String(), r.covered.String(), string(std), amount); err != nil {
				return err
			}
			if r.charged.Empty() {
				if _, err := tx.Exec(ctx, `DELETE FROM billing_charge WHERE license_id=$1 AND route=$2 AND reqid=$3`,
					r.licenseID, route, r.reqid); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO billing_charge (license_id, app_key, route, credit_code, reqid, request_id, ledger_id,
                            charged_invoice, charged_tax, covered_invoice, covered_tax,
                            fee_standard, amount_fen, kind, window_to, created_at)
VALUES ($1,$2,$3,$4,$5,$6,(SELECT id FROM billing_ledger WHERE request_id=$6 LIMIT 1),$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (license_id, route, reqid) DO UPDATE SET
  charged_invoice=$7, charged_tax=$8, covered_invoice=$9, covered_tax=$10,
  fee_standard=$11, amount_fen=$12, kind=$13, window_to=$14`,
				r.licenseID, r.appKey, route, r.creditCode, r.reqid, r.requestID,
				r.charged.Invoice, r.charged.Tax, r.covered.Invoice, r.covered.Tax,
				string(std), amount, r.kind, r.windowTo, r.at); err != nil {
				return err
			}
		}
		return nil
	})
}
