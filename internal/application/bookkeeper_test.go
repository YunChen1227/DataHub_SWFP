package application

import (
	"context"
	"testing"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/quota"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
)

// seedBegin 在 memory store 里播种 license 并开一条 PENDING 台账，返回结算 token。
func seedBegin(t *testing.T, store *memory.Store, q *quota.Service, reqid string) *quota.ReserveToken {
	t.Helper()
	lic := &model.LicenseView{LicenseID: "LIC-T1", AppKey: "ak-test", ClientUUID: "u1", Status: "ACTIVE"}
	store.SeedLicense(lic, "sec", "测试商户", "13800000000")
	tok, existing, err := q.Begin(context.Background(), lic, "swfp", reqid, "", "req-"+reqid, true)
	if err != nil || existing != nil || tok == nil {
		t.Fatalf("Begin: tok=%v existing=%v err=%v", tok, existing, err)
	}
	return tok
}

// TestBookkeeperSettlesAndAudits 锁定异步记账的核心行为：入队的结算工作单在
// Close(drain) 后必须完成台账 BILLED + 成功查得数/调用次数累计 + 审计落库。
func TestBookkeeperSettlesAndAudits(t *testing.T) {
	store := memory.New()
	q := quota.New(store, store)
	tok := seedBegin(t, store, q, "r1")

	b := NewBookkeeper(q, store, 8, 1, nil)
	dec := &model.BillingDecision{Resolved: true, Returned: true, Result: &model.UpstreamResult{Code: "001"}}
	b.Submit(bookTask{token: tok, decision: dec, rec: &model.AuditRecord{RequestID: "req-r1", Version: "swfp", AppKey: "ak-test"}})
	b.Close() // drain

	l, err := store.FindByReqid(context.Background(), "ak-test", "swfp", "r1")
	if err != nil || l == nil {
		t.Fatalf("FindByReqid: %v %v", l, err)
	}
	if l.State != model.StateBilled || !l.CountedService {
		t.Fatalf("台账未结算: state=%s counted=%v", l.State, l.CountedService)
	}
	if used, _ := store.ServiceUsed(context.Background(), "LIC-T1", "swfp"); used != 1 {
		t.Fatalf("成功查得数=%d, want 1", used)
	}
	if calls, _ := store.TotalCalls(context.Background(), "LIC-T1", "swfp"); calls != 1 {
		t.Fatalf("调用次数=%d, want 1", calls)
	}
	audits, _ := store.ListAudits(context.Background(), model.AuditFilter{Version: "swfp", Limit: 10})
	if len(audits) != 1 || audits[0].RequestID != "req-r1" {
		t.Fatalf("审计未落库: %+v", audits)
	}
}

// TestBookkeeperSubmitAfterClose 关闭后 Submit 必须降级为同步执行（不 panic、不丢）。
func TestBookkeeperSubmitAfterClose(t *testing.T) {
	store := memory.New()
	q := quota.New(store, store)
	tok := seedBegin(t, store, q, "r2")

	b := NewBookkeeper(q, store, 8, 1, nil)
	b.Close()
	dec := &model.BillingDecision{Resolved: true, Returned: false, Result: &model.UpstreamResult{Code: "999"}}
	b.Submit(bookTask{token: tok, decision: dec, rec: &model.AuditRecord{RequestID: "req-r2", Version: "swfp", AppKey: "ak-test"}})

	l, _ := store.FindByReqid(context.Background(), "ak-test", "swfp", "r2")
	if l == nil || l.State != model.StateBilled || l.CountedService {
		t.Fatalf("关闭后同步降级未生效: %+v", l)
	}
	audits, _ := store.ListAudits(context.Background(), model.AuditFilter{Version: "swfp", Limit: 10})
	if len(audits) != 1 {
		t.Fatalf("审计条数=%d, want 1", len(audits))
	}
}

// TestBookkeeperAuditOnlyTask 无结算单（鉴权失败/PENDING 场景）只写审计。
func TestBookkeeperAuditOnlyTask(t *testing.T) {
	store := memory.New()
	b := NewBookkeeper(nil, store, 8, 1, nil)
	b.Submit(bookTask{rec: &model.AuditRecord{RequestID: "req-r3", Version: "swfp"}})
	b.Close()
	audits, _ := store.ListAudits(context.Background(), model.AuditFilter{Version: "swfp", Limit: 10})
	if len(audits) != 1 || audits[0].RequestID != "req-r3" {
		t.Fatalf("审计未落库: %+v", audits)
	}
}
