package auth

import (
	"github.com/datahub/relay/internal/common/errs"
	"github.com/datahub/relay/internal/domain/model"
)

// CheckDims 校验本次请求维度是否在客户已开通的接口范围内。want 为空按票税
// （全维度）处理；超出范围直接拒绝（不降级），返回 1009 并在 msg 里写明开通情况。
func CheckDims(lic *model.LicenseView, want model.DimSet) error {
	if want.Empty() {
		want = model.AllDims()
	}
	allowed := lic.Allowed()
	if allowed.Covers(want) {
		return nil
	}
	var msg string
	switch {
	case allowed.Tax:
		msg = "服务尚未开通：仅允许访问税务接口"
	case allowed.Invoice:
		msg = "服务尚未开通：仅允许访问发票接口"
	default:
		msg = "服务尚未开通：发票、税务接口均未开通"
	}
	return errs.New(errs.BusiServiceNotOpen, msg)
}
