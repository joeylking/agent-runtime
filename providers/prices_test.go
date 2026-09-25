package providers_test

import (
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
)

func TestPriceFor_UnpricedModelRefused(t *testing.T) {
	table := agentrt.PriceTable{"anthropic:claude-sonnet-5": {InputPerMTok: 2_000_000, OutputPerMTok: 10_000_000}}
	p, err := providers.PriceFor(table, "anthropic:claude-sonnet-5")
	if err != nil || p.OutputPerMTok != 10_000_000 {
		t.Fatalf("price = %+v, err = %v", p, err)
	}
	_, err = providers.PriceFor(table, "anthropic:claude-opus-5")
	if err == nil || !strings.Contains(err.Error(), "a paid model runs only with a known price") {
		t.Fatalf("err = %v", err)
	}
}

func TestFree_DistinguishesFreeFromUnpriced(t *testing.T) {
	table := providers.Merge(
		providers.Free("ollama:qwen3:30b-a3b"),
		agentrt.PriceTable{"anthropic:claude-opus-5": {InputPerMTok: 5_000_000}},
	)
	p, err := providers.PriceFor(table, "ollama:qwen3:30b-a3b")
	if err != nil || p != (agentrt.Price{}) {
		t.Fatalf("a model marked free must look up as costing nothing: %+v, %v", p, err)
	}
	if _, err := providers.PriceFor(table, "ollama:other"); err == nil {
		t.Fatal("an unlisted model must stay unpriced")
	}
	if _, err := providers.PriceFor(table, "anthropic:claude-opus-5"); err != nil {
		t.Fatalf("merged paid price lost: %v", err)
	}
}

func TestMerge_LaterTableWins(t *testing.T) {
	got := providers.Merge(
		agentrt.PriceTable{"m": {InputPerMTok: 1}},
		agentrt.PriceTable{"m": {InputPerMTok: 2}},
	)
	if got["m"].InputPerMTok != 2 {
		t.Fatalf("merged = %+v", got)
	}
}

func TestParseDollars_AcceptedForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want agentrt.Micros
	}{
		{"5", 5_000_000},
		{"4.41", 4_410_000},
		{"$0.50", 500_000},
		{" $2 ", 2_000_000},
		{"0", 0},
		{"0.000001", 1},
	} {
		got, err := providers.ParseDollars(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("ParseDollars(%q) = %d, %v, want %d", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "-1", "$-0.5", "free", "1,50", "NaN", "Inf"} {
		if got, err := providers.ParseDollars(in); err == nil {
			t.Fatalf("ParseDollars(%q) = %d, want an error", in, got)
		}
	}
}
