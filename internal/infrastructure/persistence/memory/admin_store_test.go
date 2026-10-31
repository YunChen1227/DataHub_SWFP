package memory

import (
	"context"
	"strconv"
	"testing"

	"github.com/datahub/relay/internal/domain/model"
)

func TestAuditFilterAndCount(t *testing.T) {
	ctx := context.Background()
	s := New()
	for i := 1; i <= 5; i++ {
		code := "91310000MA1FL0Q40X"
		if i%2 == 0 {
			code = "91440300MA5DC1234Y"
		}
		n := strconv.Itoa(i)
		if err := s.AppendAudit(ctx, &model.AuditRecord{
			RequestID: "req-" + n, Reqid: "r" + n, Version: "swfp", AppKey: "ak", CreditCode: code,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.AppendAudit(ctx, &model.AuditRecord{RequestID: "req-x", Reqid: "r1", Version: "other"})

	cases := []struct {
		name string
		f    model.AuditFilter
		want int64
	}{
		{"all in route", model.AuditFilter{Version: "swfp"}, 5},
		{"by reqid", model.AuditFilter{Version: "swfp", Reqid: "r3"}, 1},
		{"by requestId", model.AuditFilter{Version: "swfp", Reqid: "req-4"}, 1},
		{"by credit code", model.AuditFilter{Version: "swfp", CreditCode: "91440300MA5DC1234Y"}, 2},
		{"no match", model.AuditFilter{Version: "swfp", Reqid: "nope"}, 0},
	}
	for _, c := range cases {
		got, err := s.CountAudits(ctx, c.f)
		if err != nil || got != c.want {
			t.Errorf("%s: CountAudits = %d, %v; want %d", c.name, got, err, c.want)
		}
	}

	page, _ := s.ListAudits(ctx, model.AuditFilter{Version: "swfp", Limit: 2, Offset: 2})
	if len(page) != 2 || page[0].RequestID != "req-3" || page[1].RequestID != "req-2" {
		t.Fatalf("second page = %+v", page)
	}
	total, _ := s.CountAudits(ctx, model.AuditFilter{Version: "swfp", Limit: 2, Offset: 2})
	if total != 5 {
		t.Fatalf("CountAudits must ignore Limit/Offset, got %d", total)
	}
}
