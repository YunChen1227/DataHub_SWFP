package auth

import (
	"testing"

	"github.com/datahub/relay/internal/common/errs"
	"github.com/datahub/relay/internal/domain/model"
)

func TestCheckDims(t *testing.T) {
	inv := model.DimSet{Invoice: true}
	tax := model.DimSet{Tax: true}
	both := model.AllDims()
	none := model.DimSet{}

	cases := []struct {
		name       string
		allowInv   bool
		allowTax   bool
		want       model.DimSet
		wantErrMsg string // 空 = 放行
	}{
		{"两项都开-票税", true, true, both, ""},
		{"两项都开-发票", true, true, inv, ""},
		{"两项都开-税务", true, true, tax, ""},
		{"两项都开-不传按票税", true, true, none, ""},
		{"只开税-税务放行", false, true, tax, ""},
		{"只开税-票税拒绝", false, true, both, "服务尚未开通：仅允许访问税务接口"},
		{"只开税-不传按票税拒绝", false, true, none, "服务尚未开通：仅允许访问税务接口"},
		{"只开税-发票拒绝", false, true, inv, "服务尚未开通：仅允许访问税务接口"},
		{"只开票-发票放行", true, false, inv, ""},
		{"只开票-票税拒绝", true, false, both, "服务尚未开通：仅允许访问发票接口"},
		{"只开票-税务拒绝", true, false, tax, "服务尚未开通：仅允许访问发票接口"},
		{"都没开-税务拒绝", false, false, tax, "服务尚未开通：发票、税务接口均未开通"},
		{"都没开-票税拒绝", false, false, both, "服务尚未开通：发票、税务接口均未开通"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lic := &model.LicenseView{Status: "ACTIVE", AllowInvoice: c.allowInv, AllowTax: c.allowTax}
			err := CheckDims(lic, c.want)
			if c.wantErrMsg == "" {
				if err != nil {
					t.Fatalf("应放行, 实际 %v", err)
				}
				return
			}
			ae := errs.AsAppError(err)
			if ae == nil || ae.Busi != errs.BusiServiceNotOpen || ae.Msg != c.wantErrMsg {
				t.Fatalf("应返回 1009 %q, 实际 %+v", c.wantErrMsg, ae)
			}
		})
	}
}
