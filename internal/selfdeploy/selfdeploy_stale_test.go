package selfdeploy

import (
	"os"
	"strings"
	"testing"
	"time"
)

// #807: the resolved watermark round-trips through the state dir so the
// stale-trigger watchdog survives the orchestrator unit restarting.
func TestRecordAndReadLastResolved(t *testing.T) {
	dir := t.TempDir()

	if _, ok := LastResolved(dir); ok {
		t.Fatal("LastResolved ok=true with no marker")
	}

	when := time.Date(2026, 7, 3, 6, 55, 0, 0, time.UTC)
	if err := RecordResolved(dir, when); err != nil {
		t.Fatalf("RecordResolved: %v", err)
	}
	got, ok := LastResolved(dir)
	if !ok {
		t.Fatal("LastResolved ok=false after RecordResolved")
	}
	if !got.Equal(when) {
		t.Errorf("LastResolved = %s, want %s", got, when)
	}
}

// A blank state dir is a no-op, matching RecordTrigger.
func TestRecordResolvedBlankStateDir(t *testing.T) {
	if err := RecordResolved("", time.Now().UTC()); err != nil {
		t.Fatalf("RecordResolved(\"\"): %v", err)
	}
	if _, ok := LastResolved(""); ok {
		t.Fatal("LastResolved(\"\") ok=true, want false")
	}
}

// A malformed watermark fails open (ok=false) so a corrupt file never hides a
// genuinely-lost deploy.
func TestLastResolvedMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(resolvedMarkerPath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := LastResolved(dir); ok {
		t.Fatal("LastResolved ok=true for malformed marker")
	}
}

func TestStaleTrigger(t *testing.T) {
	const timeout = 60 * time.Minute
	now := time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)

	t.Run("no marker → not stale", func(t *testing.T) {
		if _, _, stale := StaleTrigger(t.TempDir(), now, timeout); stale {
			t.Fatal("stale=true with no trigger marker")
		}
	})

	t.Run("recent trigger within timeout → not stale (in-flight deploy)", func(t *testing.T) {
		dir := t.TempDir()
		if err := RecordTrigger(dir, 806, now.Add(-10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, _, stale := StaleTrigger(dir, now, timeout); stale {
			t.Fatal("stale=true for a trigger still inside the timeout window")
		}
	})

	t.Run("old trigger, no result → stale", func(t *testing.T) {
		dir := t.TempDir()
		if err := RecordTrigger(dir, 806, now.Add(-90*time.Minute)); err != nil {
			t.Fatal(err)
		}
		pr, age, stale := StaleTrigger(dir, now, timeout)
		if !stale {
			t.Fatal("stale=false for an old trigger with no result — the silent-loss case")
		}
		if pr != 806 {
			t.Errorf("pr = %d, want 806", pr)
		}
		if age != 90*time.Minute {
			t.Errorf("age = %s, want 90m", age)
		}
	})

	t.Run("old trigger but result present → not stale (consumed this cycle)", func(t *testing.T) {
		dir := t.TempDir()
		if err := RecordTrigger(dir, 806, now.Add(-90*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ResultPath(dir), []byte(`{"status":"deployed","pr":806}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, stale := StaleTrigger(dir, now, timeout); stale {
			t.Fatal("stale=true while a result file waits to be consumed")
		}
	})

	t.Run("old trigger already resolved → not stale (completed deploy)", func(t *testing.T) {
		dir := t.TempDir()
		triggeredAt := now.Add(-90 * time.Minute)
		if err := RecordTrigger(dir, 806, triggeredAt); err != nil {
			t.Fatal(err)
		}
		// A result was consumed AFTER the trigger — the marker legitimately
		// outlives the cleared result; this must not read as a silent loss.
		if err := RecordResolved(dir, triggeredAt.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, _, stale := StaleTrigger(dir, now, timeout); stale {
			t.Fatal("stale=true for a trigger whose result was already resolved")
		}
	})

	t.Run("new trigger after an old resolution → stale again", func(t *testing.T) {
		dir := t.TempDir()
		// Previous deploy resolved long ago.
		if err := RecordResolved(dir, now.Add(-5*time.Hour)); err != nil {
			t.Fatal(err)
		}
		// A fresh trigger fired after that resolution and never produced a result.
		if err := RecordTrigger(dir, 900, now.Add(-90*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, _, stale := StaleTrigger(dir, now, timeout); !stale {
			t.Fatal("stale=false for a new trigger that fired after the last resolution")
		}
	})
}

func TestStaleTriggerFinding(t *testing.T) {
	now := time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)
	d := StaleTriggerFinding(806, 92*time.Minute, "owner/repo", now)

	if d.Status != "failed" {
		t.Errorf("Status = %q, want failed", d.Status)
	}
	if d.RequiresApproval {
		t.Error("watchdog finding must not require approval")
	}
	if !strings.Contains(d.Summary, "no result") || !strings.Contains(d.Summary, "PR #806") {
		t.Errorf("Summary = %q", d.Summary)
	}
	if !strings.HasPrefix(d.ID, "self-deploy-stale-trigger-") {
		t.Errorf("ID = %q, want self-deploy-stale-trigger- prefix", d.ID)
	}
	if d.Project != "owner/repo" {
		t.Errorf("Project = %q", d.Project)
	}
}
