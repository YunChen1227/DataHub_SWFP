package billing

import "github.com/datahub/relay/internal/domain/model"

// 主体年度计费的判定逻辑 (设计_主体年度计费.md §1)。
//
// 它是 Decide 之后的一步**收窄**，而不是对 Decide 的替换：
//
//	Decide        按实得维度给出毛口径 —— 「如果没有免费期，这次该收多少」
//	ApplyCoverage 扣掉还在免费期内的类目，按剩下的重算标准与金额
//
// 分成两步的好处是毛口径仍然可观测（免费期省了客户多少钱一目了然），且既有的
// 「按实得维度定档」测试与语义完全不受影响。

// Chargeable 是整条规则的规范表述：本次应收的类目 = 实得的类目 − 还在免费期内的类目。
//
// 复用 DimSet 现成的 Missing（语义为 want − d），因此这里不新增任何集合运算：
// covered.Missing(got) 即 got − covered。
func Chargeable(got, covered model.DimSet) model.DimSet { return covered.Missing(got) }

// ApplyCoverage 用主体年度计费的判定结论收窄 decision：收费标准与应收金额改按
// 【本次真正产生应收的类目】计算，而非全部实得类目。
//
// res 为空或无类目可判（查无 999 / 部分源异常 002 / 全源失败 / 复查路径不带维度）时
// **不改动** Standard 与 AmountFen：查无场景 Decide 已给出 none/0；复查场景则依赖
// 持久层的「空值不覆盖」保住首次结算写下的标准与金额（见 FromRequery 注释）。
func (s *Service) ApplyCoverage(d *model.BillingDecision, res *model.CoverageResult, rates model.FeeRates) {
	if d == nil {
		return
	}
	if res == nil || len(res.Verdicts) == 0 {
		if d.ChargeState == "" {
			d.ChargeState = model.ChargeNoCharge
		}
		return
	}
	d.Charged, d.Covered = res.Charged(), res.Covered()
	d.Standard = model.StandardOf(d.Charged)
	d.AmountFen = rates.OrDefault(s.defaultRates).Of(d.Standard)
	if d.Charged.Empty() {
		d.ChargeState = model.ChargeFree // 查得了数据，但全部类目都在免费期内
		return
	}
	d.ChargeState = model.ChargeCharged
}

// MarkCoverageDeferred 标记主体计费判定本身失败（免费期库不可用）。**fail-closed**：
// 本次按 0 收并标 DEFERRED，由对账任务凭台账上的 credit_code + reqid 重放那条幂等的
// 原子 SQL 补记。
//
// 宁可暂时少收，也绝不 fail-open：后者会让客户在对账时发现同一主体一年内被收了两次，
// 那是最难解释、最伤信任的错误。
func MarkCoverageDeferred(d *model.BillingDecision) {
	if d == nil {
		return
	}
	d.Charged, d.Covered = model.DimSet{}, model.DimSet{}
	d.Standard, d.AmountFen = model.FeeNone, 0
	d.ChargeState = model.ChargeDeferred
}
