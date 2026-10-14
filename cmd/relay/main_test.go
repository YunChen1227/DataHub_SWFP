package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/datahub/relay/internal/infrastructure/upstream"
)

func TestBuildUpstreamsDropsEntCreditPart1(t *testing.T) {
	ent := func(label, product string) upstreamConfig {
		return upstreamConfig{kind: upstream.ProviderEntCredit, label: label, product: product, baseURL: "http://example.invalid"}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	all := []upstreamConfig{
		ent("invoice1", "P0130081"),
		ent("invoice2", "P0130083"),
		ent("tax1", "P0130082"),
		ent("tax2", "P0130084"),
	}
	p, _, err := buildUpstreams("test", all, 0, nil, logger)
	if err != nil {
		t.Fatalf("buildUpstreams: %v", err)
	}
	if got := p.(*upstream.Sourcer).Active(); !strings.Contains(got, "2源/2调用") {
		t.Fatalf("part1 应被丢弃，只剩发票/税务 part2 两次调用, got %s", got)
	}

	onlyPart1 := []upstreamConfig{ent("invoice1", "P0130081"), ent("tax1", "P0130082")}
	p, _, err = buildUpstreams("test", onlyPart1, 0, nil, logger)
	if err != nil {
		t.Fatalf("buildUpstreams(only part1): %v", err)
	}
	if got := p.(*upstream.Sourcer).Active(); !strings.Contains(got, "2源/2调用") {
		t.Fatalf("只配 part1 时应改调 part2，仍是发票/税务两次调用, got %s", got)
	}
}
