package main

import (
	"reflect"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

func TestNightWorkerChainUsesResolvedProviderRoute(t *testing.T) {
	cfg := &config.Config{Model: config.ModelConfig{
		Default: "legacy",
		ProviderLanes: []config.ProviderLane{
			{Provider: "anthropic", Default: "claude"},
			{Provider: "openai", Default: "sol", FallbackBackends: []string{"gpt55"}},
		},
	}}
	if got := nightWorkerChain(cfg); !reflect.DeepEqual(got, []string{"claude", "sol", "gpt55"}) {
		t.Fatalf("night worker chain = %v", got)
	}
}
