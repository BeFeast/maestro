package orchestrator

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

// Regression coverage for #1247: a forge credential that may not merge must
// not be asked to merge the same head every cycle. The merge call goes through
// the real github → forgejo transport against a fake Forgejo merge endpoint, so
// the call counts below are HTTP requests, and the classification under test is
// the one production uses.

const (
	mergeDeniedRepo     = "BeFeast/example"
	mergeDeniedTokenEnv = "MAESTRO_TEST_MERGE_DENIED_TOKEN"
	mergeDeniedToken    = "native-worker-token"
	mergeDeniedHeadA    = "0123456789abcdef0123456789abcdef01234567"
	mergeDeniedHeadB    = "fedcba9876543210fedcba9876543210fedcba98"

	forgejoActorDeniedBody = `{"message":"User not allowed to merge PR","url":""}`
)

// fakeForgejoMerge is the merge endpoint of a fake Forgejo instance. It
// records the head_commit_id of every merge request and answers with the
// configured status/body.
type fakeForgejoMerge struct {
	mu     sync.Mutex
	status int
	body   string
	heads  []string
}

func (f *fakeForgejoMerge) answer(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeForgejoMerge) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.heads...)
}

func (f *fakeForgejoMerge) serve(t *testing.T, prNumber int) *httptest.Server {
	t.Helper()
	want := "/api/v1/repos/" + mergeDeniedRepo + "/pulls/" + strconv.Itoa(prNumber) + "/merge"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != want {
			t.Errorf("unexpected forge request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var payload struct {
			HeadCommitID string `json:"head_commit_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		f.mu.Lock()
		f.heads = append(f.heads, payload.HeadCommitID)
		status, body := f.status, f.body
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type mergeDeniedHarness struct {
	o        *Orchestrator
	s        *state.State
	sess     *state.Session
	forge    *fakeForgejoMerge
	notifier *notify.Notifier
	logs     *strings.Builder
	head     string
	merged   bool
	open     bool
}

// newMergeDeniedHarness wires a green, review-cleared PR #20 on a Forgejo
// project whose merge credential is mergeDeniedToken. Its forge answers the
// actor refusal until told otherwise.
func newMergeDeniedHarness(t *testing.T, forge config.ForgeConfig) *mergeDeniedHarness {
	t.Helper()
	t.Setenv(mergeDeniedTokenEnv, mergeDeniedToken)
	pr := github.PR{Number: 20, HeadRefName: "feat/native"}
	h := &mergeDeniedHarness{
		forge: &fakeForgejoMerge{status: http.StatusMethodNotAllowed, body: forgejoActorDeniedBody},
		head:  mergeDeniedHeadA,
		open:  true,
	}
	if forge.IsForgejo() {
		forge.BaseURL = h.forge.serve(t, pr.Number).URL
		forge.TokenEnv = mergeDeniedTokenEnv
	}
	cfg := &config.Config{Repo: mergeDeniedRepo, MergeStrategy: "parallel", Forge: forge}
	o, _ := newMergeTestOrchestrator(cfg, []github.PR{pr})
	h.notifier = notify.NewWithToken("", "123", "", "")
	h.notifier.SetDigestMode(true)
	o.notifier = h.notifier
	o.ghMergePRFn = nil
	client := github.New(cfg.Repo, cfg.Forge)
	o.ghMergePRAtHeadFn = func(prNumber int, head string) error {
		err := client.MergePRAtHead(prNumber, head)
		if err == nil {
			h.merged, h.open = true, false
		}
		return err
	}
	o.listOpenPRsFn = func() ([]github.PR, error) {
		if !h.open {
			return nil, nil
		}
		return []github.PR{pr}, nil
	}
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{HeadSHA: h.head, Verdict: "success", Fingerprint: strings.Repeat("1", 16), Complete: true}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return h.head, nil }
	o.ghPRUnresolvedThreadsFn = func(int) (string, []github.ReviewThread, error) { return h.head, nil, nil }
	o.isPRMergedFn = func(int) (bool, error) { return h.merged, nil }
	h.o = o
	h.s = makeTestState([]github.PR{pr})
	h.sess = h.s.Sessions["slot-0"]
	h.logs = captureMergeDeniedLog(t)
	return h
}

func forgejoForge() config.ForgeConfig { return config.ForgeConfig{Kind: config.ForgeKindForgejo} }

func captureMergeDeniedLog(t *testing.T) *strings.Builder {
	t.Helper()
	var logs strings.Builder
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &logs
}

func (h *mergeDeniedHarness) cycles(n int) {
	for i := 0; i < n; i++ {
		h.o.autoMergePRs(h.s)
	}
}

// journalLines counts the orchestrator's own merge-denied journal lines (the
// notifier's digest-buffer echo of the notification is not a journal line).
func (h *mergeDeniedHarness) journalLines() int {
	n := 0
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, "[orch] ") && strings.Contains(line, mergeDeniedSummary) {
			n++
		}
	}
	return n
}

func (h *mergeDeniedHarness) assertHeld(t *testing.T, gate string) {
	t.Helper()
	if h.sess.OperatorGateName != gate {
		t.Fatalf("operator gate = %q, want %q", h.sess.OperatorGateName, gate)
	}
	attention := state.SessionAttentionFor(h.sess, nil)
	if !attention.NeedsAttention || !strings.Contains(attention.Reason, gate) || !strings.Contains(attention.NextAction, "allowed to merge") {
		t.Fatalf("attention = %+v, want a merge-denied attention item naming %q", attention, gate)
	}
}

func (h *mergeDeniedHarness) assertNotHeld(t *testing.T) {
	t.Helper()
	if h.sess.OperatorGateName != "" || h.sess.MergeDeniedHeadSHA != "" || h.sess.MergeDeniedActor != "" || h.sess.MergeDeniedSurfaced != "" {
		t.Fatalf("merge-denied state not clear: gate=%q head=%q actor=%q surfaced=%q",
			h.sess.OperatorGateName, h.sess.MergeDeniedHeadSHA, h.sess.MergeDeniedActor, h.sess.MergeDeniedSurfaced)
	}
}

func TestAutoMergePRs_ForgeRefusedActorLatchesHeadAfterOneCall(t *testing.T) {
	h := newMergeDeniedHarness(t, forgejoForge())
	h.sess.Status = state.StatusRetryExhausted
	h.sess.LastNotifiedStatus = "review_retry_exhausted"
	h.sess.RetryCount = 2

	h.cycles(5)

	if got := h.forge.calls(); len(got) != 1 || got[0] != mergeDeniedHeadA {
		t.Fatalf("forge merge calls = %v, want exactly one call at head %s", got, mergeDeniedHeadA)
	}
	if h.sess.MergeDeniedHeadSHA != mergeDeniedHeadA || h.sess.MergeDeniedActor == "" {
		t.Fatalf("latch = head %q actor %q, want head %s and a credential fingerprint", h.sess.MergeDeniedHeadSHA, h.sess.MergeDeniedActor, mergeDeniedHeadA)
	}
	if strings.Contains(h.sess.MergeDeniedActor, mergeDeniedToken) {
		t.Fatal("the latch must not persist the credential itself")
	}
	if got := h.journalLines(); got != 1 {
		t.Fatalf("journal lines = %d, want exactly one %q\n%s", got, mergeDeniedSummary, h.logs.String())
	}
	if got := h.notifier.Buffered(); got != 1 {
		t.Fatalf("notifications = %d, want exactly one", got)
	}
	h.assertHeld(t, "merge-denied:forge-actor")
	// The hold is an attention item only: no status, retry or #565 marker
	// change, and no generic merge-failure notification.
	if h.sess.Status != state.StatusRetryExhausted || h.sess.RetryCount != 2 || h.sess.NextRetryAt != nil || h.sess.LastNotifiedStatus != "review_retry_exhausted" {
		t.Fatalf("session mutated beyond the hold: status=%s retries=%d next=%v last=%q", h.sess.Status, h.sess.RetryCount, h.sess.NextRetryAt, h.sess.LastNotifiedStatus)
	}
	if h.merged || h.sess.PRMerged {
		t.Fatal("a refused merge must not be recorded as merged")
	}

	// The latch survives a JSON round trip (state.json persistence).
	b, err := json.Marshal(h.sess)
	if err != nil {
		t.Fatal(err)
	}
	var restored state.Session
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.MergeDeniedHeadSHA != h.sess.MergeDeniedHeadSHA || restored.MergeDeniedActor != h.sess.MergeDeniedActor || restored.MergeDeniedSurfaced != h.sess.MergeDeniedSurfaced {
		t.Fatalf("latch lost in JSON round trip: %+v", restored)
	}
}

func TestAutoMergePRs_MergeDeniedLatchReleasesOnceWhenHeadMoves(t *testing.T) {
	h := newMergeDeniedHarness(t, forgejoForge())

	h.cycles(3)
	if got := h.forge.calls(); len(got) != 1 {
		t.Fatalf("forge merge calls before the push = %v, want one", got)
	}

	// A push moves the head: exactly one fresh attempt at the new head, and
	// the forge's renewed refusal latches the new head.
	h.head = mergeDeniedHeadB
	h.cycles(3)
	if got := h.forge.calls(); len(got) != 2 || got[1] != mergeDeniedHeadB {
		t.Fatalf("forge merge calls = %v, want one more call at head %s", got, mergeDeniedHeadB)
	}
	if h.sess.MergeDeniedHeadSHA != mergeDeniedHeadB {
		t.Fatalf("latched head = %q, want %s", h.sess.MergeDeniedHeadSHA, mergeDeniedHeadB)
	}
	if got := h.journalLines(); got != 2 {
		t.Fatalf("journal lines = %d, want one per refused head\n%s", got, h.logs.String())
	}
	h.assertHeld(t, "merge-denied:forge-actor")

	// The forge now accepts the merge (for example a whitelist change) and a
	// new head arrives: one attempt, merged, the hold is gone.
	h.forge.answer(http.StatusOK, ``)
	h.head = mergeDeniedHeadA
	h.cycles(2)
	if got := h.forge.calls(); len(got) != 3 || got[2] != mergeDeniedHeadA {
		t.Fatalf("forge merge calls = %v, want a third call at head %s", got, mergeDeniedHeadA)
	}
	if !h.merged || !h.sess.PRMerged {
		t.Fatalf("PR not merged after the forge accepted: merged=%v session=%+v", h.merged, h.sess)
	}
	h.assertNotHeld(t)
}

func TestAutoMergePRs_MergeDeniedLatchReleasesWhenCredentialChanges(t *testing.T) {
	h := newMergeDeniedHarness(t, forgejoForge())

	h.cycles(2)
	if got := h.forge.calls(); len(got) != 1 {
		t.Fatalf("forge merge calls = %v, want one", got)
	}

	// The project is rebound to a merge-capable credential: same head, one
	// fresh attempt with the new credential.
	t.Setenv(mergeDeniedTokenEnv, "operator-token")
	h.forge.answer(http.StatusOK, ``)
	h.cycles(2)
	if got := h.forge.calls(); len(got) != 2 || got[1] != mergeDeniedHeadA {
		t.Fatalf("forge merge calls = %v, want one more call at the same head", got)
	}
	if !h.sess.PRMerged {
		t.Fatal("PR not merged with the new credential")
	}
	h.assertNotHeld(t)
}

func TestAutoMergePRs_AlreadyMergedPRIsNotLatchedAndConverges(t *testing.T) {
	h := newMergeDeniedHarness(t, forgejoForge())
	// An operator merged the PR between the open-PR read and the merge call:
	// the forge still refuses this actor, but the PR is merged.
	h.o.ghMergePRAtHeadFn = func(prNumber int, head string) error {
		err := github.New(h.o.cfg.Repo, h.o.cfg.Forge).MergePRAtHead(prNumber, head)
		h.merged = true
		return err
	}

	h.cycles(1)
	if len(h.forge.calls()) != 1 {
		t.Fatalf("forge merge calls = %v, want one", h.forge.calls())
	}
	h.assertNotHeld(t)
	if h.journalLines() != 0 || h.notifier.Buffered() != 0 {
		t.Fatalf("an already-merged PR must not raise a merge-denied hold (journal=%d notifications=%d)\n%s", h.journalLines(), h.notifier.Buffered(), h.logs.String())
	}

	// Next cycle: the PR has left the open set and the existing merged-PR
	// reconciliation converges the session.
	h.open = false
	h.cycles(1)
	if len(h.forge.calls()) != 1 {
		t.Fatalf("forge merge calls = %v, want no further call", h.forge.calls())
	}
	if h.sess.Status != state.StatusDone && h.sess.Status != state.StatusCodeLanded {
		t.Fatalf("session status = %s, want converged as merged", h.sess.Status)
	}
	h.assertNotHeld(t)
}

func TestAutoMergePRs_OperatorMergeReleasesMergeDeniedHold(t *testing.T) {
	h := newMergeDeniedHarness(t, forgejoForge())
	h.cycles(2)
	h.assertHeld(t, "merge-denied:forge-actor")

	// The operator merges the PR with their own credential.
	h.merged, h.open = true, false
	h.cycles(1)
	if len(h.forge.calls()) != 1 {
		t.Fatalf("forge merge calls = %v, want only the original refused call", h.forge.calls())
	}
	if h.sess.Status != state.StatusDone && h.sess.Status != state.StatusCodeLanded {
		t.Fatalf("session status = %s, want converged as merged", h.sess.Status)
	}
	h.assertNotHeld(t)
	if attention := state.SessionAttentionFor(h.sess, nil); strings.Contains(attention.Reason, mergeDeniedGatePrefix) {
		t.Fatalf("merge-denied attention outlived the merged PR: %+v", attention)
	}
}

func boundEvidence(login, repo, token string, mergeDenied bool) func(aiexecution.Policy) (aiexecution.NativeForgejoAuthorizationReport, error) {
	return func(aiexecution.Policy) (aiexecution.NativeForgejoAuthorizationReport, error) {
		return aiexecution.NativeForgejoAuthorizationReport{Authorizations: []aiexecution.NativeForgejoAuthorization{{
			ProfileKey:       "worker",
			Repository:       repo,
			WorkerLogin:      login,
			CredentialSHA256: aiexecution.NativeForgejoCredentialSHA256(token),
			MergeDenied:      mergeDenied,
		}}}, nil
	}
}

func withManifest(h *mergeDeniedHarness) {
	h.o.cfg.AIExecution = aiexecution.Policy{ManifestPath: "/etc/maestro/test-manifest.json", ManifestSHA256: strings.Repeat("a", 64)}
}

func TestAutoMergePRs_EvidenceMergeDeniedMakesZeroMergeCalls(t *testing.T) {
	h := newMergeDeniedHarness(t, forgejoForge())
	withManifest(h)
	h.o.nativeForgejoAuthorizationsFn = boundEvidence("native-worker", mergeDeniedRepo, mergeDeniedToken, true)

	h.cycles(4)

	if got := h.forge.calls(); len(got) != 0 {
		t.Fatalf("forge merge calls = %v, want zero for an evidence-declared merge-denied credential", got)
	}
	if got := h.journalLines(); got != 1 {
		t.Fatalf("journal lines = %d, want exactly one %q\n%s", got, mergeDeniedSummary, h.logs.String())
	}
	if got := h.notifier.Buffered(); got != 1 {
		t.Fatalf("notifications = %d, want exactly one", got)
	}
	h.assertHeld(t, "merge-denied:native-worker")
	if h.sess.Status != state.StatusPROpen || h.sess.RetryCount != 0 || h.sess.MergeDeniedHeadSHA != "" {
		t.Fatalf("session mutated beyond the hold: %+v", h.sess)
	}
	// A head move does not matter: the evidence speaks for the credential.
	h.head = mergeDeniedHeadB
	h.cycles(2)
	if got := h.forge.calls(); len(got) != 0 || h.journalLines() != 1 {
		t.Fatalf("calls = %v journal = %d after a head move, want zero calls and no new line", got, h.journalLines())
	}

	// The evidence no longer declares the credential merge-denied: the hold
	// goes and today's merge path resumes.
	h.o.nativeForgejoAuthorizationsFn = boundEvidence("native-worker", mergeDeniedRepo, mergeDeniedToken, false)
	h.forge.answer(http.StatusOK, ``)
	h.cycles(1)
	if got := h.forge.calls(); len(got) != 1 || !h.sess.PRMerged {
		t.Fatalf("calls = %v merged = %v, want the merge to proceed once the evidence clears", got, h.sess.PRMerged)
	}
	h.assertNotHeld(t)
}

// Evidence that is missing, unreadable, or not about this exact credential and
// repository must leave auto-merge exactly as it was: the merge is attempted.
func TestAutoMergePRs_MissingOrUnboundEvidenceKeepsTodayBehaviour(t *testing.T) {
	cases := []struct {
		name     string
		manifest bool
		read     func(aiexecution.Policy) (aiexecution.NativeForgejoAuthorizationReport, error)
	}{
		{"no ai_execution manifest", false, func(aiexecution.Policy) (aiexecution.NativeForgejoAuthorizationReport, error) {
			t.Fatal("evidence must not be read without a manifest")
			return aiexecution.NativeForgejoAuthorizationReport{}, nil
		}},
		{"manifest unreadable", true, func(aiexecution.Policy) (aiexecution.NativeForgejoAuthorizationReport, error) {
			return aiexecution.NativeForgejoAuthorizationReport{}, aiexecution.Held("manifest_drift")
		}},
		{"profiles unverifiable", true, func(aiexecution.Policy) (aiexecution.NativeForgejoAuthorizationReport, error) {
			return aiexecution.NativeForgejoAuthorizationReport{Unverified: []string{"worker"}}, nil
		}},
		{"denial for another credential", true, boundEvidence("native-worker", mergeDeniedRepo, "some-other-token", true)},
		{"denial for another repository", true, boundEvidence("native-worker", "BeFeast/elsewhere", mergeDeniedToken, true)},
		{"credential allowed to merge", true, boundEvidence("native-worker", mergeDeniedRepo, mergeDeniedToken, false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newMergeDeniedHarness(t, forgejoForge())
			if tc.manifest {
				withManifest(h)
			}
			h.o.nativeForgejoAuthorizationsFn = tc.read
			h.forge.answer(http.StatusOK, ``)

			h.cycles(1)

			if got := h.forge.calls(); len(got) != 1 || got[0] != mergeDeniedHeadA || !h.sess.PRMerged {
				t.Fatalf("calls = %v merged = %v, want the unchanged single merge at head %s", got, h.sess.PRMerged, mergeDeniedHeadA)
			}
			if h.journalLines() != 0 || h.sess.OperatorGateName != "" {
				t.Fatalf("no merge-denied hold may be raised without bound evidence (journal=%d gate=%q)", h.journalLines(), h.sess.OperatorGateName)
			}
		})
	}
}

// Every other forge refusal keeps today's behaviour: the merge is asked again
// on every cycle and no merge-denied hold is raised.
func TestAutoMergePRs_OtherForgeRefusalsKeepRetrying(t *testing.T) {
	for _, body := range []string{
		`{"message":"not allowed to merge [reason: Does not have enough approvals]","url":""}`,
		`{"message":"not allowed to merge [reason: The head branch is behind the base branch]","url":""}`,
		`{"message":"Work in progress PRs cannot be merged","url":""}`,
		`{"message":"Please try again later","url":""}`,
		`{"message":"","url":""}`,
	} {
		t.Run(body, func(t *testing.T) {
			h := newMergeDeniedHarness(t, forgejoForge())
			h.forge.answer(http.StatusMethodNotAllowed, body)
			h.o.ghPRMergeStatusFn = func(int) (string, string, error) { return "MERGEABLE", "blocked", nil }

			h.cycles(3)

			if got := h.forge.calls(); len(got) != 3 {
				t.Fatalf("forge merge calls = %v, want one per cycle as today", got)
			}
			if h.journalLines() != 0 || h.sess.MergeDeniedHeadSHA != "" || strings.HasPrefix(h.sess.OperatorGateName, mergeDeniedGatePrefix) {
				t.Fatalf("a non-actor refusal must not raise a merge-denied hold (journal=%d latch=%q gate=%q)", h.journalLines(), h.sess.MergeDeniedHeadSHA, h.sess.OperatorGateName)
			}
		})
	}
}

// A GitHub project never consults containment evidence and its merge path is
// unchanged, including for a refusal that reads like an actor denial.
func TestAutoMergePRs_GitHubProjectUnaffected(t *testing.T) {
	h := newMergeDeniedHarness(t, config.ForgeConfig{})
	withManifest(h)
	h.o.nativeForgejoAuthorizationsFn = func(aiexecution.Policy) (aiexecution.NativeForgejoAuthorizationReport, error) {
		t.Fatal("a GitHub project must not read forgejo-authorization evidence")
		return aiexecution.NativeForgejoAuthorizationReport{}, nil
	}
	var calls []string
	h.o.ghMergePRAtHeadFn = func(prNumber int, head string) error {
		calls = append(calls, head)
		return errors.New("gh pr merge 20: GraphQL: User not allowed to merge PR")
	}
	h.o.ghPRMergeStatusFn = func(int) (string, string, error) { return "MERGEABLE", "blocked", nil }

	h.cycles(3)

	if len(calls) != 3 {
		t.Fatalf("merge calls = %v, want one per cycle as today", calls)
	}
	if h.journalLines() != 0 || h.sess.MergeDeniedHeadSHA != "" || h.sess.OperatorGateName != "" {
		t.Fatalf("a GitHub project must not raise a merge-denied hold (journal=%d latch=%q gate=%q)", h.journalLines(), h.sess.MergeDeniedHeadSHA, h.sess.OperatorGateName)
	}

	// And a plain successful merge is untouched.
	h.o.ghMergePRAtHeadFn = func(prNumber int, head string) error {
		calls = append(calls, head)
		h.merged, h.open = true, false
		return nil
	}
	h.cycles(1)
	if len(calls) != 4 || !h.sess.PRMerged {
		t.Fatalf("calls = %v merged = %v, want one successful merge", calls, h.sess.PRMerged)
	}
}

// In sequential mode a latched PR does not occupy the single merge slot: a
// younger ready PR still merges.
func TestAutoMergePRs_MergeDeniedLatchDoesNotHoldSequentialSlot(t *testing.T) {
	h := newMergeDeniedHarness(t, forgejoForge())
	h.o.cfg.MergeStrategy = "sequential"
	h.o.ghPRMergeStatusFn = func(int) (string, string, error) { return "MERGEABLE", "clean", nil }
	h.sess.MergeDeniedHeadSHA = mergeDeniedHeadA
	h.sess.MergeDeniedActor = h.o.mergeActorFingerprint()
	h.sess.MergeDeniedSurfaced = mergeDeniedHeadKey(mergeDeniedHeadA, h.sess.MergeDeniedActor)

	older := github.PR{Number: 20, HeadRefName: "feat/native"}
	younger := github.PR{Number: 21, HeadRefName: "feat/other"}
	h.o.listOpenPRsFn = func() ([]github.PR, error) { return []github.PR{older, younger}, nil }
	h.s.Sessions["slot-1"] = &state.Session{IssueNumber: 101, Branch: younger.HeadRefName, Status: state.StatusPROpen, PRNumber: younger.Number}
	var merged []int
	h.o.ghMergePRAtHeadFn = func(prNumber int, head string) error {
		merged = append(merged, prNumber)
		return nil
	}

	h.cycles(1)

	if len(merged) != 1 || merged[0] != younger.Number {
		t.Fatalf("merged = %v, want only the younger PR #%d", merged, younger.Number)
	}
	h.assertHeld(t, "merge-denied:forge-actor")
	if h.journalLines() != 0 {
		t.Fatalf("an already-surfaced latch must not journal again\n%s", h.logs.String())
	}
}
