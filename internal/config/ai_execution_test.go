package config

import (
	"strings"
	"testing"
)

func TestAIExecutionStrictConfig(t *testing.T) {
	base := "repo: fixture/repo\nai_execution:\n"
	for _, tail := range []string{
		"  require_verified_route: true\n",
		"  require_verified_route: true\n  manifest_path: /approved/proof.json\n  manifest_sha256: " + strings.Repeat("a", 64) + "\nreview_producer:\n  native_opus: true\n  opus_model: requested-model\n",
	} {
		cfg, err := ParseStrict([]byte(base + tail))
		if err != nil || !cfg.AIExecution.RequireVerifiedRoute {
			t.Fatalf("config=%+v error=%v", cfg, err)
		}
	}
	for _, tail := range []string{
		"  require_verified_routes: true\n",
		"  manifest_path: relative.json\n  manifest_sha256: " + strings.Repeat("a", 64) + "\n",
		"  manifest_path: /approved/proof.json\n",
		"  manifest_sha256: garbage\n",
	} {
		if _, err := ParseStrict([]byte(base + tail)); err == nil {
			t.Fatalf("accepted invalid policy: %s", tail)
		}
	}
}
