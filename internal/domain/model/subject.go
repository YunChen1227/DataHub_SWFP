package model

import "time"

// 本文件承载「主体年度计费」的核心类型 (设计_主体年度计费.md)：同一客户 + 同一社会
// 信用代码 + 同一类目，首次查得计费一次，此后一年内免费。
//
// 它与既有的「按实得维度定档」是两个正交的轴，相乘即可，不新增定价档位：
//
//	charged  = Got − Covered          实得的类目，减掉还在免费期的
//	Standard = StandardOf(charged)    复用 both/invoice/tax/none 四档
//	Amount   = rates.Of(Standard)     复用 license 上的三档合同价
//
// 计费主体三要素里用 licenseID 而非 appKey：appKey 会因重建账号而变，一变则该客户的
// 全部免费期凭空消失，客户视角就是重复收费。

// Category 是计费类目。它与 Dimension 取值相同但语义不同：Dimension 描述「数据的
// 维度」（寻源用），Category 描述「计费的类目」（免费窗口用）。分成两个类型是为了
// 让「按维度寻源」与「按类目计费」在类型上不会互相串用。
type Category string

const (
	CatInvoice Category = "invoice"
	CatTax     Category = "tax"
)

// Dim 返回该类目对应的数据维度。
func (c Category) Dim() Dimension {
	if c == CatTax {
		return DimTax
	}
	return DimInvoice
}

// CategoriesOf 把维度集展开为类目列表，顺序稳定（发票在前）——落库与日志的顺序
// 稳定，测试才好断言。
func CategoriesOf(d DimSet) []Category {
	out := make([]Category, 0, 2)
	if d.Invoice {
		out = append(out, CatInvoice)
	}
	if d.Tax {
		out = append(out, CatTax)
	}
	return out
}

// ChargeState 是本次请求的计费标识（落 billing_ledger / audit_log 的 charge_state）。
// 它取代了旧的「billed = 是否查得数据」这个与 found_data 完全重复的口径。
type ChargeState string

const (
	ChargeCharged  ChargeState = "CHARGED"  // 本次产生应收
	ChargeFree     ChargeState = "FREE"     // 查得了数据，但全部类目都在免费期内
	ChargeNoCharge ChargeState = "NOCHARGE" // 没查得数据 (999/002/失败)，无从计费
	ChargeDeferred ChargeState = "DEFERRED" // 计费判定本身失败，按 0 收，留待对账补记
)

// DefaultFreeWindow 是免费期长度的缺省值，**Postgres 日历 interval 字面量**。
// 必须用日历 interval 而非 Go 的 8760h：周年制下 2026-12-28 计费应免到 2027-12-28，
// 用固定小时数会在闰年差一天，客户会在 2 月 29 日附近发现被提前收费。
const DefaultFreeWindow = "1 year"

// CoverageRequest 是一次主体计费判定的输入。Got 是唯一的判定依据——只有真的拿到
// 数据才动窗口，查无/部分源异常/全源失败一律不计费、不开窗口。
type CoverageRequest struct {
	LicenseID  string
	Route      string
	CreditCode string
	Got        DimSet // 本次实际查得的类目
	Reqid      string
	RequestID  string
	Window     string // Postgres interval；空则取 DefaultFreeWindow
}

// CategoryVerdict 是单个类目的判定结论，由那条原子 UPSERT 的 RETURNING 得出。
type CategoryVerdict struct {
	Category    Category
	Charged     bool      // 本次是否计费（首次开窗或到期续期）
	FirstEver   bool      // 该主体该类目历史首次（真 INSERT）
	ExpiresAt   time.Time // 判定后该类目的免费期截止
	ChargeCount int       // 该主体该类目累计计费轮数
}

// CoverageResult 汇总一次判定的逐类目结论。不变式：Charged() ∪ Covered() == 请求的 Got。
type CoverageResult struct {
	Verdicts []CategoryVerdict
}

// Charged 返回本次真正产生应收的类目集 = 收费标准的输入。
func (r *CoverageResult) Charged() DimSet {
	var d DimSet
	for _, v := range r.Verdicts {
		if v.Charged {
			d = d.Union(NewDimSet(v.Category.Dim()))
		}
	}
	return d
}

// Covered 返回本次命中免费期（查得了但不收钱）的类目集。
func (r *CoverageResult) Covered() DimSet {
	var d DimSet
	for _, v := range r.Verdicts {
		if !v.Charged {
			d = d.Union(NewDimSet(v.Category.Dim()))
		}
	}
	return d
}

// Kind 描述本次计费的性质：first(全是首次) / renew(全是到期续期) / mixed(两者兼有)；
// 未产生应收时为空。
func (r *CoverageResult) Kind() string {
	var first, renew bool
	for _, v := range r.Verdicts {
		if !v.Charged {
			continue
		}
		if v.FirstEver {
			first = true
		} else {
			renew = true
		}
	}
	switch {
	case first && renew:
		return "mixed"
	case first:
		return "first"
	case renew:
		return "renew"
	default:
		return ""
	}
}

// WindowTo 是本次开启/续期的窗口截止（多类目时取最晚），落进 billing_charge 作快照
// ——billing_coverage 会被续期覆盖，事件表必须自己留一份。
func (r *CoverageResult) WindowTo() time.Time {
	var t time.Time
	for _, v := range r.Verdicts {
		if v.Charged && v.ExpiresAt.After(t) {
			t = v.ExpiresAt
		}
	}
	return t
}

// SubjectCharge 是一条计费事件（billing_charge 的一行）。一次请求一行、不是一个类目
// 一行：both 是一个打包价而非两个单价之和，硬拆到类目行上会产生任意分摊，对账反而
// 说不清；用两组布尔表达「为哪些类目收了钱」，既能按类目统计又不必编造分摊金额。
type SubjectCharge struct {
	LicenseID       string
	AppKey          string
	Route           string
	CreditCode      string
	Reqid           string
	RequestID       string
	LedgerID        int64
	Charged         DimSet
	Covered         DimSet
	FeeStandard     FeeStandard
	AmountFen       int64
	UpstreamCostFen int64
	Kind            string
	WindowTo        time.Time
	CreatedAt       time.Time
}

// CategoryCoverage 是单个类目的免费期视图（对客自查 + 后台展示）。
type CategoryCoverage struct {
	Covered     bool      `json:"covered"`
	ChargedAt   time.Time `json:"chargedAt,omitempty"`
	ExpiresAt   time.Time `json:"expiresAt,omitempty"`
	ChargeCount int       `json:"chargeCount,omitempty"`
	FreeHits    int64     `json:"freeHits,omitempty"` // 内部成本/使用强度指标，不对客返回
}

// SubjectCoverage 是某主体两个类目的免费期状态。
type SubjectCoverage struct {
	CreditCode string           `json:"creditCode"`
	Invoice    CategoryCoverage `json:"invoice"`
	Tax        CategoryCoverage `json:"tax"`
}

// BillingCounters 是客户可见的计费口径统计。它与 ServiceQuotaView 里的
// Used/Calls（调用口径）严格分开、互不换算：一次调用可能不计费（免费期内），
// 所以 ChargedTotal 与 Calls 之间没有任何加减关系。
type BillingCounters struct {
	ChargedInvoice int64 `json:"chargedInvoice"` // 发票计费笔数
	ChargedTax     int64 `json:"chargedTax"`     // 税务计费笔数
	ChargedBoth    int64 `json:"chargedBoth"`    // 税务发票计费笔数
	ChargedTotal   int64 `json:"chargedTotal"`   // 三者之和
	AmountFen      int64 `json:"amountFen"`      // 累计应收（分）
}
