package versioning

import (
	"fmt"
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

func TestResolveFiles(t *testing.T) {
	tests := []struct {
		localPath string
		files     []string
		want      []string
	}{
		{"/repo", []string{"Cargo.toml"}, []string{"/repo/Cargo.toml"}},
		{"/repo", []string{"/abs/path.json"}, []string{"/abs/path.json"}},
		{"/repo", []string{"a.toml", "/b.json"}, []string{"/repo/a.toml", "/b.json"}},
	}
	for _, tt := range tests {
		got := ResolveFiles(tt.localPath, tt.files)
		if len(got) != len(tt.want) {
			t.Errorf("ResolveFiles(%q, %v) len = %d, want %d", tt.localPath, tt.files, len(got), len(tt.want))
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("ResolveFiles(%q, %v)[%d] = %q, want %q", tt.localPath, tt.files, i, got[i], tt.want[i])
			}
		}
	}
}

// mockPRClient implements PRClient for testing.
type mockPRClient struct {
	labels  []string
	commits []string

	labelsErr  error
	commitsErr error
	releaseErr error

	releaseCalls []string // tags passed to CreateRelease
}

func (m *mockPRClient) PRLabels(prNumber int) ([]string, error) {
	return m.labels, m.labelsErr
}

func (m *mockPRClient) PRCommits(prNumber int) ([]string, error) {
	return m.commits, m.commitsErr
}

func (m *mockPRClient) CreateRelease(tag, title string) error {
	m.releaseCalls = append(m.releaseCalls, tag)
	return m.releaseErr
}

func TestDetectBump(t *testing.T) {
	tests := []struct {
		name        string
		fileContent string
		labels      []string
		commits     []string
		defaultBump string
		wantBump    BumpType
		wantOld     Version
		wantNew     Version
	}{
		{
			name:        "patch from label",
			fileContent: `version = "1.0.0"`,
			labels:      []string{"version:patch"},
			commits:     []string{"feat: something"},
			defaultBump: "patch",
			wantBump:    BumpPatch,
			wantOld:     Version{1, 0, 0},
			wantNew:     Version{1, 0, 1},
		},
		{
			name:        "minor from label",
			fileContent: `version = "1.2.3"`,
			labels:      []string{"version:minor"},
			commits:     []string{"fix: typo"},
			defaultBump: "patch",
			wantBump:    BumpMinor,
			wantOld:     Version{1, 2, 3},
			wantNew:     Version{1, 3, 0},
		},
		{
			name:        "major from label",
			fileContent: `version = "0.9.5"`,
			labels:      []string{"version:major", "bug"},
			commits:     []string{"fix: small thing"},
			defaultBump: "patch",
			wantBump:    BumpMajor,
			wantOld:     Version{0, 9, 5},
			wantNew:     Version{1, 0, 0},
		},
		{
			name:        "label takes priority over commits",
			fileContent: `version = "2.0.0"`,
			labels:      []string{"version:patch"},
			commits:     []string{"feat!: breaking change"},
			defaultBump: "patch",
			wantBump:    BumpPatch,
			wantOld:     Version{2, 0, 0},
			wantNew:     Version{2, 0, 1},
		},
		{
			name:        "fallback to commits when no version label",
			fileContent: `version = "1.0.0"`,
			labels:      []string{"bug", "enhancement"},
			commits:     []string{"feat: new api endpoint"},
			defaultBump: "patch",
			wantBump:    BumpMinor,
			wantOld:     Version{1, 0, 0},
			wantNew:     Version{1, 1, 0},
		},
		{
			name:        "breaking commit detected from conventional prefix",
			fileContent: `version = "3.1.2"`,
			labels:      []string{},
			commits:     []string{"fix: a", "feat!: breaking api change"},
			defaultBump: "patch",
			wantBump:    BumpMajor,
			wantOld:     Version{3, 1, 2},
			wantNew:     Version{4, 0, 0},
		},
		{
			name:        "default bump used when no labels or conventional commits",
			fileContent: `version = "0.1.0"`,
			labels:      []string{},
			commits:     []string{"chore: update readme"},
			defaultBump: "minor",
			wantBump:    BumpMinor,
			wantOld:     Version{0, 1, 0},
			wantNew:     Version{0, 2, 0},
		},
		{
			name:        "default patch when no signals at all",
			fileContent: `version = "5.0.0"`,
			labels:      []string{},
			commits:     []string{"docs: update readme"},
			defaultBump: "patch",
			wantBump:    BumpPatch,
			wantOld:     Version{5, 0, 0},
			wantNew:     Version{5, 0, 1},
		},
		{
			name:        "package.json format",
			fileContent: `{"name": "app", "version": "2.3.4"}`,
			labels:      []string{"version:minor"},
			commits:     []string{},
			defaultBump: "patch",
			wantBump:    BumpMinor,
			wantOld:     Version{2, 3, 4},
			wantNew:     Version{2, 4, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			versionFile := filepath.Join(dir, "Cargo.toml")
			os.WriteFile(versionFile, []byte(tt.fileContent), 0644)

			mock := &mockPRClient{
				labels:  tt.labels,
				commits: tt.commits,
			}

			result, err := DetectBump(mock, 42, []string{versionFile}, tt.defaultBump)
			if err != nil {
				t.Fatalf("DetectBump: %v", err)
			}

			if result.BumpType != tt.wantBump {
				t.Errorf("BumpType = %v, want %v", result.BumpType, tt.wantBump)
			}
			if result.OldVersion != tt.wantOld {
				t.Errorf("OldVersion = %v, want %v", result.OldVersion, tt.wantOld)
			}
			if result.NewVersion != tt.wantNew {
				t.Errorf("NewVersion = %v, want %v", result.NewVersion, tt.wantNew)
			}
		})
	}
}

func TestDetectBump_CommitFallbackOnError(t *testing.T) {
	// When PRCommits fails, should fall back to default bump
	dir := t.TempDir()
	versionFile := filepath.Join(dir, "Cargo.toml")
	os.WriteFile(versionFile, []byte(`version = "1.0.0"`), 0644)

	mock := &mockPRClient{
		labels:     []string{"bug"},
		commits:    nil,
		commitsErr: fmt.Errorf("gh api error"),
	}

	result, err := DetectBump(mock, 1, []string{versionFile}, "minor")
	if err != nil {
		t.Fatalf("DetectBump: %v", err)
	}
	// No version label, commits fail → fall back to default "minor"
	if result.BumpType != BumpMinor {
		t.Errorf("BumpType = %v, want %v (default fallback)", result.BumpType, BumpMinor)
	}
}

func TestDetectBump_LabelErrorReturnsError(t *testing.T) {
	dir := t.TempDir()
	versionFile := filepath.Join(dir, "Cargo.toml")
	os.WriteFile(versionFile, []byte(`version = "1.0.0"`), 0644)

	mock := &mockPRClient{
		labelsErr: fmt.Errorf("gh api error"),
	}

	_, err := DetectBump(mock, 1, []string{versionFile}, "patch")
	if err == nil {
		t.Error("expected error when PRLabels fails")
	}
}

func TestDetectBump_NoVersionFile(t *testing.T) {
	mock := &mockPRClient{
		labels: []string{"version:patch"},
	}

	_, err := DetectBump(mock, 1, []string{"/nonexistent/file"}, "patch")
	if err == nil {
		t.Error("expected error when no version file exists")
	}
}

func TestReadCurrentVersion_MultipleFiles(t *testing.T) {
	dir := t.TempDir()

	// First file has no version
	noVer := filepath.Join(dir, "README.md")
	os.WriteFile(noVer, []byte("# My App\n"), 0644)

	// Second file has version
	pkg := filepath.Join(dir, "package.json")
	os.WriteFile(pkg, []byte(`{"version": "3.2.1"}`), 0644)

	v, err := ReadCurrentVersion([]string{noVer, pkg})
	if err != nil {
		t.Fatalf("ReadCurrentVersion: %v", err)
	}
	if v != "3.2.1" {
		t.Errorf("got %q, want %q", v, "3.2.1")
	}
}
