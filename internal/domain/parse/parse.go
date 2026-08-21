// Package parse validates and normalises SWFP client requests into upstream
// request shapes. Provider-specific signing is handled by upstream clients.
package parse

import (
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/datahub/relay/internal/common/errs"
	"github.com/datahub/relay/internal/domain/model"
)

// 统一社会信用代码 (GB 32100)：18 位，字符集不含 I/O/S/V/Z。
var creditCodeRe = regexp.MustCompile(`^[0-9A-HJ-NPQRTUWXY]{2}\d{6}[0-9A-HJ-NPQRTUWXY]{10}$`)

// ParseCreditCode 校验 swfp 入参：creditCode 必填；dataType 可选
// (invoice/tax/both，缺省 both——老下游不传该字段时维度与改造前一致)；
// scope 可选 (all/basic)。失败返回 busiCode 1007，不调上游/不计费。
func ParseCreditCode(cmd *model.QueryCommand) (*model.UpstreamRequest, error) {
	if cmd == nil {
		return nil, errs.New(errs.BusiDataRequestErr, "请求体为空")
	}
	creditCode, err := NormalizeCreditCode(cmd.CreditCode)
	if err != nil {
		return nil, err
	}
	dataType := strings.ToLower(strings.TrimSpace(cmd.DataType))
	switch dataType {
	case "", model.DataTypeBoth:
		dataType = model.DataTypeBoth
	case model.DataTypeInvoice, model.DataTypeTax:
	default:
		return nil, errs.New(errs.BusiDataRequestErr, "dataType 取值非法, 须为 invoice / tax / both")
	}
	scope := strings.ToLower(strings.TrimSpace(cmd.Scope))
	switch scope {
	case "", model.ScopeAll:
		scope = model.ScopeAll
	case model.ScopeBasic:
	default:
		return nil, errs.New(errs.BusiDataRequestErr, "scope 取值非法, 须为 all 或 basic")
	}
	return &model.UpstreamRequest{
		CreditCode: creditCode,
		Scope:      scope,
		Want:       model.DimSetOf(dataType),
		Reqid:      NewReqid(),
	}, nil
}

// NormalizeCreditCode 归一化并校验统一社会信用代码。它同时是**主体年度计费的计费
// 键归一化函数**：免费期窗口按 (license, creditCode, category) 判定，同一家企业的
// 大小写/空格差异必须收敛到同一个键，否则客户会为同一主体被重复计费。查询接口与
// 免费期自查接口共用本函数，正是为了保证两侧算出同一个键。
func NormalizeCreditCode(raw string) (string, error) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if !creditCodeRe.MatchString(code) {
		return "", errs.New(errs.BusiDataRequestErr, "creditCode 格式非法")
	}
	return code, nil
}

var reqidSeq atomic.Uint64

// NewReqid generates an internal upstream reqid（≤20 位，同进程内绝不重复）。
func NewReqid() string {
	ts := strconv.FormatInt(time.Now().UnixNano(), 36)
	seq := strconv.FormatUint(reqidSeq.Add(1)%46656, 36)
	r := ts + seq
	if len(r) > 20 {
		r = r[:20]
	}
	return r
}
