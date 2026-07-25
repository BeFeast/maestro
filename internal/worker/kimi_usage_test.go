package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseKimiUsage_Fixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "kimi_stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	usage, ok := ParseKimiUsage(string(fixture))
	if !ok {
		t.Fatal("ParseKimiUsage returned ok=false")
	}
	if usage.Model != "kimi-k2.5" {
		t.Errorf("Model = %q, want kimi-k2.5", usage.Model)
	}
	if usage.Input != 2100 || usage.Output != 180 || usage.CacheRead != 350 || usage.CacheWrite != 20 {
		t.Fatalf("split usage = %+v, want input=2100 output=180 cache_read=350 cache_write=20", usage)
	}
	if usage.TotalTokens != 2650 {
		t.Fatalf("TotalTokens = %d, want 2650", usage.TotalTokens)
	}
	if usage.CostUSD != 0 {
		t.Fatalf("CostUSD = %v, want 0 for virtual pricing", usage.CostUSD)
	}
}

func TestParseKimiUsage_NoUsage(t *testing.T) {
	usage, ok := ParseKimiUsage("{\"role\":\"assistant\",\"content\":\"done\"}\nnot json\n")
	if ok || usage.TotalTokens != 0 {
		t.Fatalf("usage = %+v ok=%t, want no usage", usage, ok)
	}
}
