package application

import (
	"context"
	"testing"

	"github.com/datahub/relay/internal/common/errs"
	"github.com/datahub/relay/internal/domain/allowlist"
	"github.com/datahub/relay/internal/domain/auth"
	"github.com/datahub/relay/internal/domain/billing"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/quota"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
	"github.com/datahub/relay/internal/infrastructure/secret"
)

func TestHandleRejectsUpstreamAllowlist(t *testing.T) {
	store := memory.New()
	store.SeedLicense(&model.LicenseView{
		LicenseID: "LIC-AL", AppKey: "ak-al", ClientUUID: "u-al", Status: "ACTIVE",
		AllowInvoice: true, AllowTax: true,
	}, "sec-al", "白名单测试", "13800000001")
	up := &countingUpstream{}
	gate := allowlist.New(nil)
	if err := gate.LoadEntries([]*model.UpstreamAllowEntry{
		{CreditCode: "91110108551385082Q", Provider: "entcredit", Invoice: true},
	}); err != nil {
		t.Fatal(err)
	}
	_ = gate.SetEnabled(context.Background(), true)

	orch := NewQueryOrchestrator("swfp",
		auth.New(store, secret.NewStore(store), auth.Md5Verifier{}),
		quota.New(store, store), billing.New(billing.DefaultTable()), up, store, nil).
		WithUpstreamAllowGate(gate)

	call := func(code, dataType string) *model.QueryResponse {
		params := map[string]string{"creditCode": code, "dataType": dataType}
		signed := &model.SignedRequest{AppKey: "ak-al", Sign: auth.Sign(params, "sec-al"), BodyParams: params}
		return orch.Handle(context.Background(), signed, &model.QueryCommand{CreditCode: code, DataType: dataType})
	}
	notOpen := errs.ErrorCode(errs.BusiServiceNotOpen)

	resp := call("913101165647980623", "invoice")
	if resp.Head.ErrorCode != notOpen {
		t.Fatalf("不在名单应拒绝, head=%+v", resp.Head)
	}
	resp = call("91110108551385082Q", "tax")
	if resp.Head.ErrorCode != notOpen {
		t.Fatalf("维度不符应拒绝, head=%+v", resp.Head)
	}
	if n := up.calls.Load(); n != 0 {
		t.Fatalf("拒绝路径不应调上游, hits=%d", n)
	}

	resp = call("91110108551385082Q", "invoice")
	if resp.Head.ErrorCode != "0" {
		t.Fatalf("匹配应放行, head=%+v", resp.Head)
	}
	if n := up.calls.Load(); n != 1 {
		t.Fatalf("放行应调上游 1 次, hits=%d", n)
	}

	_ = gate.SetEnabled(context.Background(), false)
	resp = call("913101165647980623", "tax")
	if resp.Head.ErrorCode != "0" {
		t.Fatalf("关闭后应放行任意税号, head=%+v", resp.Head)
	}
}
