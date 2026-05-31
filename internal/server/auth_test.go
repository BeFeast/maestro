package server

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// --- credential plumbing -----------------------------------------------------

func TestServerAuthConfig_TokenComesFromEnv(t *testing.T) {
	const envVar = "MAESTRO_TEST_TOKEN_487"
	const want = "from-secret-manager"
	t.Setenv(envVar, want)
	cfg := config.ServerAuthConfig{TokenEnv: envVar}
	if got := cfg.Token(); got != want {
		t.Fatalf("Token() = %q, want %q", got, want)
	}
}

func TestServerAuthConfig_TokenEmptyWhenEnvUnset(t *testing.T) {
	const envVar = "MAESTRO_TEST_TOKEN_487_MISSING"
	os.Unsetenv(envVar)
	cfg := config.ServerAuthConfig{TokenEnv: envVar}
	if got := cfg.Token(); got != "" {
		t.Fatalf("Token() = %q, want empty (env unset)", got)
	}
}

func TestServerAuthConfig_TokenEnvBlankDisablesAuth(t *testing.T) {
	cfg := config.ServerAuthConfig{TokenEnv: ""}
	if got := cfg.Token(); got != "" {
		t.Fatalf("Token() = %q, want empty (TokenEnv empty)", got)
	}
	if newAuthChecker(cfg).Required() {
		t.Fatalf("Required() = true when TokenEnv is empty; must be false")
	}
}

// --- 401 on every mutating POST ----------------------------------------------

func TestSafeAction_NoCredentialReturns401(t *testing.T) {
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)
	srv.SetAuthForTest("the-real-token", "alice")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":42}`))
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (#487 spec: 'every mutating POST without a valid credential returns 401')", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want Bearer challenge", got)
	}
}

func TestSafeAction_WrongTokenReturns401(t *testing.T) {
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)
	srv.SetAuthForTest("the-real-token", "alice")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":42}`))
	req.Header.Set("Authorization", "Bearer the-wrong-token")
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (wrong token)", w.Code)
	}
}

func TestSafeAction_ValidBearerExecutes(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)
	srv.SetAuthForTest("good-token", "alice")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":42}`))
	req.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(gh.addLabelCalls) != 1 {
		t.Fatalf("gh not called with valid auth: %+v", gh.addLabelCalls)
	}
}

func TestSafeAction_ValidBasicAuthExecutes(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)
	srv.SetAuthForTest("good-token", "alice")

	encoded := base64.StdEncoding.EncodeToString([]byte("operator:good-token"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":7}`))
	req.Header.Set("Authorization", "Basic "+encoded)
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(gh.addLabelCalls) != 1 {
		t.Fatalf("gh not called with valid Basic auth: %+v", gh.addLabelCalls)
	}
}

// 401 fires BEFORE read-only / 403 — an unauthenticated attacker on a
// read-only deployment must NOT be able to enumerate verbs via 403 mapping.
func TestSafeAction_AuthFiresBeforeReadOnly(t *testing.T) {
	cfg := newSafeActionTestCfg()
	cfg.Server.ReadOnly = true
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)
	srv.SetAuthForTest("good-token", "alice")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":42}`))
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (auth must fire before read-only/403)", w.Code)
	}
}

func TestRefresh_NoCredentialReturns401(t *testing.T) {
	srv := New(newSafeActionTestCfg(), make(chan struct{}, 1))
	srv.SetAuthForTest("good-token", "alice")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	w := httptest.NewRecorder()
	srv.handleRefresh(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// --- approve/reject endpoints — the cautious gate bypass we're closing ------

func TestApprove_NoCredentialReturns401(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	srv.SetAuthForTest("good-token", "alice")
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 12})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/approvals/"+a.ID+"/approve",
		bytes.NewBufferString(`{"actor":"attacker","reason":"please"}`))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (approve IS the human-authorization step; #487 fm#4)", w.Code)
	}
	// State on disk must be UNCHANGED — auth-fail before mutation.
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusPending {
		t.Fatalf("disk status = %q, want still pending (auth-fail must not approve)", st.Approvals[0].Status)
	}
}

func TestReject_NoCredentialReturns401(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	srv.SetAuthForTest("good-token", "alice")
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 12})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/approvals/"+a.ID+"/reject",
		bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusPending {
		t.Fatalf("disk status = %q, want still pending", st.Approvals[0].Status)
	}
}

func TestApprove_ValidBearerExecutes(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	srv.SetAuthForTest("good-token", "alice")
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 12})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/approvals/"+a.ID+"/approve",
		bytes.NewBufferString(`{"reason":"green"}`))
	req.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusApproved {
		t.Fatalf("disk status = %q, want approved", st.Approvals[0].Status)
	}
	// The audit trail's most recent entry MUST credit the authenticated
	// identity, not the request body's actor (which the test omitted, but
	// a malicious client could otherwise set).
	audits := st.Approvals[0].Audit
	if len(audits) == 0 {
		t.Fatalf("approval has no audit entries")
	}
	if audits[len(audits)-1].Actor != "alice" {
		t.Fatalf("audit actor = %q, want %q (authenticated actor must populate audit, not body)", audits[len(audits)-1].Actor, "alice")
	}
}

// --- authenticated actor overrides the request body ------------------------

func TestSafeAction_AuthenticatedActorOverridesBodyActor(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetAuthForTest("good-token", "alice")
	var recordedActor string
	srv.SetActionDeps(gh, func(actor, action, target, reason string) error {
		recordedActor = actor
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":42,"actor":"attacker-claim"}`))
	req.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if recordedActor != "alice" {
		t.Fatalf("audit actor = %q, want %q — the body's actor field MUST NOT be trusted when auth is configured (#487 fm#9)", recordedActor, "alice")
	}
}

func TestApprovalEnqueue_AuthenticatedActorOverridesBodyActor(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetAuthForTest("good-token", "alice")
	var recordedActor string
	srv.SetActionDeps(&fakeActionGH{}, func(actor, action, target, reason string) error {
		recordedActor = actor
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"merge_pr","pr_number":42,"actor":"attacker-claim"}`))
	req.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if recordedActor != "alice" {
		t.Fatalf("audit actor = %q, want %q", recordedActor, "alice")
	}
	st, _ := state.Load(dir)
	if len(st.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(st.Approvals))
	}
}

// --- defense in depth: cautious gate still fires for authenticated callers --

func TestApprovalEnqueue_AuthenticatedCallerStillEnqueues_DoesNotExecute(t *testing.T) {
	// Even with a valid bearer token, merge_pr / close_issue /
	// delete_worktree / change_global_config MUST still go through the
	// cautious-gate (enqueue as pending Approval, 202). The supervisor's
	// approver/executor is the second gate; defense in depth.
	for _, tc := range []struct {
		actionID string
		payload  string
	}{
		{"merge_pr", `{"action_id":"merge_pr","pr_number":1}`},
		{"close_issue", `{"action_id":"close_issue","issue_number":2}`},
		{"delete_worktree", `{"action_id":"delete_worktree","slot":"sup-3"}`},
		{"change_global_config", `{"action_id":"change_global_config","reason":"swap backend"}`},
	} {
		cfg, dir := approvalEnqueueCfg(t)
		srv := New(cfg, nil)
		srv.SetActionDeps(&fakeActionGH{}, nil)
		srv.SetAuthForTest("good-token", "alice")

		req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
			bytes.NewBufferString(tc.payload))
		req.Header.Set("Authorization", "Bearer good-token")
		w := httptest.NewRecorder()
		srv.handleAction(w, req)

		if w.Code != http.StatusAccepted {
			t.Errorf("verb %s: status = %d, want 202 (cautious gate must NOT execute even for authed caller)", tc.actionID, w.Code)
			continue
		}
		st, _ := state.Load(dir)
		if len(st.Approvals) != 1 {
			t.Errorf("verb %s: approvals on disk = %d, want 1 pending", tc.actionID, len(st.Approvals))
			continue
		}
		if st.Approvals[0].Status != state.ApprovalStatusPending {
			t.Errorf("verb %s: status = %q, want pending", tc.actionID, st.Approvals[0].Status)
		}
	}
}

// --- backward-compat: auth disabled by default keeps existing behaviour -----

func TestSafeAction_AuthDisabledKeepsOldBehaviour(t *testing.T) {
	// With no token configured, an unauthenticated POST proceeds as it did
	// before #487 — preserves test fixtures and the initial rollout path
	// where operators have not yet wired Infisical.
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)
	// no SetAuthForTest → auth disabled

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":42}`))
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (auth disabled = backward-compat)", w.Code)
	}
	if len(gh.addLabelCalls) != 1 {
		t.Fatalf("gh not called: %+v", gh.addLabelCalls)
	}
}

// --- fleet ------------------------------------------------------------------

func TestFleetAction_NoCredentialReturns401(t *testing.T) {
	stateDir := t.TempDir()
	srv := NewFleet([]FleetProject{
		NewFleetProject("AuthTest", "/tmp/auth-target.yaml", "", &config.Config{
			Repo: "owner/auth", StateDir: stateDir, MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, false)
	srv.SetAuthForTest("good-token", "alice")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":42,"project":"AuthTest"}`))
	w := httptest.NewRecorder()
	srv.handleFleetAction(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestFleetAuditLog_NoCredentialReturns401(t *testing.T) {
	stateDir := t.TempDir()
	srv := NewFleet([]FleetProject{
		NewFleetProject("AuthTest", "/tmp/auth-target.yaml", "", &config.Config{
			Repo: "owner/auth", StateDir: stateDir, MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)
	srv.SetAuthForTest("good-token", "alice")

	body := strings.NewReader(`{"actor":"attacker","action":"forensic_noise","target":"x","reason":"y","project":"AuthTest"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/audit/log", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleFleetAuditLog(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (audit log forensic integrity)", w.Code)
	}
}

func TestFleetAuditLog_AuthenticatedActorWinsOverBody(t *testing.T) {
	stateDir := t.TempDir()
	srv := NewFleet([]FleetProject{
		NewFleetProject("AuthTest", "/tmp/auth-target.yaml", "", &config.Config{
			Repo: "owner/auth", StateDir: stateDir, MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)
	srv.SetAuthForTest("good-token", "alice")

	body := strings.NewReader(`{"actor":"attacker-claim","action":"smoke","project":"AuthTest"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/audit/log", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()
	srv.handleFleetAuditLog(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Read the audit log line and confirm actor was overridden.
	data, err := os.ReadFile(stateDir + "/audit-log.jsonl")
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !strings.Contains(string(data), `"actor":"alice"`) {
		t.Fatalf("audit line missing authenticated actor; line=%s", string(data))
	}
	if strings.Contains(string(data), `"actor":"attacker-claim"`) {
		t.Fatalf("audit line trusted attacker-controlled body actor; line=%s", string(data))
	}
}

func TestFleetApproval_NoCredentialReturns401_BeforeProjectLookup(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetAuthForTest("good-token", "alice")

	// Path is well-formed but no project exists. Auth must fire BEFORE the
	// project lookup, so the response is 401 (not 404 / 400) — an
	// unauthenticated probe cannot enumerate fleet topology.
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/fleet/approvals/some-id/approve?project=Missing",
		bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	srv.handleFleetApproval(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (auth must fire before project lookup)", w.Code)
	}
}
