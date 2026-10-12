package upstream

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/parse"
	"github.com/datahub/relay/internal/domain/port"
)

// sy（税友，源7）的调用白名单。sy 是惠众按主体计费的老接口：对账单里已经计过费的
// (企业, 维度) 再调不产生新费用，其余任何调用都是实打实的新成本。故 sy 只允许调
// 「对账单里该企业计过费的维度」，过了 until 一律不调。
//
// 文件格式 (scripts/convert_sy_allowlist.go 由对账单 xlsx 生成)：
//
//	creditCode,tax,invoice,callTime
//	913101165647980623,1,1,2026-09-13T15:27:00+08:00
//
// 加载是 fail-closed 的：任何一行不合法都整体报错，调用方据此拒绝启动——宁可没有
// sy，也不能带着一张半对的白名单上线。
type SyAllowlist struct {
	dims  map[string]model.DimSet
	until time.Time
}

// SyAllowlistStats 是白名单的分类计数（启动日志用）。
type SyAllowlistStats struct {
	Total, TaxOnly, InvoiceOnly, Both int
}

var syAllowlistHeader = []string{"creditCode", "tax", "invoice", "callTime"}

// LoadSyAllowlist 读取并严格校验白名单文件。until 为零值视为配置缺失，直接报错。
func LoadSyAllowlist(path string, until time.Time) (*SyAllowlist, error) {
	if until.IsZero() {
		return nil, errors.New("sy 白名单: allowUntil 未配置")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("sy 白名单: %w", err)
	}
	defer f.Close()
	return parseSyAllowlist(f, until)
}

func parseSyAllowlist(r io.Reader, until time.Time) (*SyAllowlist, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(syAllowlistHeader)
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("sy 白名单: 读表头: %w", err)
	}
	for i, h := range syAllowlistHeader {
		if strings.TrimPrefix(strings.TrimSpace(header[i]), "\ufeff") != h {
			return nil, fmt.Errorf("sy 白名单: 表头应为 %s, got %s",
				strings.Join(syAllowlistHeader, ","), strings.Join(header, ","))
		}
	}

	dims := map[string]model.DimSet{}
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("sy 白名单: 第 %d 行: %w", line, err)
		}
		code, err := parse.NormalizeCreditCode(rec[0])
		if err != nil {
			return nil, fmt.Errorf("sy 白名单: 第 %d 行税号非法: %q", line, rec[0])
		}
		if _, dup := dims[code]; dup {
			return nil, fmt.Errorf("sy 白名单: 第 %d 行税号重复: %s", line, code)
		}
		tax, err1 := syFlag(rec[1])
		inv, err2 := syFlag(rec[2])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("sy 白名单: 第 %d 行 tax/invoice 只能是 0/1: %q/%q", line, rec[1], rec[2])
		}
		d := model.DimSet{Tax: tax, Invoice: inv}
		if d.Empty() {
			return nil, fmt.Errorf("sy 白名单: 第 %d 行 tax/invoice 全为 0: %s", line, code)
		}
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(rec[3])); err != nil {
			return nil, fmt.Errorf("sy 白名单: 第 %d 行 callTime 非法: %q", line, rec[3])
		}
		dims[code] = d
	}
	if len(dims) == 0 {
		return nil, errors.New("sy 白名单: 没有任何条目")
	}
	return &SyAllowlist{dims: dims, until: until}, nil
}

func syFlag(s string) (bool, error) {
	switch strings.TrimSpace(s) {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("非 0/1: %q", s)
}

// Allowed 返回 creditCode 此刻允许向 sy 请求的维度：已过 until、税号非法或不在白名单
// 时为空集。
func (a *SyAllowlist) Allowed(creditCode string, now time.Time) model.DimSet {
	if a == nil || !now.Before(a.until) {
		return model.DimSet{}
	}
	code, err := parse.NormalizeCreditCode(creditCode)
	if err != nil {
		return model.DimSet{}
	}
	return a.dims[code]
}

// Until 是白名单的截止时间（含义：从该时刻起不再调用 sy）。
func (a *SyAllowlist) Until() time.Time { return a.until }

// Stats 返回分类计数。
func (a *SyAllowlist) Stats() SyAllowlistStats {
	s := SyAllowlistStats{Total: len(a.dims)}
	for _, d := range a.dims {
		switch {
		case d.Both():
			s.Both++
		case d.Tax:
			s.TaxOnly++
		default:
			s.InvoiceOnly++
		}
	}
	return s
}

// GatedPort 是 sy 真实 port 外的第二道闸：寻源器已按白名单收窄过 Want，这里再校验
// 一次，Want 为空或超出白名单此刻允许的维度就拒绝，**不发任何网络请求**。
type GatedPort struct {
	inner port.UpstreamPort
	allow *SyAllowlist
	now   func() time.Time
}

// NewGatedPort wraps inner with the allowlist check。now 为 nil 时用 time.Now。
func NewGatedPort(inner port.UpstreamPort, allow *SyAllowlist, now func() time.Time) *GatedPort {
	if now == nil {
		now = time.Now
	}
	return &GatedPort{inner: inner, allow: allow, now: now}
}

func (g *GatedPort) Query(ctx context.Context, req *model.UpstreamRequest) (*model.UpstreamResult, error) {
	allowed := g.allow.Allowed(req.CreditCode, g.now())
	if req.Want.Empty() || !allowed.Covers(req.Want) {
		return nil, fmt.Errorf("sy 白名单拒绝: creditCode=%s 请求维度=%s 允许维度=%s",
			req.CreditCode, req.Want.String(), allowed.String())
	}
	return g.inner.Query(ctx, req)
}

// Requery 不经上游（sy 无复查接口），恒未决。
func (g *GatedPort) Requery(ctx context.Context, reqid string) (*model.RequeryResult, error) {
	return &model.RequeryResult{Reachable: false}, nil
}
