package emergencystore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMessages(t *testing.T) {
	llm := State{Level: LevelLLMStopped, Actor: "oleg", Reason: "burn"}.ActivationMessage()
	if !strings.Contains(llm, "EMERGENCY STOP activated") || !strings.Contains(llm, "oleg") || !strings.Contains(llm, "burn") {
		t.Fatalf("activation message missing content: %q", llm)
	}
	all := State{Level: LevelAllStopped, Actor: "a"}.ActivationMessage()
	if !strings.Contains(all, "whole fleet stopped") {
		t.Fatalf("all-stopped activation message wrong: %q", all)
	}
	resume := ResumeMessage(State{Level: LevelLLMStopped}, State{Level: LevelNone, Actor: "op"})
	if !strings.Contains(resume, "resumed") || !strings.Contains(resume, "op") {
		t.Fatalf("resume message missing content: %q", resume)
	}
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"":            LevelNone,
		"none":        LevelNone,
		"llm_stopped": LevelLLMStopped,
		"llm":         LevelLLMStopped,
		"stop-llm":    LevelLLMStopped,
		"all_stopped": LevelAllStopped,
		"all":         LevelAllStopped,
		"STOP-ALL":    LevelAllStopped,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseLevel(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseLevel("bogus"); err == nil {
		t.Fatal("ParseLevel(bogus) should error")
	}
}

func TestGet_DefaultsToNone(t *testing.T) {
	s := openTemp(t)
	st, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Level != LevelNone {
		t.Fatalf("fresh store level = %q, want none", st.Level)
	}
	if st.Active() || st.HaltsLLM() || st.HaltsSpawns() {
		t.Fatal("fresh store must not be active")
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)

	if err := s.Set(ctx, LevelLLMStopped, "oleg", "runaway supervise burn", at); err != nil {
		t.Fatalf("set: %v", err)
	}
	st, err := s.Get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Level != LevelLLMStopped {
		t.Fatalf("level = %q, want llm_stopped", st.Level)
	}
	if !st.Active() || !st.HaltsLLM() || !st.HaltsSpawns() {
		t.Fatal("llm_stopped must halt LLM + spawns and be active")
	}
	if st.Actor != "oleg" || st.Reason != "runaway supervise burn" {
		t.Fatalf("actor/reason = %q/%q", st.Actor, st.Reason)
	}
	if !st.Since.Equal(at) {
		t.Fatalf("since = %v, want %v", st.Since, at)
	}
}

// TestSetOverwritesAndResume covers the idempotent single-row upsert and the
// resume path (LevelNone clears the switch).
func TestSetOverwritesAndResume(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)

	if err := s.Set(ctx, LevelLLMStopped, "a", "first", at); err != nil {
		t.Fatalf("set llm: %v", err)
	}
	if err := s.Set(ctx, LevelAllStopped, "b", "escalate", at.Add(time.Minute)); err != nil {
		t.Fatalf("set all: %v", err)
	}
	st, _ := s.Get(ctx)
	if st.Level != LevelAllStopped || st.Actor != "b" {
		t.Fatalf("after overwrite level=%q actor=%q, want all_stopped/b", st.Level, st.Actor)
	}

	if err := s.Resume(ctx, "operator", at.Add(2*time.Minute)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st, _ = s.Get(ctx)
	if st.Active() {
		t.Fatalf("after resume still active: %+v", st)
	}
	if st.Actor != "operator" {
		t.Fatalf("resume actor = %q, want operator", st.Actor)
	}
	if !st.Since.IsZero() {
		t.Fatalf("since should be zero after resume, got %v", st.Since)
	}
}

// TestSurvivesReopen is the restart-persistence guarantee (#840 AC: flag
// survives `systemctl restart maestro.service`). Closing and re-opening the same
// file must reload the switch intact.
func TestSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "maestro.db")
	at := time.Date(2026, 7, 9, 14, 30, 0, 0, time.UTC)

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := s1.Set(context.Background(), LevelLLMStopped, "oleg", "incident", at); err != nil {
		t.Fatalf("set: %v", err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer s2.Close()
	st, err := s2.Get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Level != LevelLLMStopped || st.Actor != "oleg" {
		t.Fatalf("switch lost across reopen: %+v", st)
	}
	if !st.Since.Equal(at) {
		t.Fatalf("since lost across reopen: %v, want %v", st.Since, at)
	}
}

func TestFingerprintAdvancesOnWrite(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	fp0, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint0: %v", err)
	}
	if !fp0.IsZero() {
		t.Fatalf("fresh fingerprint = %v, want zero", fp0)
	}
	if err := s.Set(ctx, LevelLLMStopped, "a", "x", time.Now().UTC()); err != nil {
		t.Fatalf("set: %v", err)
	}
	fp1, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint1: %v", err)
	}
	if fp1.IsZero() {
		t.Fatal("fingerprint did not advance after write")
	}
}
