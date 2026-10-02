package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/state"
)

type fakeLaneReadiness struct {
	err   error
	calls int
}

func (f *fakeLaneReadiness) ObserveLaneReadiness(aiexecution.Policy) error {
	f.calls++
	return f.err
}

func expectDeferredLaneHold(t *testing.T, err error, code string) {
	t.Helper()
	h, ok := NativeHold(err)
	if !ok || !h.Deferred || h.Code != code || h.LaunchUncertain {
		t.Fatalf("hold=%+v err=%v, want deferred %s", h, err, code)
	}
}

func sessionJSON(t *testing.T, sess *state.Session) string {
	t.Helper()
	b, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A managed lane that is not ready pauses every native entrypoint before its
// registration: no receipt, authority registration, generation, process stop
// or persisted hold exists, and the same call succeeds once the lane is ready.
func TestNativeLaneNotReadyDefersEveryEntrypointBeforeRegistration(t *testing.T) {
	for _, entry := range []string{"fresh", "respawn", "in_place", "phase"} {
		t.Run(entry, func(t *testing.T) {
			f := nativeTestFixture(t)
			lane := &fakeLaneReadiness{}
			f.cfg.RuntimeNativeLaneReadiness = lane
			if entry != "fresh" {
				if _, err := f.start(); err != nil {
					t.Fatal(err)
				}
				f.cfg.WorkerLaunchContext = nil
			}
			sess := f.st.Sessions[f.slot]
			run := func() error {
				var err error
				switch entry {
				case "fresh":
					_, err = f.start()
				case "respawn":
					err = Respawn(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "prompt", "claude")
				case "in_place":
					err = RespawnInPlace(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "prompt", "claude")
				case "phase":
					sess.Phase = state.PhaseAdvisor
					err = StartPhase(f.cfg, sess, f.slot, "prompt", "claude")
				}
				return err
			}
			lane.err = aiexecution.Held("binding_route_unserved")
			registered, spawned, statuses := len(f.registered), f.spawned, len(f.statuses)
			var before string
			if sess != nil {
				before = sessionJSON(t, sess)
			}
			expectDeferredLaneHold(t, run(), "binding_route_unserved")
			if len(f.registered) != registered || f.spawned != spawned || f.stopped != 0 || len(f.statuses) != statuses {
				t.Fatalf("deferred lane registered=%d spawned=%d stopped=%d receipts=%v", len(f.registered)-registered, f.spawned-spawned, f.stopped, f.statuses[statuses:])
			}
			if entry == "fresh" {
				if f.st.Sessions[f.slot] != nil {
					t.Fatal("deferred fresh start created a held session")
				}
				if _, err := os.Stat(filepath.Join(nativeReceiptDir(f.cfg.StateDir, f.slot), nativeReceiptName(1))); !os.IsNotExist(err) {
					t.Fatalf("deferred fresh start wrote a receipt: %v", err)
				}
			} else {
				if sess.NativeRegistrationHold != "" || sess.NativeLaneDeferred != "binding_route_unserved" {
					t.Fatalf("hold=%q deferred=%q", sess.NativeRegistrationHold, sess.NativeLaneDeferred)
				}
				sess.NativeLaneDeferred = ""
				if entry != "phase" && sessionJSON(t, sess) != before {
					t.Fatal("deferred respawn changed the persisted projection")
				}
				if _, err := os.Stat(filepath.Join(nativeReceiptDir(f.cfg.StateDir, f.slot), nativeReceiptName(2))); !os.IsNotExist(err) {
					t.Fatalf("deferred respawn wrote a successor receipt: %v", err)
				}
			}
			lane.err = nil
			if err := run(); err != nil {
				t.Fatalf("ready lane: %v", err)
			}
			if len(f.registered) != registered+1 || f.spawned != spawned+1 {
				t.Fatalf("ready lane registered=%d spawned=%d", len(f.registered)-registered, f.spawned-spawned)
			}
		})
	}
}

// Any probe failure, typed or not, is a deferred pause; an untyped one is
// reported as unobservable instead of ready.
func TestNativeLaneUntypedReadinessFailureIsUnobservable(t *testing.T) {
	f := nativeTestFixture(t)
	f.cfg.RuntimeNativeLaneReadiness = &fakeLaneReadiness{err: os.ErrDeadlineExceeded}
	_, err := f.start()
	expectDeferredLaneHold(t, err, "binding_readiness_unobservable")
	if len(f.registered) != 0 {
		t.Fatal("registered without an observed lane")
	}
}

// A first-generation registration stopped by a binding hold before launch
// intent is the only shape that may be resumed automatically, and only
// through the exact operator recovery path.
func TestNativeLaneHeldPrelaunchSelectsOnlyRegisteredBindingHolds(t *testing.T) {
	cases := map[string]func(*NativeWorkerReceipt, *state.Session) bool{
		"binding hold": func(*NativeWorkerReceipt, *state.Session) bool { return true },
		"other hold": func(_ *NativeWorkerReceipt, s *state.Session) bool {
			s.NativeRegistrationHold = "setup_failed"
			return false
		},
		"launch intent":   func(r *NativeWorkerReceipt, _ *state.Session) bool { r.Status = "launch_intent"; return false },
		"log written":     func(r *NativeWorkerReceipt, _ *state.Session) bool { r.LogFile = "/fixture.log"; return false },
		"acknowledgement": func(r *NativeWorkerReceipt, _ *state.Session) bool { r.Acknowledgement = nil; return false },
		"expiring": func(r *NativeWorkerReceipt, _ *state.Session) bool {
			r.Request.ExpiresAt = time.Now().Unix() + 30
			return false
		},
		"resumed too often": func(r *NativeWorkerReceipt, _ *state.Session) bool {
			r.PrelaunchRecoveries = make([]NativePrelaunchRecoveryRecord, 3)
			return false
		},
		"successor":          func(_ *NativeWorkerReceipt, s *state.Session) bool { s.WorkerGeneration = 1; return false },
		"running projection": func(_ *NativeWorkerReceipt, s *state.Session) bool { s.Status = state.StatusRunning; return false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, r, calls := registeredPrelaunchFixture(t)
			sess := f.st.Sessions[f.slot]
			sess.NativeRegistrationHold = "binding_route_unserved"
			want := mutate(r, sess)
			if err := writeNativeWorkerReceipt(nativeReceiptDir(f.cfg.StateDir, f.slot), r); err != nil {
				t.Fatal(err)
			}
			id, ok := NativeLaneHeldPrelaunch(f.cfg, f.slot, sess)
			if ok != want || ok && id != r.Request.NativeSessionID {
				t.Fatalf("selected=%v id=%q", ok, id)
			}
			if !want {
				return
			}
			got, err := RecoverRegisteredWorkerStart(f.cfg, f.st, f.cfg.Repo, f.issue, "fixture task", f.slot, id)
			if err != nil || got != f.slot || f.spawned != 1 || *calls != 1 {
				t.Fatalf("resume slot=%s error=%v spawned=%d registrations=%d", got, err, f.spawned, *calls)
			}
			if _, again := NativeLaneHeldPrelaunch(f.cfg, f.slot, f.st.Sessions[f.slot]); again {
				t.Fatal("a resumed generation is still selectable")
			}
		})
	}
}
