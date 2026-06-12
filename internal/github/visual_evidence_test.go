package github

import (
	"reflect"
	"testing"
)

func TestParsePRChangedFiles(t *testing.T) {
	out := []byte(`[
		{"filename": "web/src/App.jsx", "status": "modified"},
		{"filename": "internal/api/server.go", "status": "added"},
		{"filename": "  "}
	]`)
	files, err := parsePRChangedFiles(out)
	if err != nil {
		t.Fatalf("parsePRChangedFiles: %v", err)
	}
	want := []string{"web/src/App.jsx", "internal/api/server.go"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
}

func TestParsePRChangedFiles_Invalid(t *testing.T) {
	if _, err := parsePRChangedFiles([]byte("not json")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestContainsEmbeddedImage(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"markdown image", "### Visual evidence\n![home](https://raw.githubusercontent.com/o/r/abc/.maestro/screenshots/home.png)", true},
		{"img tag", `before <img src="https://example.com/shot.png" width="400"> after`, true},
		{"user-images link", "see https://user-images.githubusercontent.com/123/shot-1.PNG for evidence", true},
		{"bare jpeg url", "uploaded to https://example.com/captures/page.jpeg?raw=true", true},
		{"plain text", "no screenshots here, tests pass", false},
		{"non-image link", "see https://example.com/report.html", false},
		{"link text mentioning png", "the file home.png is in the repo", false},
		{"empty", "  ", false},
	}
	for _, tc := range cases {
		if got := ContainsEmbeddedImage(tc.text); got != tc.want {
			t.Errorf("%s: ContainsEmbeddedImage = %v, want %v", tc.name, got, tc.want)
		}
	}
}
