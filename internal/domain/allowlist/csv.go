package allowlist

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/parse"
)

// CSV 表头：creditCode,provider,tax,invoice,callTime
var csvHeader = []string{"creditCode", "provider", "tax", "invoice", "callTime"}

// LoadCSV 严格解析白名单 CSV；任一行非法即失败。
func LoadCSV(path string) ([]*model.UpstreamAllowEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读上游白名单: %w", err)
	}
	defer f.Close()
	return ParseCSV(f)
}

// ParseCSV 从 reader 解析白名单。
func ParseCSV(r io.Reader) ([]*model.UpstreamAllowEntry, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(csvHeader)
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("上游白名单: 读表头: %w", err)
	}
	for i, h := range csvHeader {
		if strings.TrimPrefix(strings.TrimSpace(header[i]), "\ufeff") != h {
			return nil, fmt.Errorf("上游白名单: 表头应为 %s, got %s",
				strings.Join(csvHeader, ","), strings.Join(header, ","))
		}
	}

	// 合并同 (code, provider)
	type key struct{ code, provider string }
	merged := map[key]*model.UpstreamAllowEntry{}
	order := []key{}
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("上游白名单: 第 %d 行: %w", line, err)
		}
		code, err := parse.NormalizeCreditCode(rec[0])
		if err != nil {
			return nil, fmt.Errorf("上游白名单: 第 %d 行税号非法: %q", line, rec[0])
		}
		provider := strings.TrimSpace(rec[1])
		if !validProvider(provider) {
			return nil, fmt.Errorf("上游白名单: 第 %d 行 provider 非法: %q", line, rec[1])
		}
		tax, err1 := flag(rec[2])
		inv, err2 := flag(rec[3])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("上游白名单: 第 %d 行 tax/invoice 只能是 0/1: %q/%q", line, rec[2], rec[3])
		}
		if !tax && !inv {
			return nil, fmt.Errorf("上游白名单: 第 %d 行 tax/invoice 全为 0: %s", line, code)
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(rec[4]))
		if err != nil {
			return nil, fmt.Errorf("上游白名单: 第 %d 行 callTime 非法: %q", line, rec[4])
		}
		k := key{code, provider}
		if cur, ok := merged[k]; ok {
			cur.Tax = cur.Tax || tax
			cur.Invoice = cur.Invoice || inv
			if t.After(cur.CallTime) {
				cur.CallTime = t
			}
			continue
		}
		merged[k] = &model.UpstreamAllowEntry{
			CreditCode: code,
			Provider:   provider,
			Tax:        tax,
			Invoice:    inv,
			CallTime:   t,
		}
		order = append(order, k)
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("上游白名单: 没有任何条目")
	}
	out := make([]*model.UpstreamAllowEntry, 0, len(order))
	for _, k := range order {
		out = append(out, merged[k])
	}
	return out, nil
}

func flag(s string) (bool, error) {
	switch strings.TrimSpace(s) {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("非 0/1: %q", s)
}
