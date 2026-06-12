package pipeline

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

func TestMatchesVisualPath(t *testing.T) {
	cases := []struct {
		pattern string
		file    string
		want    bool
	}{
		{"**/*.jsx", "src/App.jsx", true},
		{"**/*.jsx", "App.jsx", true}, // ** matches zero segments
		{"**/*.jsx", "src/App.tsx", false},
		{"web/**", "web/src/Button.tsx", true},
		{"web/**", "web", true},
		{"web/**", "server/main.go", false},
		{"web/", "web/styles/app.css", true}, // trailing slash = directory prefix
		{"web/src/*.css", "web/src/app.css", true},
		{"web/src/*.css", "web/src/nested/app.css", false}, // single * does not cross /
		{"*.css", "web/app.css", false},                    // root-level only without **
		{"*.css", "app.css", true},
		{"**/components/**/*.vue", "web/src/components/ui/Button.vue", true},
		{"./web/**", "web/index.html", true}, // leading ./ normalized
		{"", "web/app.css", false},
		{"web/**", "", false},
	}
	for _, tc := range cases {
		if got := MatchesVisualPath(tc.pattern, tc.file); got != tc.want {
			t.Errorf("MatchesVisualPath(%q, %q) = %v, want %v", tc.pattern, tc.file, got, tc.want)
		}
	}
}

func TestMatchUIAffectingFiles(t *testing.T) {
	patterns := []string{"**/*.jsx", "web/**"}
	files := []string{"internal/api/server.go", "web/src/app.css", "src/Panel.jsx", "README.md"}

	got := MatchUIAffectingFiles(patterns, files)
	want := []string{"web/src/app.css", "src/Panel.jsx"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MatchUIAffectingFiles = %v, want %v", got, want)
	}

	if got := MatchUIAffectingFiles(patterns, []string{"cmd/main.go", "docs/x.md"}); len(got) != 0 {
		t.Fatalf("non-UI files should not match, got %v", got)
	}
	if got := MatchUIAffectingFiles(nil, files); len(got) != 0 {
		t.Fatalf("no patterns should match nothing, got %v", got)
	}
}

func TestRunVisualCapture_Succeeds(t *testing.T) {
	worktree := t.TempDir()
	v := config.VerifyVisualConfig{
		Enabled: true,
		Command: `mkdir -p "$MAESTRO_SCREENSHOT_DIR" && touch "$MAESTRO_SCREENSHOT_DIR/home.png" "$MAESTRO_SCREENSHOT_DIR/notes.txt"`,
		Paths:   []string{"web/**"},
	}

	shots, err := RunVisualCapture(v, worktree)
	if err != nil {
		t.Fatalf("RunVisualCapture: %v", err)
	}
	want := []string{".maestro/screenshots/home.png"}
	if !reflect.DeepEqual(shots, want) {
		t.Fatalf("screenshots = %v, want %v (non-image files must be ignored)", shots, want)
	}
}

func TestRunVisualCapture_CommandFails(t *testing.T) {
	worktree := t.TempDir()
	v := config.VerifyVisualConfig{
		Enabled: true,
		Command: "echo capture exploded >&2; exit 3",
		Paths:   []string{"web/**"},
	}

	shots, err := RunVisualCapture(v, worktree)
	if err == nil {
		t.Fatal("expected error from failing capture command")
	}
	if len(shots) != 0 {
		t.Fatalf("expected no screenshots, got %v", shots)
	}
}

func TestRunVisualCapture_NoOutput(t *testing.T) {
	worktree := t.TempDir()
	v := config.VerifyVisualConfig{
		Enabled: true,
		Command: "true",
		Paths:   []string{"web/**"},
	}

	shots, err := RunVisualCapture(v, worktree)
	if err != nil {
		t.Fatalf("RunVisualCapture: %v", err)
	}
	if len(shots) != 0 {
		t.Fatalf("expected zero screenshots for no-op command, got %v", shots)
	}
}

func TestListScreenshots_MissingDirAndNesting(t *testing.T) {
	worktree := t.TempDir()
	if got := ListScreenshots(worktree, "no/such/dir"); len(got) != 0 {
		t.Fatalf("missing dir should yield no screenshots, got %v", got)
	}

	nested := filepath.Join(worktree, "shots", "pages")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b.PNG", "a.jpeg"} {
		if err := os.WriteFile(filepath.Join(nested, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := ListScreenshots(worktree, "shots")
	want := []string{"shots/pages/a.jpeg", "shots/pages/b.PNG"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListScreenshots = %v, want %v", got, want)
	}
}
