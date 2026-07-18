package repopolicy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProhibitsPublicAIAttribution_AGENTSIsAuthoritative(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("No AI attribution anywhere in git/GitHub artifacts.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("Preserve Maestro-Backend trailers.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prohibited, err := ProhibitsPublicAIAttribution(root)
	if err != nil {
		t.Fatal(err)
	}
	if !prohibited {
		t.Fatal("AGENTS.md no-attribution policy was not detected")
	}
}

func TestProhibitsPublicAIAttribution_DoesNotInferProhibition(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Preserve the historical Maestro-Backend trailer.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prohibited, err := ProhibitsPublicAIAttribution(root)
	if err != nil {
		t.Fatal(err)
	}
	if prohibited {
		t.Fatal("positive attribution guidance was misread as a prohibition")
	}
}

func TestContainsForbiddenPublicAttribution(t *testing.T) {
	for _, text := range []string{
		"subject\n\nMaestro-Backend: codex openai gpt-5.6\n",
		"Co-authored-by: Claude <noreply@anthropic.com>\n",
		"🤖 Generated with Claude Code\n",
	} {
		if !ContainsForbiddenPublicAttribution(text) {
			t.Fatalf("forbidden attribution not detected in %q", text)
		}
	}
	if ContainsForbiddenPublicAttribution("Co-authored-by: Ada Lovelace <ada@example.com>\n") {
		t.Fatal("human co-author trailer must remain allowed")
	}
}
