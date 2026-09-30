package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestSelfDeployPromotionPolicyParsing(t *testing.T) {
	for _, parse := range []struct {
		name string
		fn   func([]byte) (*Config, error)
	}{{"parse", Parse}, {"strict", ParseStrict}} {
		for _, policy := range []string{"", "automatic", "explicit", " Explicit ", "typo"} {
			for _, enabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%q/enabled=%t", parse.name, policy, enabled), func(t *testing.T) {
					data := fmt.Sprintf("repo: owner/repo\nself_deploy:\n  enabled: %t\n  promotion_policy: %q\n", enabled, policy)
					cfg, err := parse.fn([]byte(data))
					if policy == "typo" {
						if err == nil || !strings.Contains(err.Error(), "self_deploy.promotion_policy") {
							t.Fatalf("invalid policy: cfg=%v err=%v", cfg, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					want := strings.ToLower(strings.TrimSpace(policy))
					if want == "" {
						want = SelfDeployPromotionAutomatic
					}
					if got := cfg.SelfDeploy.EffectivePromotionPolicy(); got != want {
						t.Fatalf("effective policy = %q, want %q", got, want)
					}
				})
			}
		}
	}
}
