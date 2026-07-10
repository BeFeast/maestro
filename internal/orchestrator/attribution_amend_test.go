package orchestrator

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// These tests exercise the real git-level attribution amend against temp repos,
// reproducing the "stale info" force-with-lease race the orchestrator hit in
// production when a worker/operator push lands between our read and our push
// (#858).

func amendGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func amendWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func amendConfig(t *testing.T, dir string) {
	t.Helper()
	amendGit(t, dir, "config", "user.email", "test@example.com")
	amendGit(t, dir, "config", "user.name", "Test User")
}

// setupAmendRepo builds a bare origin holding a feature branch with one worker
// commit, plus two clones:
//   - wt:   the orchestrator's worker worktree, checked out on the branch,
//     tracking origin/<branch> — the amend target.
//   - seed: a separate clone on the branch used to push new commits to origin,
//     simulating a concurrent worker/operator push.
func setupAmendRepo(t *testing.T) (origin, wt, seed, branch string) {
	t.Helper()
	branch = "feat/sup-1-worker-feature"
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	seed = filepath.Join(root, "seed")
	wt = filepath.Join(root, "wt")

	amendGit(t, root, "init", "--bare", origin)
	amendGit(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")

	amendGit(t, root, "clone", origin, seed)
	amendConfig(t, seed)
	amendWrite(t, seed, "README.md", "v1\n")
	amendGit(t, seed, "add", "README.md")
	amendGit(t, seed, "commit", "-m", "initial")
	amendGit(t, seed, "push", "-u", "origin", "main")
	amendGit(t, seed, "checkout", "-b", branch)
	amendWrite(t, seed, "work.txt", "worker output v1\n")
	amendGit(t, seed, "add", "work.txt")
	amendGit(t, seed, "commit", "-m", "worker: implement feature")
	amendGit(t, seed, "push", "-u", "origin", branch)

	amendGit(t, root, "clone", origin, wt)
	amendConfig(t, wt)
	amendGit(t, wt, "checkout", branch)
	return origin, wt, seed, branch
}

func testAttribution() []state.BackendAttribution {
	return []state.BackendAttribution{{
		Backend:   "claude",
		Provider:  "anthropic",
		Model:     "opus-4.8",
		Effort:    "xhigh",
		StartedAt: time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
	}}
}

func trailerCount(msg string) int {
	return strings.Count(msg, state.AttributionTrailerKey+":")
}

// On a stale-info rejection, the amend re-fetches, re-applies the trailer onto
// the commit the worker actually pushed, and retries — landing the trailer
// exactly once without discarding the worker's concurrent commit.
func TestAmendHead_StaleInfoRefetchRetrySucceeds(t *testing.T) {
	origin, wt, seed, branch := setupAmendRepo(t)

	prevBackoff := amendRetryBackoff
	amendRetryBackoff = 0
	defer func() { amendRetryBackoff = prevBackoff }()

	// The concurrent worker push lands exactly once, in the window between our
	// read and our first force-with-lease push (requirement: simulate the race).
	fired := false
	amendPushRaceHook = func() {
		if fired {
			return
		}
		fired = true
		amendWrite(t, seed, "work.txt", "worker output v2 (concurrent)\n")
		amendGit(t, seed, "commit", "-am", "worker: follow-up commit")
		amendGit(t, seed, "push", "origin", branch)
	}
	defer func() { amendPushRaceHook = nil }()

	now := time.Date(2026, 7, 10, 4, 5, 0, 0, time.UTC)
	if err := amendHeadWithAttributionTrailer(wt, branch, testAttribution(), now); err != nil {
		t.Fatalf("amend should recover from stale-info race, got: %v", err)
	}
	if !fired {
		t.Fatal("race hook never fired — test did not exercise the stale-info path")
	}

	headMsg := amendGit(t, origin, "log", "-1", "--pretty=%B", branch)
	if !strings.Contains(headMsg, state.AttributionTrailerKey+":") {
		t.Fatalf("origin head missing attribution trailer:\n%s", headMsg)
	}
	if n := trailerCount(headMsg); n != 1 {
		t.Fatalf("trailer appears %d times, want exactly 1:\n%s", n, headMsg)
	}
	// The worker's concurrent commit must survive — only its message is
	// reworded to carry the trailer; its work (and log subject) is intact.
	work := amendGit(t, origin, "show", branch+":work.txt")
	if !strings.Contains(work, "concurrent") {
		t.Fatalf("worker's concurrent work.txt was discarded: %q", work)
	}
	if !strings.Contains(headMsg, "worker: follow-up commit") {
		t.Fatalf("head is not the worker's follow-up commit reworded:\n%s", headMsg)
	}
}

// When the branch keeps advancing under a live worker, every push loses the
// lease. The amend defers (errAmendDeferred) instead of erroring, and never
// force-pushes over the worker's commits.
func TestAmendHead_BranchKeepsMovingDefers(t *testing.T) {
	origin, wt, seed, branch := setupAmendRepo(t)

	prevBackoff := amendRetryBackoff
	amendRetryBackoff = 0
	defer func() { amendRetryBackoff = prevBackoff }()

	pushes := 0
	amendPushRaceHook = func() {
		pushes++
		amendWrite(t, seed, "work.txt", strings.Repeat("worker output live\n", pushes+1))
		amendGit(t, seed, "commit", "-am", "worker: live push")
		amendGit(t, seed, "push", "origin", branch)
	}
	defer func() { amendPushRaceHook = nil }()

	before := amendGit(t, origin, "rev-parse", branch)
	err := amendHeadWithAttributionTrailer(wt, branch, testAttribution(), time.Now().UTC())
	if !errors.Is(err, errAmendDeferred) {
		t.Fatalf("expected errAmendDeferred when branch keeps moving, got: %v", err)
	}
	if pushes < 2 {
		t.Fatalf("expected multiple retry attempts, saw %d", pushes)
	}

	// The worker's latest commit is untouched: no force-push discarded it, and
	// no trailer was rewritten onto it.
	head := amendGit(t, origin, "rev-parse", branch)
	if head == before {
		t.Fatal("test setup error: origin branch never advanced")
	}
	headMsg := amendGit(t, origin, "log", "-1", "--pretty=%B", branch)
	if strings.Contains(headMsg, state.AttributionTrailerKey+":") {
		t.Fatalf("deferred amend must not have rewritten the worker's head:\n%s", headMsg)
	}
	if last := amendGit(t, seed, "rev-parse", branch); head != last {
		t.Fatalf("origin head %s != worker's last push %s — orchestrator clobbered a worker commit", head, last)
	}
}

// A second amend on a head that already carries the trailer is a no-op: no push,
// no duplicate trailer.
func TestAmendHead_IdempotentNoDuplicateTrailer(t *testing.T) {
	origin, wt, _, branch := setupAmendRepo(t)

	now := time.Date(2026, 7, 10, 4, 5, 0, 0, time.UTC)
	if err := amendHeadWithAttributionTrailer(wt, branch, testAttribution(), now); err != nil {
		t.Fatalf("first amend: %v", err)
	}
	firstHead := amendGit(t, origin, "rev-parse", branch)

	if err := amendHeadWithAttributionTrailer(wt, branch, testAttribution(), now); err != nil {
		t.Fatalf("second amend (should be no-op): %v", err)
	}
	secondHead := amendGit(t, origin, "rev-parse", branch)
	if firstHead != secondHead {
		t.Fatalf("idempotent amend re-pushed: %s -> %s", firstHead, secondHead)
	}
	headMsg := amendGit(t, origin, "log", "-1", "--pretty=%B", branch)
	if n := trailerCount(headMsg); n != 1 {
		t.Fatalf("trailer appears %d times after repeat amend, want 1:\n%s", n, headMsg)
	}
}

// After a deferral (the branch kept moving on every attempt), the worktree must
// NOT be left sitting on an un-pushed local amend that already carries the
// trailer. If it were, the next cycle's AppendAttributionTrailer would see the
// local trailer and no-op before any fetch/push, so the remote — which advanced
// during the race — would permanently miss attribution despite the deferral
// promising a retry. This drives the deferral, then a quiet second cycle, and
// asserts the trailer actually lands on the remote (#858).
func TestAmendHead_DeferDoesNotPoisonNextCycle(t *testing.T) {
	origin, wt, seed, branch := setupAmendRepo(t)

	prevBackoff := amendRetryBackoff
	amendRetryBackoff = 0
	defer func() { amendRetryBackoff = prevBackoff }()

	// First cycle: the branch advances on every push attempt, forcing a defer.
	pushes := 0
	amendPushRaceHook = func() {
		pushes++
		amendWrite(t, seed, "work.txt", strings.Repeat("live churn\n", pushes+1))
		amendGit(t, seed, "commit", "-am", "worker: live push")
		amendGit(t, seed, "push", "origin", branch)
	}

	if err := amendHeadWithAttributionTrailer(wt, branch, testAttribution(), time.Now().UTC()); !errors.Is(err, errAmendDeferred) {
		t.Fatalf("expected errAmendDeferred while branch keeps moving, got: %v", err)
	}
	if pushes < 2 {
		t.Fatalf("expected multiple retry attempts, saw %d", pushes)
	}

	// The worktree must not be left carrying an un-pushed trailer: HEAD must be a
	// commit whose message has no attribution trailer, or the next cycle no-ops.
	localMsg := amendGit(t, wt, "log", "-1", "--pretty=%B")
	if strings.Contains(localMsg, state.AttributionTrailerKey+":") {
		t.Fatalf("deferred amend left an un-pushed local trailer (poisons next cycle):\n%s", localMsg)
	}

	// Second cycle: the branch is now quiet. The amend must land the trailer on
	// the remote exactly once — proving the first deferral did not poison this.
	amendPushRaceHook = nil
	if err := amendHeadWithAttributionTrailer(wt, branch, testAttribution(), time.Now().UTC()); err != nil {
		t.Fatalf("quiet second cycle should land the trailer, got: %v", err)
	}

	headMsg := amendGit(t, origin, "log", "-1", "--pretty=%B", branch)
	if !strings.Contains(headMsg, state.AttributionTrailerKey+":") {
		t.Fatalf("remote head missing attribution trailer after quiet retry:\n%s", headMsg)
	}
	if n := trailerCount(headMsg); n != 1 {
		t.Fatalf("trailer appears %d times, want exactly 1:\n%s", n, headMsg)
	}
	// The worker's latest concurrent commit is what got reworded — nothing lost.
	if last := amendGit(t, seed, "rev-parse", branch); amendGit(t, origin, "rev-parse", branch) == last {
		t.Fatal("expected the remote head to be the reworded worker commit, not the raw worker push")
	}
	work := amendGit(t, origin, "show", branch+":work.txt")
	if !strings.Contains(work, "live churn") {
		t.Fatalf("worker's concurrent work.txt was discarded: %q", work)
	}
}

// If the new remote head does NOT descend from the commit we were attributing
// (a real divergence, e.g. an unpushed local commit), the amend must defer
// rather than reset --hard and drop a commit that is not ours.
func TestAmendHead_DivergedHeadDefersWithoutDiscard(t *testing.T) {
	origin, wt, seed, branch := setupAmendRepo(t)

	prevBackoff := amendRetryBackoff
	amendRetryBackoff = 0
	defer func() { amendRetryBackoff = prevBackoff }()

	// The worktree has a local commit that was never pushed to origin.
	amendWrite(t, wt, "local.txt", "unpushed worker work\n")
	amendGit(t, wt, "add", "local.txt")
	amendGit(t, wt, "commit", "-m", "worker: unpushed local commit")

	// Meanwhile origin advances to a sibling commit (diverges from our head).
	amendWrite(t, seed, "work.txt", "diverging remote push\n")
	amendGit(t, seed, "commit", "-am", "operator: diverging push")
	amendGit(t, seed, "push", "origin", branch)

	remoteHeadBefore := amendGit(t, origin, "rev-parse", branch)

	err := amendHeadWithAttributionTrailer(wt, branch, testAttribution(), time.Now().UTC())
	if !errors.Is(err, errAmendDeferred) {
		t.Fatalf("expected errAmendDeferred on divergence, got: %v", err)
	}

	// Origin is untouched, and our unpushed local work still exists locally.
	if after := amendGit(t, origin, "rev-parse", branch); after != remoteHeadBefore {
		t.Fatalf("origin head moved on a diverged defer: %s -> %s", remoteHeadBefore, after)
	}
	if got := amendGit(t, wt, "show", "HEAD:local.txt"); !strings.Contains(got, "unpushed") {
		t.Fatalf("local unpushed commit was discarded: %q", got)
	}
}
