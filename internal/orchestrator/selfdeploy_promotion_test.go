package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/selfdeploy"
	"github.com/befeast/maestro/internal/state"
)

func TestSelfDeployPromotionFenceBlocksAutomaticPaths(t *testing.T) {
	for _, policy := range []string{"explicit", "typo"} {
		for _, path := range []string{"merge", "main-advance", "direct"} {
			t.Run(policy+"/"+path, func(t *testing.T) {
				stateDir := filepath.Join(t.TempDir(), "untouched")
				calls, lookups := 0, 0
				o := &Orchestrator{
					cfg: &config.Config{
						StateDir: stateDir,
						SelfDeploy: config.SelfDeployConfig{
							Enabled: true, PromotionPolicy: policy,
						},
					},
					notifier:          &notify.Notifier{},
					binaryVersion:     "1.4.2+gabc1234",
					selfDeployStartFn: func(int) error { calls++; return nil },
					mainHeadSHAFn: func() (string, error) {
						lookups++
						return "fed9876543210fed9876543210fed9876543210f", nil
					},
				}
				s := &state.State{}
				switch path {
				case "merge":
					o.maybeSelfDeployAfterMerge(s, 1190)
				case "main-advance":
					o.maybeSelfDeployOnMainAdvance(s)
				case "direct":
					err := o.triggerSelfDeploy(1190)
					if err == nil || (policy == "explicit" && !errors.Is(err, selfdeploy.ErrExplicitPromotionRequired)) {
						t.Fatalf("direct trigger error = %v", err)
					}
				}
				if calls != 0 || lookups != 0 {
					t.Fatalf("blocked policy reached launcher/head lookup: %d/%d", calls, lookups)
				}
				if !reflect.DeepEqual(s, &state.State{}) {
					t.Fatal("blocked trigger mutated project state")
				}
				if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
					t.Fatalf("blocked trigger created state directory: %v", err)
				}
			})
		}
	}
}

func TestSelfDeployAutomaticAndLegacyPoliciesLaunch(t *testing.T) {
	for _, policy := range []string{"", "automatic"} {
		for _, path := range []string{"merge", "main-advance"} {
			t.Run(policy+"/"+path, func(t *testing.T) {
				calls := 0
				o := &Orchestrator{
					cfg: &config.Config{
						StateDir: t.TempDir(),
						SelfDeploy: config.SelfDeployConfig{
							Enabled: true, PromotionPolicy: policy,
						},
					},
					notifier:          &notify.Notifier{},
					binaryVersion:     "1.4.2+gabc1234",
					selfDeployStartFn: func(int) error { calls++; return nil },
					mainHeadSHAFn: func() (string, error) {
						return "fed9876543210fed9876543210fed9876543210f", nil
					},
				}
				if path == "merge" {
					o.maybeSelfDeployAfterMerge(nil, 1190)
				} else {
					o.maybeSelfDeployOnMainAdvance(nil)
				}
				if calls != 1 {
					t.Fatalf("automatic launcher calls = %d, want 1", calls)
				}
				if _, _, ok := selfdeploy.LastTrigger(o.cfg.StateDir); !ok {
					t.Fatal("successful automatic trigger did not record debounce marker")
				}
			})
		}
	}
}
