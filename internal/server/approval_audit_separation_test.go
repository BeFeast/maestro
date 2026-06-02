package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// fleetApprovalFixture is a small helper that builds a fleetApprovalState
// for a given lifecycle status without touching disk. Tests use it to
// pin the pending-only / history-only / mixed contracts the M-011 issue
// requires for the approval surfaces.
func fleetApprovalFixture(project, id, status string, age time.Duration, now time.Time) fleetApprovalState {
	return fleetApprovalState{
		ProjectName:  project,
		ID:           id,
		Action:       "approve_merge",
		Status:       status,
		Risk:         "approval_gated",
		Summary:      "Fixture approval " + id + " (" + status + ").",
		DashboardURL: "http://127.0.0.1:8788",
		PRNumber:     1,
		createdAt:    now.Add(-age),
		updatedAt:    now.Add(-age),
	}
}

// --- historicalFleetApprovals filter --------------------------------------

func TestHistoricalFleetApprovalsFiltersPendingOut(t *testing.T) {
	now := time.Now().UTC()
	mixed := []fleetApprovalState{
		fleetApprovalFixture("proj", "pending-1", string(state.ApprovalStatusPending), time.Minute, now),
		fleetApprovalFixture("proj", "stale-1", string(state.ApprovalStatusStale), 2*time.Hour, now),
		fleetApprovalFixture("proj", "approved-1", string(state.ApprovalStatusApproved), 3*time.Hour, now),
		fleetApprovalFixture("proj", "rejected-1", string(state.ApprovalStatusRejected), 4*time.Hour, now),
		fleetApprovalFixture("proj", "superseded-1", string(state.ApprovalStatusSuperseded), 5*time.Hour, now),
	}

	got := historicalFleetApprovals(mixed)
	if len(got) != 4 {
		t.Fatalf("historicalFleetApprovals len = %d, want 4 (all non-pending)", len(got))
	}
	for _, item := range got {
		if state.ApprovalStatus(item.Status) == state.ApprovalStatusPending {
			t.Fatalf("historicalFleetApprovals leaked pending approval %q", item.ID)
		}
	}
}

func TestHistoricalFleetApprovalsPendingOnlyReturnsEmpty(t *testing.T) {
	now := time.Now().UTC()
	pendingOnly := []fleetApprovalState{
		fleetApprovalFixture("proj", "pending-1", string(state.ApprovalStatusPending), time.Minute, now),
		fleetApprovalFixture("proj", "pending-2", string(state.ApprovalStatusPending), 2*time.Minute, now),
	}
	if got := historicalFleetApprovals(pendingOnly); len(got) != 0 {
		t.Fatalf("historicalFleetApprovals len = %d, want 0 for pending-only input", len(got))
	}
}

func TestHistoricalFleetApprovalsHistoryOnlyReturnsAll(t *testing.T) {
	now := time.Now().UTC()
	historyOnly := []fleetApprovalState{
		fleetApprovalFixture("proj", "stale-1", string(state.ApprovalStatusStale), time.Hour, now),
		fleetApprovalFixture("proj", "approved-1", string(state.ApprovalStatusApproved), 2*time.Hour, now),
	}
	if got := historicalFleetApprovals(historyOnly); len(got) != 2 {
		t.Fatalf("historicalFleetApprovals len = %d, want 2 for history-only input", len(got))
	}
}

// --- approvalAuditSummary copy --------------------------------------------

func TestApprovalAuditSummaryCalmWhenNoHistory(t *testing.T) {
	got := approvalAuditSummary(nil)
	if got != "No historical approvals recorded." {
		t.Fatalf("approvalAuditSummary(nil) = %q, want calm empty copy", got)
	}
	for _, alarmWord := range []string{"error", "alert", "blocked", "fail"} {
		if strings.Contains(strings.ToLower(got), alarmWord) {
			t.Fatalf("calm empty summary contains alarm word %q: %q", alarmWord, got)
		}
	}
}

func TestApprovalHistoryCountTextForAuditCounts(t *testing.T) {
	counts := map[string]int{
		string(state.ApprovalStatusSuperseded): 1,
		string(state.ApprovalStatusStale):      2,
		string(state.ApprovalStatusApproved):   3,
		string(state.ApprovalStatusRejected):   4,
	}
	got := approvalHistoryCountTextForAudit(counts, 10)
	for _, want := range []string{"1 superseded", "2 stale", "3 approved", "4 rejected"} {
		if !strings.Contains(got, want) {
			t.Fatalf("approvalHistoryCountTextForAudit missing %q in %q", want, got)
		}
	}
	if !strings.Contains(got, "·") {
		t.Fatalf("approvalHistoryCountTextForAudit should join parts with ·, got %q", got)
	}
}

func TestApprovalHistoryCountTextForAuditOnlyUnknown(t *testing.T) {
	got := approvalHistoryCountTextForAudit(map[string]int{}, 3)
	if !strings.Contains(got, "3 other") {
		t.Fatalf("unknown-only audit summary = %q, want 3 other", got)
	}
}

// --- audit page rendering -------------------------------------------------

func TestRenderFleetApprovalAuditRowsPendingOnlyShowsCalmEmpty(t *testing.T) {
	now := time.Now().UTC()
	pendingOnly := []fleetApprovalState{
		fleetApprovalFixture("proj", "pending-1", string(state.ApprovalStatusPending), time.Minute, now),
	}
	html := renderFleetApprovalAuditRows(historicalFleetApprovals(pendingOnly))
	if !strings.Contains(html, "No historical approvals have been recorded yet.") {
		t.Fatalf("pending-only audit rows should render calm empty state, got:\n%s", html)
	}
	if strings.Contains(html, "approval-past-sla") {
		t.Fatalf("pending-only audit rows should not surface SLA-styled cards, got:\n%s", html)
	}
}

func TestRenderFleetApprovalAuditRowsHistoryOnlyRendersCards(t *testing.T) {
	now := time.Now().UTC()
	historyOnly := []fleetApprovalState{
		fleetApprovalFixture("proj", "approved-1", string(state.ApprovalStatusApproved), 2*time.Hour, now),
		fleetApprovalFixture("proj", "stale-1", string(state.ApprovalStatusStale), 3*time.Hour, now),
	}
	html := renderFleetApprovalAuditRows(historicalFleetApprovals(historyOnly))
	for _, want := range []string{"approval-card-muted", "approval-approved", "approval-stale", "approved-1", "stale-1"} {
		if !strings.Contains(html, want) {
			t.Fatalf("history-only audit rows missing %q in:\n%s", want, html)
		}
	}
	if strings.Contains(html, "No historical approvals have been recorded yet.") {
		t.Fatalf("history-only audit rows should not render the empty state, got:\n%s", html)
	}
}

func TestRenderFleetApprovalAuditRowsMixedDropsPending(t *testing.T) {
	now := time.Now().UTC()
	mixed := []fleetApprovalState{
		fleetApprovalFixture("proj", "pending-keep-in-inbox", string(state.ApprovalStatusPending), time.Minute, now),
		fleetApprovalFixture("proj", "stale-show-in-audit", string(state.ApprovalStatusStale), time.Hour, now),
	}
	html := renderFleetApprovalAuditRows(historicalFleetApprovals(mixed))
	if strings.Contains(html, "pending-keep-in-inbox") {
		t.Fatalf("audit page leaked pending approval id, got:\n%s", html)
	}
	if !strings.Contains(html, "stale-show-in-audit") {
		t.Fatalf("audit page should keep stale approval id, got:\n%s", html)
	}
}

// TestExecutionSkippedAuditRowSurfacesSummary pins premortem failure mode #8:
// an approval the operator approved that came back execution_skipped must
// keep its executor summary on the audit row, AND must render with a CSS
// class distinct from `approval-executed`. Both signals feed the SPA-side
// distinct rendering (amber border + manual-follow-up callout) and the
// server-rendered audit page.
func TestExecutionSkippedAuditRowSurfacesSummary(t *testing.T) {
	now := time.Now().UTC()
	manualSummary := "change_global_config requires a manual edit + systemctl restart (executor not implemented)"
	items := []fleetApprovalState{
		{
			ProjectName: "proj",
			ID:          "skip-1",
			Action:      "change_global_config",
			Status:      string(state.ApprovalStatusExecutionSkipped),
			Risk:        "approval_gated",
			Summary:     manualSummary,
			createdAt:   now.Add(-time.Hour),
			updatedAt:   now.Add(-time.Hour),
		},
		{
			ProjectName: "proj",
			ID:          "exec-1",
			Action:      "merge_pr",
			Status:      string(state.ApprovalStatusExecuted),
			Risk:        "approval_gated",
			Summary:     "merged PR #1",
			createdAt:   now.Add(-2 * time.Hour),
			updatedAt:   now.Add(-2 * time.Hour),
		},
	}

	html := renderFleetApprovalAuditRows(items)
	if !strings.Contains(html, "approval-execution_skipped") {
		t.Fatalf("execution_skipped row should carry approval-execution_skipped class for distinct styling, got:\n%s", html)
	}
	if !strings.Contains(html, "approval-executed") {
		t.Fatalf("executed row should still carry approval-executed class, got:\n%s", html)
	}
	if strings.Contains(html, "approval-executed") &&
		strings.Index(html, "approval-execution_skipped") == strings.Index(html, "approval-executed") {
		t.Fatalf("execution_skipped and executed rows must NOT collapse to the same CSS class")
	}
	if !strings.Contains(html, "change_global_config requires a manual edit") {
		t.Fatalf("execution_skipped audit row must surface executor summary verbatim, got:\n%s", html)
	}
	if !strings.Contains(html, "execution_skipped") {
		t.Fatalf("execution_skipped audit row must surface status label, got:\n%s", html)
	}
}

// --- audit subtitle -------------------------------------------------------

func TestApprovalAuditSubtitleReportsPendingCount(t *testing.T) {
	snapshot := fleetResponse{
		Summary: fleetSummary{
			Projects:         3,
			ApprovalsPending: 2,
		},
	}
	got := approvalAuditSubtitle(snapshot)
	for _, want := range []string{"3 configured projects", "2 active pending approvals"} {
		if !strings.Contains(got, want) {
			t.Fatalf("audit subtitle missing %q in %q", want, got)
		}
	}
}

// --- audit endpoint behaviour --------------------------------------------

func TestHandleFleetApprovalAuditPendingOnlyRendersCalmEmpty(t *testing.T) {
	srv, projectName, dir := newFleetForApprovalTest(t)
	_ = projectName
	enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})

	req := httptest.NewRequest(http.MethodGet, "/approvals/audit", nil)
	w := httptest.NewRecorder()
	srv.handleFleetApprovalAudit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No historical approvals have been recorded yet.") {
		t.Fatalf("pending-only audit endpoint should render calm empty body, got:\n%s", body)
	}
	if !strings.Contains(body, "Historical Approvals") {
		t.Fatalf("audit endpoint should keep header, got:\n%s", body)
	}
}

// --- attention count contract --------------------------------------------

func TestFleetAttentionSentenceIgnoresHistoricalApprovalCounts(t *testing.T) {
	historicalOnly := fleetSummary{
		ApprovalsHistorical: 12,
		ApprovalsApproved:   4,
		ApprovalsRejected:   4,
		ApprovalsStale:      2,
		ApprovalsSuperseded: 2,
	}
	if got := fleetAttentionSentence(historicalOnly); got != "No item needs attention." {
		t.Fatalf("attention sentence with historical-only approvals = %q, want calm no-attention copy", got)
	}

	pendingOnly := fleetSummary{ApprovalsPending: 1, Approvals: 1}
	if got := fleetAttentionSentence(pendingOnly); got != "1 item needs attention." {
		t.Fatalf("attention sentence with pending approval = %q, want 1 item needs attention", got)
	}
}

func TestFleetVerdictToneDoesNotEscalateOnHistoricalApprovals(t *testing.T) {
	now := time.Now().UTC()
	latest := &supervisorDecisionInfo{CreatedAt: now}

	historicalOnly := fleetSummary{
		ApprovalsHistorical: 50,
		ApprovalsApproved:   40,
		ApprovalsRejected:   10,
	}
	if tone := fleetVerdictTone(historicalOnly, latest, now); tone == "attention" {
		t.Fatalf("verdict tone with historical-only approvals = %q, should not escalate to attention", tone)
	}

	pendingOnly := fleetSummary{ApprovalsPending: 1, Approvals: 1}
	if tone := fleetVerdictTone(pendingOnly, latest, now); tone != "attention" {
		t.Fatalf("verdict tone with pending approval = %q, want attention", tone)
	}
}
