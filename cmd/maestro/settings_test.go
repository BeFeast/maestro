package main

import (
	"reflect"
	"testing"
)

func TestSplitSettingsArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantF   []string
		wantPos []string
	}{
		{
			name:    "positional first, flag after",
			args:    []string{"supervisor.enabled=false", "--project", "idle"},
			wantF:   []string{"--project", "idle"},
			wantPos: []string{"supervisor.enabled=false"},
		},
		{
			name:    "flag first, positional after",
			args:    []string{"--db", "/tmp/x.db", "worker_max_tokens=100"},
			wantF:   []string{"--db", "/tmp/x.db"},
			wantPos: []string{"worker_max_tokens=100"},
		},
		{
			name:    "equals-form flag mixed",
			args:    []string{"--project=idle", "supervisor.backend=codex", "--actor", "me"},
			wantF:   []string{"--project=idle", "--actor", "me"},
			wantPos: []string{"supervisor.backend=codex"},
		},
		{
			name:    "no flags",
			args:    []string{"poll_interval_seconds"},
			wantF:   nil,
			wantPos: []string{"poll_interval_seconds"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotF, gotPos := splitSettingsArgs(tc.args)
			if !reflect.DeepEqual(gotF, tc.wantF) {
				t.Errorf("flags = %v, want %v", gotF, tc.wantF)
			}
			if !reflect.DeepEqual(gotPos, tc.wantPos) {
				t.Errorf("positionals = %v, want %v", gotPos, tc.wantPos)
			}
		})
	}
}

func TestSettingsActor(t *testing.T) {
	if got := settingsActor("incident-responder"); got != "incident-responder" {
		t.Errorf("explicit actor = %q, want incident-responder", got)
	}
	t.Setenv("USER", "god")
	if got := settingsActor(""); got != "cli:god" {
		t.Errorf("default actor = %q, want cli:god", got)
	}
	t.Setenv("USER", "")
	if got := settingsActor(""); got != "cli:unknown" {
		t.Errorf("empty-user actor = %q, want cli:unknown", got)
	}
}
