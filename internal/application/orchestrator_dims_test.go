package application

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/datahub/relay/internal/common/errs"
	"github.com/datahub/relay/internal/domain/auth"
	"github.com/datahub/relay/internal/domain/billing"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/quota"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
	"github.com/datahub/relay/internal/infrastructure/secret"
)

// countingUpstream 记录被调用次数，固定返回查得。
type countingUpstream struct{ calls atomic.Int64 }

func (u *countingUpstream) Query(_ context.Context, req *model.UpstreamRequest) (*model.UpstreamResult, error) {
	u.calls.Add(1)
	return &model.UpstreamResult{Code: "001", Msg: "成功", Reqid: req.Reqid, Got: req.Want}, nil
}

func (u *countingUpstream) Requery(_ context.Context, reqid string) (*model.RequeryResult, error) {
	return nil, nil
}

// TestHandleRejectsDimNotAllowed 锁定接口开关：请求维度超出客户已开通范围时返回
// 1009（head.errorCode 505007），且不调用上游、不计费。
func TestHandleRejectsDimNotAllowed(t *testing.T) {
	store := memory.New()
	store.SeedLicense(&model.LicenseView{
		LicenseID: "LIC-TAX", AppKey: "ak-tax", ClientUUID: "u-tax", Status: "ACTIVE",
		AllowTax: true,
	}, "sec-tax", "只开税商户", "13800000000")
	up := &countingUpstream{}
	orch := NewQueryOrchestrator("swfp",
		auth.New(store, secret.NewStore(store), auth.Md5Verifier{}),
		quota.New(store, store), billing.New(billing.DefaultTable()), up, store, nil)

	call := func(dataType string) *model.QueryResponse {
		params := map[string]string{"creditCode": "91110108551385082Q"}
		if dataType != "" {
			params["dataType"] = dataType
		}
		signed := &model.SignedRequest{AppKey: "ak-tax", Sign: auth.Sign(params, "sec-tax"), BodyParams: params}
		return orch.Handle(context.Background(), signed, &model.QueryCommand{CreditCode: params["creditCode"], DataType: dataType})
	}
	notOpen := errs.ErrorCode(errs.BusiServiceNotOpen)

	for _, dt := range []string{"both", "", "invoice"} {
		resp := call(dt)
		if resp.Head.ErrorCode != notOpen || resp.Head.ErrorMsg != "服务尚未开通：仅允许访问税务接口" {
			t.Fatalf("dataType=%q 应被拒绝, 实际 head=%+v", dt, resp.Head)
		}
	}
	if n := up.calls.Load(); n != 0 {
		t.Fatalf("被拒绝的请求不应调用上游, 实际调用 %d 次", n)
	}

	resp := call("tax")
	if resp.Head.ErrorCode != "0" {
		t.Fatalf("dataType=tax 应放行, 实际 head=%+v", resp.Head)
	}
	if n := up.calls.Load(); n != 1 {
		t.Fatalf("放行的请求应调用上游 1 次, 实际 %d 次", n)
	}
}
