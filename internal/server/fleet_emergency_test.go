package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/emergencystore"
	"github.com/befeast/maestro/internal/notify"
)

// fakeEmergencySwitch is an in-memory EmergencySwitch for the endpoint tests.
type fakeEmergencySwitch struct {
	st emergencystore.State
}

func (f *fakeEmergencySwitch) Set(_ context.Context, level emergencystore.Level, actor, reason string, at time.Time) error {
	f.st = emergencystore.State{Level: level, Actor: actor, Reason: reason, Since: at.UTC()}
	return nil
}

func (f *fakeEmergencySwitch) Resume(_ context.Context, actor string, _ time.Time) error {
	f.st = emergencystore.State{Level: emergencystore.LevelNone, Actor: actor}
	return nil
}

func (f *fakeEmergencySwitch) Get(_ context.Context) (emergencystore.State, error) {
	return f.st, nil
}

type fakeEmergencyNotifier struct {
	msgs   []string
	alerts []notify.AlertClass
}

func (f *fakeEmergencyNotifier) Sendf(format string, args ...any) {
	f.msgs = append(f.msgs, format)
	if len(args) == 1 {
		if s, ok := args[0].(string); ok {
			f.msgs[len(f.msgs)-1] = s
		}
	}
}

func (f *fakeEmergencyNotifier) Alert(class notify.AlertClass, _, _, _ string) error {
	f.alerts = append(f.alerts, class)
	return nil
}

// TestFleetAPIExposesEmergencyBlock pins #840: GET /api/v1/fleet always carries
// an `emergency` object; when the switch is llm_stopped the fleet API reports
// `emergency: llm_stopped` with active=true (the acceptance criterion), and when
// unset it reports level "none" with active=false.
func TestFleetAPIExposesEmergencyBlock(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)

	// Default (no source wired): explicit inactive object.
	var resp fleetResponse
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Emergency.Active || resp.Emergency.Level != "none" {
		t.Fatalf("default emergency = %+v, want active=false level=none", resp.Emergency)
	}
	// The wire format must always carry the object so the SPA never reads undefined.
	if !strings.Contains(w.Body.String(), `"emergency":`) {
		t.Fatal(`fleet response missing "emergency" key`)
	}

	// Switch engaged → llm_stopped, active.
	since := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	srv.SetEmergencySource(func() emergencystore.State {
		return emergencystore.State{Level: emergencystore.LevelLLMStopped, Since: since, Actor: "oleg", Reason: "burn"}
	})
	w = httptest.NewRecorder()
	srv.handleFleet(w, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	resp = fleetResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Emergency.Active {
		t.Fatal("emergency active=false after engaging llm_stopped")
	}
	if resp.Emergency.Level != string(emergencystore.LevelLLMStopped) {
		t.Fatalf("emergency level = %q, want llm_stopped", resp.Emergency.Level)
	}
	if resp.Emergency.Actor != "oleg" || resp.Emergency.Reason != "burn" {
		t.Fatalf("emergency actor/reason = %q/%q", resp.Emergency.Actor, resp.Emergency.Reason)
	}
	if resp.Emergency.Since == "" {
		t.Fatal("emergency since empty while active")
	}
}

// TestHandleFleetEmergency_StopAndResume drives the red-button endpoint: a POST
// engages the switch, GET reflects it, and a resume clears it.
func TestHandleFleetEmergency_StopAndResume(t *testing.T) {
	sw := &fakeEmergencySwitch{}
	notifier := &fakeEmergencyNotifier{}
	srv := NewFleet(nil, "127.0.0.1", 8786, true) // read-only: emergency must still work
	srv.SetEmergencyStore(sw)
	srv.SetEmergencyNotifier(notifier)

	// Engage llm_stopped.
	body := strings.NewReader(`{"level":"llm_stopped","actor":"oleg","reason":"runaway burn"}`)
	w := httptest.NewRecorder()
	srv.handleFleetEmergency(w, httptest.NewRequest(http.MethodPost, "/api/v1/fleet/emergency", body))
	if w.Code != http.StatusOK {
		t.Fatalf("stop status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if len(notifier.msgs) != 1 || !strings.Contains(notifier.msgs[0], "EMERGENCY STOP activated") {
		t.Fatalf("notifier messages = %v, want one activation alert", notifier.msgs)
	}
	var got fleetEmergency
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Active || got.Level != "llm_stopped" || got.Actor != "oleg" {
		t.Fatalf("after stop = %+v, want active llm_stopped by oleg", got)
	}
	if sw.st.Level != emergencystore.LevelLLMStopped {
		t.Fatalf("store level = %q, want llm_stopped", sw.st.Level)
	}

	// Resume.
	w = httptest.NewRecorder()
	srv.handleFleetEmergency(w, httptest.NewRequest(http.MethodPost, "/api/v1/fleet/emergency", strings.NewReader(`{"action":"resume","actor":"oleg"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200", w.Code)
	}
	got = fleetEmergency{}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Active {
		t.Fatalf("after resume = %+v, want inactive", got)
	}
	if sw.st.Level != emergencystore.LevelNone {
		t.Fatalf("store level = %q after resume, want none", sw.st.Level)
	}
}

// TestHandleFleetEmergency_Unconfigured returns 501 when no switch store is wired
// (plain serve --fleet).
func TestHandleFleetEmergency_Unconfigured(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	w := httptest.NewRecorder()
	srv.handleFleetEmergency(w, httptest.NewRequest(http.MethodPost, "/api/v1/fleet/emergency", strings.NewReader(`{"level":"llm_stopped"}`)))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 when unconfigured", w.Code)
	}
}

// TestHandleFleetEmergency_BadLevel rejects an unknown level with 400.
func TestHandleFleetEmergency_BadLevel(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetEmergencyStore(&fakeEmergencySwitch{})
	w := httptest.NewRecorder()
	srv.handleFleetEmergency(w, httptest.NewRequest(http.MethodPost, "/api/v1/fleet/emergency", strings.NewReader(`{"level":"melt-reactor"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown level", w.Code)
	}
}
