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

// ParseCreditCode 校验 swfp 入参：creditCode 必填；scope 可选 (all/basic)。
// 失败返回 busiCode 1007，不调上游/不计费。
func ParseCreditCode(cmd *model.QueryCommand) (*model.UpstreamRequest, error) {
	if cmd == nil {
		return nil, errs.New(errs.BusiDataRequestErr, "请求体为空")
	}
	creditCode := strings.ToUpper(strings.TrimSpace(cmd.CreditCode))
	if !creditCodeRe.MatchString(creditCode) {
		return nil, errs.New(errs.BusiDataRequestErr, "creditCode 格式非法")
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
		Reqid:      NewReqid(),
	}, nil
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
