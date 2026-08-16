// Package billing turns an upstream result into a charge verdict (DESIGN §7.4).
// The decision table is config-driven so it can be aligned with the upstream's
// actual扣费口径 without code changes.
package billing

import "github.com/datahub/relay/internal/domain/model"

// DecisionTable separates two independent verdicts per upstream code (DESIGN §7.4):
//   - resolvedCodes → 上游给出了确定结论（查得或查无）→ 台账 BILLED。
//   - returnedCodes → 查得数据（成功查得数 +1，= busiCode 10）。
//
// 两者解耦：999 查无结果 是确定结论(resolved) 但非查得数据(not returned)。
type DecisionTable struct {
	resolvedCodes map[string]bool
	returnedCodes map[string]bool
}

// DefaultTable reflects DESIGN §7.4:
//   - RESOLVED_CODES = {001, 999, 002}（上游确定结论）
//   - RETURNED_CODES = {001}（仅查得数据才累计成功查得数）
//
// 002 为多源寻源路由 (swfp) 特有，语义已收窄为「未取得任何数据且部分数据源异常」
// ——确定结论、不计费。取得了数据（哪怕只有请求维度的一半）一律是 001，按实得
// 维度定档收费，见 model.StandardOf。单上游路由永不产生 002。
func DefaultTable() *DecisionTable {
	return &DecisionTable{
		resolvedCodes: map[string]bool{
			"001": true, // 成功
			"999": true, // 查无结果（上游已给出确定结论）
			"002": true, // 部分数据源成功（聚合路由；确定结论、不计费）
		},
		returnedCodes: map[string]bool{
			"001": true, // 仅查得数据才累计成功查得数
		},
	}
}

// NewTable builds a table from explicit resolved/returned code sets (config).
func NewTable(resolvedCodes, returnedCodes map[string]bool) *DecisionTable {
	return &DecisionTable{
		resolvedCodes: copySet(resolvedCodes),
		returnedCodes: copySet(returnedCodes),
	}
}

func copySet(src map[string]bool) map[string]bool {
	cp := make(map[string]bool, len(src))
	for k, v := range src {
		cp[k] = v
	}
	return cp
}

// IsResolved reports whether the upstream code is a确定结论 (查得/查无).
func (t *DecisionTable) IsResolved(code string) bool { return t.resolvedCodes[code] }

// IsReturned reports whether the upstream code means查得数据 (busiCode 10).
func (t *DecisionTable) IsReturned(code string) bool { return t.returnedCodes[code] }

// Service produces BillingDecisions. defaultRates 是三档收费标准的全局缺省单价
// (config billing.rates)，用于兜底 license 未单独定价的档位。
type Service struct {
	table        *DecisionTable
	defaultRates model.FeeRates
}

func New(table *DecisionTable) *Service {
	if table == nil {
		table = DefaultTable()
	}
	return &Service{table: table}
}

// WithDefaultRates 设置全局缺省费率（客户合同价优先，缺档由此兜底）。
func (s *Service) WithDefaultRates(r model.FeeRates) *Service {
	s.defaultRates = r
	return s
}

// Decide evaluates a direct upstream response. Resolved (确定结论) 与 Returned
// (查得数据→累计成功查得数) 相互独立：999 查无结果 是 Resolved=true, Returned=false
// (DESIGN §7.4)。
//
// 收费标准由【实际查得的维度】(r.Got) 决定，与下游请求了什么无关——请求两项只查得
// 发票就按【单发票】收费。金额取 rates（客户合同价）对应档位，缺档由全局缺省兜底。
func (s *Service) Decide(r *model.UpstreamResult, rates model.FeeRates) *model.BillingDecision {
	if r == nil {
		return &model.BillingDecision{Standard: model.FeeNone}
	}
	d := &model.BillingDecision{
		Resolved: s.table.IsResolved(r.Code),
		Returned: s.table.IsReturned(r.Code),
		Result:   r,
		Standard: model.StandardOf(r.Got),
		CostFen:  r.CostFen,
	}
	if d.Returned {
		d.AmountFen = rates.OrDefault(s.defaultRates).Of(d.Standard)
	}
	return d
}

// FromRequery evaluates an idempotent re-query outcome (DESIGN §7.3).
//   - Reachable + resolved code → BILLED.
//   - Reachable + non-resolved  → UNBILLED.
//   - Unreachable               → no decision yet (caller keeps PENDING for
//     reconciliation); represented as not-resolved/not-returned.
//
// 复查结果不携带维度信息（上游对账接口只答"这单执行了没有"），故本路径**不重判**
// 收费标准与金额：Standard 留空，由持久层的"空值不覆盖"保留首次结算写下的标准/
// 金额（见 postgres Store.Settle）。当前各上游 Requery 均为 stub（恒
// Reachable=false），此路径不会被走到；真正实现逐源复查时必须一并把各源的实得
// 维度带回来，否则会漏收。
func (s *Service) FromRequery(rr *model.RequeryResult) *model.BillingDecision {
	if rr == nil || !rr.Reachable || rr.Result == nil {
		return &model.BillingDecision{}
	}
	d := s.Decide(rr.Result, model.FeeRates{})
	d.Standard, d.AmountFen = "", 0
	return d
}
