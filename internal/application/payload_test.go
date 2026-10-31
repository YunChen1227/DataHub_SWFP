package application

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
)

var payloadAt = time.Date(2026, 10, 15, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))

// payloadTask 构造一次票税请求：invoice2 查得但近 12 月开票为 0，tax2 查得且有税，
// sales 查无（无数据，不留源行），tax1 跳过。
func payloadTask(requestID string) bookTask {
	raw := `{"invoice2":{"status":"ok","data":{"nsrfpxx":{"kphzxxList":[{"ssyf":"2026-05","kpje":"0"}]}}},
	         "tax2":{"status":"ok","data":{"nsrswxx":{"sbsjList":[{"sssjz":"2026-05-31","ynse":"5"}]}}},
	         "sales":{"status":"empty"}}`
	return bookTask{
		rec: &model.AuditRecord{RequestID: requestID, Version: "swfp", AppKey: "ak-test", Reqid: "r-" + requestID,
			CreditCode: "91110000TEST", CreatedAt: payloadAt},
		cmd: &model.QueryCommand{CreditCode: "91110000TEST", DataType: "3"},
		decision: &model.BillingDecision{Result: &model.UpstreamResult{
			Code: "001", Range: `{"发票数据聚合":{"nsrjbxx":{"nsrmc":"测试企业"}}}`, RawRange: raw,
		}},
		sources: []model.SourceCall{
			{Label: "invoice2", Source: "源2", Provider: "entcredit", Status: model.CallOK, Dims: model.DimSet{Invoice: true}},
			{Label: "tax2", Source: "源4", Provider: "entcredit", Status: model.CallOK, Dims: model.DimSet{Tax: true}},
			{Label: "sales", Source: "源5", Provider: "salesdata", Status: model.CallEmpty, Dims: model.DimSet{Invoice: true}},
			{Label: "tax1", Source: "源3", Provider: "entcredit", Status: model.CallSkipped},
		},
	}
}

func TestBookTaskPayload(t *testing.T) {
	p := payloadTask("req-p1").payload()
	if p == nil {
		t.Fatal("payload 为 nil")
	}
	if p.RequestID != "req-p1" || p.CreditCode != "91110000TEST" || !p.CreatedAt.Equal(payloadAt) {
		t.Fatalf("请求行字段不符: %+v", p)
	}
	var body model.QueryCommand
	if err := json.Unmarshal(p.RequestBody, &body); err != nil || body.DataType != "3" {
		t.Fatalf("request_body 未留存入参: %s %v", p.RequestBody, err)
	}
	if !strings.Contains(string(p.ResponseRange), "测试企业") {
		t.Fatalf("response_range 未留存: %s", p.ResponseRange)
	}
	if len(p.Sources) != 2 || p.Sources[0].SourceLabel != "invoice2" || p.Sources[1].SourceLabel != "tax2" {
		t.Fatalf("源行应只有查得的 invoice2/tax2, got %d", len(p.Sources))
	}
	if !strings.Contains(string(p.Sources[0].RawData), "kphzxxList") {
		t.Fatalf("raw_data 未留存: %s", p.Sources[0].RawData)
	}
	if p.InvZero == nil || !*p.InvZero || p.TaxZero == nil || *p.TaxZero {
		t.Fatalf("请求级判定应为发票为0、税务非0: inv=%v tax=%v", p.InvZero, p.TaxZero)
	}
	if p.TaxAmt12m == nil || *p.TaxAmt12m != "5.00" {
		t.Fatalf("税务金额应为 5.00: %v", p.TaxAmt12m)
	}
	if s := p.Sources[0]; s.InvZero == nil || !*s.InvZero || s.TaxZero != nil {
		t.Fatalf("invoice2 源级判定不符: %+v", s.ZeroVerdict)
	}

	if (bookTask{}).payload() != nil {
		t.Fatal("无审计记录时不应生成 payload")
	}

	// 鉴权/参数失败等无上游结果的请求：只留请求行。
	bare := bookTask{rec: &model.AuditRecord{RequestID: "req-p2"}}
	if bp := bare.payload(); bp == nil || len(bp.Sources) != 0 || bp.ResponseRange != nil {
		t.Fatalf("无上游结果时应只有请求行: %+v", bp)
	}

	// 非法 JSON 的 range 存为 JSON 字符串，不丢。
	bad := payloadTask("req-p3")
	bad.decision.Result.Range = "not-json"
	if bp := bad.payload(); string(bp.ResponseRange) != `"not-json"` {
		t.Fatalf("非法 JSON 应存为字符串: %s", bp.ResponseRange)
	}
}

type failingPayloads struct{}

func (failingPayloads) AppendPayload(context.Context, *model.RequestPayload) error {
	return errors.New("db down")
}

func TestBookkeeperStoresPayload(t *testing.T) {
	store := memory.New()
	b := NewBookkeeper(nil, store, 8, 1, nil).WithPayloads(store, nil)
	b.Submit(payloadTask("req-b1"))
	b.Close()
	if p := store.Payload("req-b1"); p == nil || len(p.Sources) != 2 {
		t.Fatalf("payload 未落库: %+v", p)
	}

	// 写库失败落 spool，库恢复后重放补齐。
	dir := t.TempDir()
	spool := NewPayloadSpool(dir, nil)
	b2 := NewBookkeeper(nil, store, 8, 1, nil).WithPayloads(failingPayloads{}, spool)
	b2.Submit(payloadTask("req-b2"))
	b2.Close()
	if store.Payload("req-b2") != nil {
		t.Fatal("写库失败时不应已落库")
	}
	n, err := spool.Replay(context.Background(), store)
	if err != nil || n != 1 {
		t.Fatalf("Replay: n=%d err=%v", n, err)
	}
	if p := store.Payload("req-b2"); p == nil || len(p.Sources) != 2 || p.InvZero == nil {
		t.Fatalf("重放后数据不完整: %+v", p)
	}
}

func TestPayloadSpoolReplay(t *testing.T) {
	dir := t.TempDir()
	spool := NewPayloadSpool(dir, nil)
	for _, id := range []string{"req-s1", "req-s2"} {
		if err := spool.Put(payloadTask(id).payload()); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// 崩溃时写了一半的行。
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("应有 1 个 spool 文件, got %v", files)
	}
	f, _ := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"requestId":"req-half` + "\n")
	f.Close()

	ctx := context.Background()
	// 库仍不可用：记录写回 spool，不丢。
	n, err := spool.Replay(ctx, failingPayloads{})
	if err != nil || n != 0 {
		t.Fatalf("Replay(失败库): n=%d err=%v", n, err)
	}
	if bad, _ := os.ReadFile(filepath.Join(dir, "unparsable.bad")); !strings.Contains(string(bad), "req-half") {
		t.Fatalf("坏行应保留在 unparsable.bad: %q", bad)
	}

	store := memory.New()
	n, err = spool.Replay(ctx, store)
	if err != nil || n != 2 {
		t.Fatalf("Replay: n=%d err=%v", n, err)
	}
	if store.Payload("req-s1") == nil || store.Payload("req-s2") == nil {
		t.Fatal("重放后缺数据")
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.json*"))
	replays, _ := filepath.Glob(filepath.Join(dir, "*.replay"))
	if len(left)+len(replays) != 0 {
		t.Fatalf("重放成功后不应残留文件: %v %v", left, replays)
	}

	// 重复重放无害（幂等）。
	if err := spool.Put(payloadTask("req-s1").payload()); err != nil {
		t.Fatal(err)
	}
	if n, err := spool.Replay(ctx, store); err != nil || n != 1 {
		t.Fatalf("重复重放: n=%d err=%v", n, err)
	}
	if p := store.Payload("req-s1"); len(p.Sources) != 2 {
		t.Fatalf("重复重放后源行重复: %d", len(p.Sources))
	}
}
