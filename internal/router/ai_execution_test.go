package router

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
)

func TestStrictRouterLeafNeverExecutes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "called")
	cfg := &config.Config{AIExecution: aiexecution.Policy{RequireVerifiedRoute: true}, Routing: config.RoutingConfig{RouterModel: "fixture"}, Model: config.ModelConfig{Backends: map[string]config.BackendDef{"fixture": {Cmd: "touch " + marker}}}}
	r := &Router{cfg: cfg}
	_, err := r.callModel("prompt")
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("strict router executed")
	}
}
