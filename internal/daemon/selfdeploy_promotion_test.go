package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/selfdeploy"
)

func TestRequestSelfDeployPromotionFencePrecedesDebounce(t *testing.T) {
	for _, policy := range []string{"explicit", "typo"} {
		for _, existing := range []bool{false, true} {
			t.Run(policy+map[bool]string{false: "/fresh", true: "/existing"}[existing], func(t *testing.T) {
				shared := filepath.Join(t.TempDir(), "shared")
				d := New(fakeLoader{}, Options{SelfDeployStateDir: shared})
				calls := 0
				d.selfDeployTrigger = func(*config.Config, int) error { calls++; return nil }
				cfg := selfDeployCfg(t, "owner/repo")
				cfg.SelfDeploy.PromotionPolicy = policy
				cfg.SelfDeploy.MinIntervalMinutes = 99
				if existing {
					d.selfDeployLast = time.Now().UTC()
					d.selfDeployLastPR = 8
					d.selfDeployWindow = time.Minute
					if err := selfdeploy.RecordTrigger(shared, 8, d.selfDeployLast); err != nil {
						t.Fatal(err)
					}
				}
				before, beforePR, beforeOK := selfdeploy.LastTrigger(shared)
				last, lastPR, window := d.selfDeployLast, d.selfDeployLastPR, d.selfDeployWindow
				err := d.RequestSelfDeploy(cfg, 1190)
				if err == nil || errors.Is(err, selfdeploy.ErrDebounced) {
					t.Fatalf("policy fence must precede debounce; got %v", err)
				}
				if policy == "explicit" && !errors.Is(err, selfdeploy.ErrExplicitPromotionRequired) {
					t.Fatalf("explicit error = %v", err)
				}
				if calls != 0 || d.selfDeployLast != last || d.selfDeployLastPR != lastPR || d.selfDeployWindow != window {
					t.Fatal("blocked request launched or mutated in-memory debounce state")
				}
				after, afterPR, afterOK := selfdeploy.LastTrigger(shared)
				if !after.Equal(before) || afterPR != beforePR || afterOK != beforeOK {
					t.Fatal("blocked request mutated shared trigger marker")
				}
				if !existing {
					if _, err := os.Stat(shared); !os.IsNotExist(err) {
						t.Fatalf("blocked request created shared state directory: %v", err)
					}
				}
				// A denied request must not suppress another automatic flow.
				cfg.SelfDeploy.PromotionPolicy = "automatic"
				cfg.SelfDeploy.MinIntervalMinutes = 1
				err = d.RequestSelfDeploy(cfg, 1191)
				if !existing && (err != nil || calls != 1) {
					t.Fatalf("next automatic request: calls=%d err=%v", calls, err)
				}
				if existing && (!errors.Is(err, selfdeploy.ErrDebounced) || calls != 0) {
					t.Fatalf("existing debounce not preserved: calls=%d err=%v", calls, err)
				}
			})
		}
	}
}
