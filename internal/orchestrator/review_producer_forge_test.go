package orchestrator

// Producer forge selection tests (#1172 M2): the llm-review producer must
// pick its forge per row — the Forgejo REST client on forgejo rows, the gh
// Forge everywhere else — and fail closed on a missing token, because the
// repos are mirrored: a fallback to the gh forge would post the review to the
// GitHub mirror instead of the Forgejo original.

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/forgejo"
	"github.com/befeast/maestro/internal/github"
)

func TestReviewForgeSelectsGitHubByDefault(t *testing.T) {
	fg, err := reviewForge(config.ForgeConfig{})
	if err != nil {
		t.Fatalf("reviewForge(zero) = %v", err)
	}
	if _, ok := fg.(*github.Forge); !ok {
		t.Fatalf("forge = %T, want *github.Forge for a zero/github row", fg)
	}
}

func TestReviewForgeSelectsForgejoWithToken(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	fg, err := reviewForge(config.ForgeConfig{
		Kind:     "forgejo",
		BaseURL:  "https://git.example.test",
		TokenEnv: "TEST_FORGEJO_TOKEN",
	})
	if err != nil {
		t.Fatalf("reviewForge(forgejo) = %v", err)
	}
	if _, ok := fg.(*forgejo.Client); !ok {
		t.Fatalf("forge = %T, want *forgejo.Client for a forgejo row", fg)
	}
}

func TestReviewForgeMissingTokenFailsClosed(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "   ")
	fg, err := reviewForge(config.ForgeConfig{
		Kind:     "forgejo",
		BaseURL:  "https://git.example.test",
		TokenEnv: "TEST_FORGEJO_TOKEN",
	})
	if err == nil {
		t.Fatalf("reviewForge = %T with an empty token, want a fail-closed error (a gh fallback would review the GitHub mirror)", fg)
	}
	if !strings.Contains(err.Error(), "TEST_FORGEJO_TOKEN") {
		t.Fatalf("err = %v, want the token env named so the fix is obvious", err)
	}
	if fg != nil {
		t.Fatalf("forge = %T alongside an error, want nil", fg)
	}
}
