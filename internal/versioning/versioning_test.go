package versioning

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input   string
		want    Version
		wantErr bool
	}{
		{"0.5.0", Version{0, 5, 0}, false},
		{"1.2.3", Version{1, 2, 3}, false},
		{"v1.0.0", Version{1, 0, 0}, false},
		{"10.20.30", Version{10, 20, 30}, false},
		{"", Version{}, true},
		{"1.2", Version{}, true},
		{"abc", Version{}, true},
		{"1.2.x", Version{}, true},
	}
	for _, tt := range tests {
		got, err := ParseVersion(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseVersion(%q) expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseVersion(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestVersionString(t *testing.T) {
	v := Version{1, 2, 3}
	if s := v.String(); s != "1.2.3" {
		t.Errorf("Version.String() = %q, want %q", s, "1.2.3")
	}
}

func TestBump(t *testing.T) {
	tests := []struct {
		input Version
		bump  BumpType
		want  Version
	}{
		{Version{0, 5, 0}, BumpPatch, Version{0, 5, 1}},
		{Version{0, 5, 0}, BumpMinor, Version{0, 6, 0}},
		{Version{0, 5, 0}, BumpMajor, Version{1, 0, 0}},
		{Version{1, 2, 3}, BumpPatch, Version{1, 2, 4}},
		{Version{1, 2, 3}, BumpMinor, Version{1, 3, 0}},
		{Version{1, 2, 3}, BumpMajor, Version{2, 0, 0}},
	}
	for _, tt := range tests {
		got := Bump(tt.input, tt.bump)
		if got != tt.want {
			t.Errorf("Bump(%v, %v) = %v, want %v", tt.input, tt.bump, got, tt.want)
		}
	}
}

func TestDetectBumpFromLabels(t *testing.T) {
	tests := []struct {
		labels      []string
		defaultBump string
		wantBump    BumpType
		wantFound   bool
	}{
		{[]string{"version:patch"}, "patch", BumpPatch, true},
		{[]string{"version:minor"}, "patch", BumpMinor, true},
		{[]string{"version:major"}, "patch", BumpMajor, true},
		{[]string{"version:patch", "version:major"}, "patch", BumpMajor, true},
		{[]string{"bug", "enhancement"}, "patch", BumpPatch, false},
		{[]string{}, "minor", BumpMinor, false},
		{[]string{"Version:Minor"}, "patch", BumpMinor, true}, // case insensitive
	}
	for _, tt := range tests {
		bump, found := DetectBumpFromLabels(tt.labels, tt.defaultBump)
		if bump != tt.wantBump || found != tt.wantFound {
			t.Errorf("DetectBumpFromLabels(%v, %q) = (%v, %v), want (%v, %v)",
				tt.labels, tt.defaultBump, bump, found, tt.wantBump, tt.wantFound)
		}
	}
}

func TestDetectBumpFromCommits(t *testing.T) {
	tests := []struct {
		messages    []string
		defaultBump string
		want        BumpType
	}{
		{[]string{"fix: typo"}, "patch", BumpPatch},
		{[]string{"feat: new api"}, "patch", BumpMinor},
		{[]string{"feat!: breaking change"}, "patch", BumpMajor},
		{[]string{"fix: a", "feat: b"}, "patch", BumpMinor},
		{[]string{"fix: a", "feat!: b"}, "patch", BumpMajor},
		{[]string{"chore: update deps"}, "patch", BumpPatch},
		{[]string{"chore: update deps"}, "minor", BumpMinor},
		{[]string{}, "patch", BumpPatch},
	}
	for _, tt := range tests {
		got := DetectBumpFromCommits(tt.messages, tt.defaultBump)
		if got != tt.want {
			t.Errorf("DetectBumpFromCommits(%v, %q) = %v, want %v",
				tt.messages, tt.defaultBump, got, tt.want)
		}
	}
}

func TestParseBumpType(t *testing.T) {
	tests := []struct {
		input string
		want  BumpType
	}{
		{"patch", BumpPatch},
		{"minor", BumpMinor},
		{"major", BumpMajor},
		{"MAJOR", BumpMajor},
		{"Minor", BumpMinor},
		{"unknown", BumpPatch},
		{"", BumpPatch},
	}
	for _, tt := range tests {
		if got := ParseBumpType(tt.input); got != tt.want {
			t.Errorf("ParseBumpType(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestReadVersionFromFile(t *testing.T) {
	dir := t.TempDir()

	// Cargo.toml style
	cargoPath := filepath.Join(dir, "Cargo.toml")
	os.WriteFile(cargoPath, []byte(`[package]
name = "myapp"
version = "1.2.3"
`), 0644)
	v, err := ReadVersionFromFile(cargoPath)
	if err != nil {
		t.Fatalf("ReadVersionFromFile(Cargo.toml): %v", err)
	}
	if v != "1.2.3" {
		t.Errorf("got %q, want %q", v, "1.2.3")
	}

	// package.json style
	pkgPath := filepath.Join(dir, "package.json")
	os.WriteFile(pkgPath, []byte(`{
  "name": "myapp",
  "version": "2.0.1"
}
`), 0644)
	v, err = ReadVersionFromFile(pkgPath)
	if err != nil {
		t.Fatalf("ReadVersionFromFile(package.json): %v", err)
	}
	if v != "2.0.1" {
		t.Errorf("got %q, want %q", v, "2.0.1")
	}

	// No version
	noVerPath := filepath.Join(dir, "empty.txt")
	os.WriteFile(noVerPath, []byte("no version here\n"), 0644)
	_, err = ReadVersionFromFile(noVerPath)
	if err == nil {
		t.Error("expected error for file with no version")
	}
}

func TestUpdateVersionInFile(t *testing.T) {
	dir := t.TempDir()

	content := `[package]
name = "myapp"
version = "0.5.0"
edition = "2021"
`
	path := filepath.Join(dir, "Cargo.toml")
	os.WriteFile(path, []byte(content), 0644)

	if err := UpdateVersionInFile(path, "0.5.0", "0.6.0"); err != nil {
		t.Fatalf("UpdateVersionInFile: %v", err)
	}

	data, _ := os.ReadFile(path)
	got := string(data)
	if expected := `[package]
name = "myapp"
version = "0.6.0"
edition = "2021"
`; got != expected {
		t.Errorf("file content mismatch:\ngot:  %q\nwant: %q", got, expected)
	}

	// Trying to update a version that doesn't exist
	if err := UpdateVersionInFile(path, "9.9.9", "10.0.0"); err == nil {
		t.Error("expected error for non-existent version")
	}
}

func TestBumpTypeString(t *testing.T) {
	tests := []struct {
		bt   BumpType
		want string
	}{
		{BumpPatch, "patch"},
		{BumpMinor, "minor"},
		{BumpMajor, "major"},
	}
	for _, tt := range tests {
		if got := tt.bt.String(); got != tt.want {
			t.Errorf("BumpType(%d).String() = %q, want %q", tt.bt, got, tt.want)
		}
	}
}
