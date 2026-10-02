package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

const auxiliaryValidProjectYAML = `repo: owner/svc
state_dir: %q
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
`

// auxiliaryBrokenProjectYAML parses on its own; it stops loading once the
// shared backend its routing tier points at is disabled, which is the row
// shape that aborted a live reconcile of every other project.
const auxiliaryBrokenProjectYAML = `repo: owner/broken
state_dir: %q
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
    kimi:
      cmd: kimi
routing:
  tiers:
    cheap:
      backend: kimi
`

// writeSettledAuxiliaryReceipt indexes stateDir as a receipt root that holds
// one settled supervisor consultation: no launch marker, no native session, so
// the replay path reports it clear.
func writeSettledAuxiliaryReceipt(t *testing.T, stateDir string) {
	t.Helper()
	receipts := filepath.Join(stateDir, "supervisor-consultations")
	if err := os.MkdirAll(receipts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settled, err := json.Marshal(supervisor.ConsultationReceipt{
		SchemaVersion: 1,
		Identity:      supervisor.ConsultationIdentity{ID: uuid.NewString(), ProjectID: "svc", Role: "supervisor"},
		Status:        "succeeded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(receipts, "current.json"), settled, 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedAuxiliaryReconcileStore writes one loadable project row owning stateDir
// and, when broken is set, one row that fails to load, then disables the shared
// backend the broken row's tier references. Both rows pass UpsertProject's
// strict parse; only the later backend change makes the second one invalid.
func seedAuxiliaryReconcileStore(t *testing.T, dbPath, stateDir string, valid, broken bool) {
	t.Helper()
	store, err := configstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if valid {
		if err := store.UpsertProject(ctx, "svc", fmt.Sprintf(auxiliaryValidProjectYAML, stateDir)); err != nil {
			t.Fatalf("seed svc: %v", err)
		}
	}
	if broken {
		brokenState := filepath.Join(filepath.Dir(stateDir), "broken-state")
		if err := store.UpsertProject(ctx, "broken", fmt.Sprintf(auxiliaryBrokenProjectYAML, brokenState)); err != nil {
			t.Fatalf("seed broken: %v", err)
		}
		if err := store.UpsertBackend(ctx, "kimi", "cmd: kimi\nenabled: false\n"); err != nil {
			t.Fatalf("disable kimi: %v", err)
		}
		if _, err := store.Load(ctx, "broken"); err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Fatalf("broken row must fail to load on the disabled backend, got %v", err)
		}
	}
	if err := store.RememberAuxiliaryStateDir(ctx, stateDir); err != nil {
		t.Fatal(err)
	}
}

func TestAuxiliaryReconcileSkipsInvalidProjectRow(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// --db is explicit, but the flag default still inspects the stores under
	// HOME; pin it so the test never reads a real operator store.
	t.Setenv("HOME", dir)
	dbPath := filepath.Join(dir, "config.db")
	stateDir := filepath.Join(dir, "svc-state")
	writeSettledAuxiliaryReceipt(t, stateDir)
	seedAuxiliaryReconcileStore(t, dbPath, stateDir, true, true)

	wantStdout := "root=" + stateDir + " role=supervisor result=clear\nroots=1 held=0\n"
	for name, extra := range map[string][]string{"all": nil, "root": {"--root", stateDir}} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"reconcile", "--db", dbPath}, extra...)
			if code := runAuxiliaryReconcile(args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stdout.String() != wantStdout {
				t.Fatalf("stdout=%q want %q", stdout.String(), wantStdout)
			}
			lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "[maestro] auxiliary reconcile: skipping project broken: ") || !strings.Contains(lines[0], "disabled") {
				t.Fatalf("stderr=%q want one skip warning naming the broken row", stderr.String())
			}
		})
	}
}

func TestAuxiliaryReconcileFailsWhenNoProjectRowLoads(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// --db is explicit, but the flag default still inspects the stores under
	// HOME; pin it so the test never reads a real operator store.
	t.Setenv("HOME", dir)
	dbPath := filepath.Join(dir, "config.db")
	stateDir := filepath.Join(dir, "orphan-state")
	writeSettledAuxiliaryReceipt(t, stateDir)
	seedAuxiliaryReconcileStore(t, dbPath, stateDir, false, true)

	var stdout, stderr bytes.Buffer
	if code := runAuxiliaryReconcile([]string{"reconcile", "--db", dbPath}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want 1 when every row was skipped", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "skipping project broken") || !strings.Contains(stderr.String(), "all 1 project rows were skipped") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	// --root names one indexed root; it is replayed from its receipt even when
	// no project row loads, exactly like the root of a removed project.
	stdout.Reset()
	stderr.Reset()
	if code := runAuxiliaryReconcile([]string{"reconcile", "--db", dbPath, "--root", stateDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if want := "root=" + stateDir + " role=supervisor result=clear\nroots=1 held=0\n"; stdout.String() != want {
		t.Fatalf("stdout=%q want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "skipping project broken") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestAuxiliaryReconcileSkipWarningIsOneLine(t *testing.T) {
	err := fmt.Errorf("config: %s", "yaml: unmarshal errors:\n  line 3: cannot unmarshal !!str `x` into int\n  line 5: cannot unmarshal !!seq into string\n")
	want := "config: yaml: unmarshal errors: line 3: cannot unmarshal !!str `x` into int line 5: cannot unmarshal !!seq into string"
	if got := singleLineError(err); got != want {
		t.Fatalf("singleLineError = %q, want %q", got, want)
	}
}
