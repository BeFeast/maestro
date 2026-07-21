package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/tmpfshygiene"
)

func TestFleetAPISurfacesTmpfsPressure(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 0, true)
	srv.SetTmpfsHygieneSource(func() (tmpfshygiene.Summary, bool) {
		return tmpfshygiene.Summary{
			Timestamp:     time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC),
			Mode:          tmpfshygiene.ModeApply,
			Root:          "/tmp",
			Tmpfs:         true,
			UsePct:        88,
			Pressure:      true,
			AttentionCode: "tmpfs_pressure",
			FreedBytes:    4096,
		}, true
	})

	rec := httptest.NewRecorder()
	srv.handleFleet(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		TmpfsHygiene *tmpfshygiene.Summary `json:"tmpfs_hygiene"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.TmpfsHygiene == nil || body.TmpfsHygiene.AttentionCode != "tmpfs_pressure" || body.TmpfsHygiene.UsePct != 88 {
		t.Fatalf("tmpfs_hygiene = %+v", body.TmpfsHygiene)
	}
}

func TestFleetAPIOmitsTmpfsHygieneWhenNotDaemonWired(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 0, true)
	rec := httptest.NewRecorder()
	srv.handleFleet(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["tmpfs_hygiene"]; ok {
		t.Fatalf("plain fleet unexpectedly exposed tmpfs_hygiene: %s", rec.Body.String())
	}
}
