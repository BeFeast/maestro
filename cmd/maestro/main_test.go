package main

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// newKillFlagSet mirrors the flags registered by killCmd so reorderArgs can be
// exercised against a realistic FlagSet.
func newKillFlagSet() (*flag.FlagSet, *multiFlag) {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	return fs, &configs
}

func TestReorderArgs(t *testing.T) {
	// boolFS registers a boolean flag to verify value-less flags do not consume
	// the following token.
	boolFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("once", flag.ContinueOnError)
		var configs multiFlag
		fs.Var(&configs, "config", "Path to config file (can be repeated)")
		fs.Bool("force", false, "force")
		return fs
	}

	tests := []struct {
		name string
		fs   *flag.FlagSet
		in   []string
		want []string
	}{
		{
			name: "flag after positional, space form",
			in:   []string{"scr-223", "--config", "/p/x.yaml"},
			want: []string{"--config", "/p/x.yaml", "scr-223"},
		},
		{
			name: "flag after positional, equals form",
			in:   []string{"scr-223", "--config=/p/x.yaml"},
			want: []string{"--config=/p/x.yaml", "scr-223"},
		},
		{
			name: "single dash flag",
			in:   []string{"scr-223", "-config", "/p/x.yaml"},
			want: []string{"-config", "/p/x.yaml", "scr-223"},
		},
		{
			name: "flags already first are unchanged",
			in:   []string{"--config", "/p/x.yaml", "scr-223"},
			want: []string{"--config", "/p/x.yaml", "scr-223"},
		},
		{
			name: "double dash terminator keeps trailing tokens positional",
			in:   []string{"scr-223", "--", "--config", "/p/x.yaml"},
			want: []string{"scr-223", "--", "--config", "/p/x.yaml"},
		},
		{
			name: "unknown flag stays in place",
			in:   []string{"scr-223", "--bogus", "v"},
			want: []string{"scr-223", "--bogus", "v"},
		},
		{
			name: "repeated config flags both lifted",
			in:   []string{"scr-223", "--config", "/a.yaml", "--config", "/b.yaml"},
			want: []string{"--config", "/a.yaml", "--config", "/b.yaml", "scr-223"},
		},
		{
			name: "bool flag does not consume next token",
			fs:   boolFS(),
			in:   []string{"slot", "--force", "--config", "/a.yaml"},
			want: []string{"--force", "--config", "/a.yaml", "slot"},
		},
		{
			name: "lone dash is positional",
			in:   []string{"-", "--config", "/a.yaml"},
			want: []string{"--config", "/a.yaml", "-"},
		},
		{
			name: "empty args",
			in:   []string{},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := tt.fs
			if fs == nil {
				fs, _ = newKillFlagSet()
			}
			got := reorderArgs(fs, tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("reorderArgs(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestReorderArgsParseEquivalence asserts that parsing flags-after-positional
// yields the same result as flags-first, which is the behavior #459 requires.
func TestReorderArgsParseEquivalence(t *testing.T) {
	cases := []struct {
		name       string
		after      []string
		first      []string
		wantConfig []string
		wantArgs   []string
	}{
		{
			name:       "space form",
			after:      []string{"scr-223", "--config", "/p/x.yaml"},
			first:      []string{"--config", "/p/x.yaml", "scr-223"},
			wantConfig: []string{"/p/x.yaml"},
			wantArgs:   []string{"scr-223"},
		},
		{
			name:       "equals form",
			after:      []string{"scr-223", "--config=/p/x.yaml"},
			first:      []string{"--config=/p/x.yaml", "scr-223"},
			wantConfig: []string{"/p/x.yaml"},
			wantArgs:   []string{"scr-223"},
		},
		{
			name:       "terminator keeps flag-looking positional",
			after:      []string{"--config", "/p/x.yaml", "--", "-weird-slot"},
			first:      []string{"--config", "/p/x.yaml", "--", "-weird-slot"},
			wantConfig: []string{"/p/x.yaml"},
			wantArgs:   []string{"-weird-slot"},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			parse := func(args []string) (multiFlag, []string) {
				fs, configs := newKillFlagSet()
				if err := fs.Parse(reorderArgs(fs, args)); err != nil {
					t.Fatalf("parse %q: %v", args, err)
				}
				return *configs, fs.Args()
			}

			afterConfig, afterArgs := parse(tt.after)
			firstConfig, firstArgs := parse(tt.first)

			if !reflect.DeepEqual([]string(afterConfig), tt.wantConfig) {
				t.Errorf("after-positional config = %q, want %q", afterConfig, tt.wantConfig)
			}
			if !reflect.DeepEqual(afterArgs, tt.wantArgs) {
				t.Errorf("after-positional args = %q, want %q", afterArgs, tt.wantArgs)
			}
			if !reflect.DeepEqual([]string(afterConfig), []string(firstConfig)) {
				t.Errorf("config mismatch: after=%q first=%q", afterConfig, firstConfig)
			}
			if !reflect.DeepEqual(afterArgs, firstArgs) {
				t.Errorf("args mismatch: after=%q first=%q", afterArgs, firstArgs)
			}
		})
	}
}

func TestLoadConfigsWithStoreFallsBackToYAMLPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "maestro.yaml")
	if err := os.WriteFile(configPath, []byte("repo: owner/repo\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfgs := loadConfigsWithStore([]string{configPath}, filepath.Join(dir, "missing", "config.db"), "")
	if len(cfgs) != 1 {
		t.Fatalf("len(cfgs) = %d, want 1", len(cfgs))
	}
	if cfgs[0].Repo != "owner/repo" {
		t.Fatalf("Repo = %q, want owner/repo", cfgs[0].Repo)
	}
}
