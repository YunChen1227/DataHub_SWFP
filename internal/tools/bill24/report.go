package bill24

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/datahub/relay/internal/common/xlsx"
)

const timeLayout = "2006-01-02 15:04:05"

// writeBill 按冠辰月度对账单的格式出表：对账单 / <M>月明细 / 不计费明细 / 待确认。
func writeBill(path string, j job, m Month, results []Result) error {
	sort.SliceStable(results, func(a, b int) bool { return results[a].At.Before(results[b].At) })
	count := map[Iface]int{}
	var bill, zero, pend []Result
	for _, r := range results {
		switch r.Outcome {
		case OutcomeBill:
			count[r.Iface]++
			bill = append(bill, r)
		case OutcomeZero:
			zero = append(zero, r)
		default:
			pend = append(pend, r)
		}
	}

	wb := &xlsx.Workbook{}

	// 对账单：版式与模板一致，从 B 列起。单价/小计留空，由人工填写。
	title := j.issuer + "月度对账单"
	monthLabel := m.dateLabel()
	rows := [][]any{
		{nil, title},
		{nil, "客户名称", j.customer},
		{nil, "日期", "产品名称", "调用计费量", "单价", "小计", "备注"},
	}
	for i, iface := range Ifaces {
		date := any(nil)
		if i == 0 {
			date = monthLabel
		}
		rows = append(rows, []any{nil, date, iface.Product(), count[iface], nil, nil, iface.Label()})
	}
	note := fmt.Sprintf("近24月总金额<=0 不计费 %d 条，待确认 %d 条（见同名 sheet）", len(zero), len(pend))
	if j.foundOnly {
		note = "查得口径，未做金额判定"
	}
	rows = append(rows, []any{nil, "小计", nil, len(bill), nil, nil, note})
	dateWidth := 12.0
	if !m.wholeMonth() {
		dateWidth = 26
	}
	sum := wb.Add("对账单", nil, rows, []float64{4, dateWidth, 52, 12, 10, 10, 46})
	sum.HeaderRows = map[int]bool{2: true}
	last := len(Ifaces) + 3
	sum.Merges = []string{"B1:G1", "D2:G2", fmt.Sprintf("B4:B%d", last), fmt.Sprintf("B%d:C%d", last+1, last+1)}

	// <M>月明细：只放计费行，列与模板一致。
	detail := make([][]any, 0, len(bill))
	for _, r := range bill {
		detail = append(detail, []any{
			r.At.In(cst).Format(timeLayout), r.Credit, j.appID(r.Item), j.customer,
			flag(r.Iface.Tax()), flag(r.Iface.Invoice()), "",
		})
	}
	wb.Add(m.detailSheet(),
		[]string{"调用时间", "税号", "appId", "企业名称", "计费税", "计费票", "备注"},
		detail, []float64{20, 22, 16, 30, 8, 8, 20})

	if j.foundOnly {
		return save(path, wb)
	}

	zeroRows := make([][]any, 0, len(zero))
	for _, r := range zero {
		zeroRows = append(zeroRows, []any{
			r.At.In(cst).Format(timeLayout), r.Credit, r.Iface.Label(), r.Company,
			fmt.Sprintf("%d-%d", r.From, r.To), num(r.Inv), num(r.Tax), num(r.Total), r.RequestID,
		})
	}
	wb.Add("不计费明细",
		[]string{"调用时间", "税号", "接口", "查询主体", "24月窗口", "近24月开票金额", "近24月税务金额", "近24月总金额", "requestId"},
		zeroRows, []float64{20, 22, 14, 30, 14, 16, 16, 16, 36})

	pendRows := make([][]any, 0, len(pend))
	for _, r := range pend {
		pendRows = append(pendRows, []any{
			r.At.In(cst).Format(timeLayout), r.Credit, r.Iface.Label(), r.Outcome, r.Note, r.RequestID,
		})
	}
	wb.Add("待确认",
		[]string{"调用时间", "税号", "接口", "重调结果", "说明", "requestId"},
		pendRows, []float64{20, 22, 14, 10, 60, 36})

	return save(path, wb)
}

func save(path string, wb *xlsx.Workbook) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := wb.Write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func flag(b bool) int {
	if b {
		return 1
	}
	return 0
}

func num(s string) any {
	if s == "" {
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}
