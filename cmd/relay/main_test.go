package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/datahub/relay/internal/infrastructure/upstream"
)

func TestBuildUpstreamsDropsEntCreditPart2(t *testing.T) {
	ent := func(label, product string) upstreamConfig {
		return upstreamConfig{kind: upstream.ProviderEntCredit, label: label, product: product, baseURL: "http://example.invalid"}
	}
	ucs := []upstreamConfig{
		ent("invoice1", "P0130081"),
		ent("invoice2", "P0130083"),
		ent("tax1", "P0130082"),
		ent("tax2", "P0130084"),
	}
	p, _, err := buildUpstreams("test", ucs, 0, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildUpstreams: %v", err)
	}
	s, ok := p.(*upstream.Sourcer)
	if !ok {
		t.Fatalf("want *upstream.Sourcer, got %T", p)
	}
	if got := s.Active(); !strings.Contains(got, "2源/2调用") {
		t.Fatalf("part2 应被丢弃，只剩发票/税务 part1 两次调用, got %s", got)
	}
}
