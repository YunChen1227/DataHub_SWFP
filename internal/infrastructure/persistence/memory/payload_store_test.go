package memory

import (
	"context"
	"testing"

	"github.com/datahub/relay/internal/domain/model"
)

func TestAppendPayloadIdempotent(t *testing.T) {
	s := New()
	ctx := context.Background()
	first := &model.RequestPayload{RequestID: "r1", AppKey: "ak", Sources: []*model.UpstreamPayload{
		{RequestID: "r1", SourceLabel: "invoice2", Status: model.CallOK},
	}}
	if err := s.AppendPayload(ctx, first); err != nil {
		t.Fatal(err)
	}
	// 重放：请求行与已有源行不覆盖，只补新源行。
	again := &model.RequestPayload{RequestID: "r1", AppKey: "other", Sources: []*model.UpstreamPayload{
		{RequestID: "r1", SourceLabel: "invoice2", Status: model.CallError},
		{RequestID: "r1", SourceLabel: "tax2", Status: model.CallOK},
	}}
	if err := s.AppendPayload(ctx, again); err != nil {
		t.Fatal(err)
	}
	p := s.Payload("r1")
	if p == nil || p.AppKey != "ak" {
		t.Fatalf("请求行被覆盖: %+v", p)
	}
	if len(p.Sources) != 2 || p.Sources[0].Status != model.CallOK || p.Sources[1].SourceLabel != "tax2" {
		t.Fatalf("源行不符: %+v %+v", p.Sources[0], p.Sources)
	}
	if s.Payload("missing") != nil {
		t.Fatal("不存在的请求应返回 nil")
	}
}
