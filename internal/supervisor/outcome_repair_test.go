package supervisor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

type fakeOutcomeRepairClient struct {
	issues       []github.Issue
	createCalls  int
	createdTitle string
	createdBody  string
	created      []string
	edited       []int
	addedLabels  []string
	ensured      []string
	ensureErr    error
	closed       []int
	onList       func()
	onCreate     func()
	nextIssue    int
}

func (f *fakeOutcomeRepairClient) ListAllOpenIssues([]string) ([]github.Issue, error) {
	if f.onList != nil {
		f.onList()
	}
	return append([]github.Issue(nil), f.issues...), nil
}

func (f *fakeOutcomeRepairClient) CreateIssue(title, body string, labels []string) (int, error) {
	if f.onCreate != nil {
		f.onCreate()
	}
	f.createCalls++
	f.createdTitle = title
	f.createdBody = body
	f.created = append([]string(nil), labels...)
	if f.nextIssue == 0 {
		f.nextIssue = 900
	}
	issue := outcomeRepairTestIssue(f.nextIssue, title, body, labels...)
	f.issues = append(f.issues, issue)
	return f.nextIssue, nil
}

func (f *fakeOutcomeRepairClient) EditIssueBody(number int, body string) error {
	f.edited = append(f.edited, number)
	for i := range f.issues {
		if f.issues[i].Number == number {
			f.issues[i].Body = body
		}
	}
	return nil
}

func (f *fakeOutcomeRepairClient) EnsureLabel(name, color, description string) error {
	f.ensured = append(f.ensured, strings.Join([]string{name, color, description}, ":"))
	return f.ensureErr
}

func (f *fakeOutcomeRepairClient) CloseIssue(number int, _ string) error {
	f.closed = append(f.closed, number)
	for i := range f.issues {
		if f.issues[i].Number == number {
			f.issues[i].State = "closed"
		}
	}
	return nil
}

func (f *fakeOutcomeRepairClient) AddIssueLabel(number int, label string) error {
	f.addedLabels = append(f.addedLabels, fmt.Sprintf("#%d:%s", number, label))
	for i := range f.issues {
		if f.issues[i].Number != number {
			continue
		}
		f.issues[i].Labels = append(f.issues[i].Labels, struct {
			Name string `json:"name"`
		}{Name: label})
	}
	return nil
}

type recordedFutileAlert struct {
	class notify.AlertClass
	key   string
	title string
	body  string
}

type fakeFutileNotifier struct {
	alerts []recordedFutileAlert
}

func (f *fakeFutileNotifier) Alert(class notify.AlertClass, key, title, body string) error {
	f.alerts = append(f.alerts, recordedFutileAlert{class: class, key: key, title: title, body: body})
	return nil
}

func outcomeRepairTestIssue(number int, title, body string, labels ...string) github.Issue {
	issue := github.Issue{Number: number, Title: title, Body: body, State: "open"}
	for _, label := range labels {
		issue.Labels = append(issue.Labels, struct {
			Name string `json:"name"`
		}{Name: label})
	}
	return issue
}

func installOutcomeRepairStubs(t *testing.T, client *fakeOutcomeRepairClient, notifier *fakeFutileNotifier) {
	t.Helper()
	originalClient, originalNotifier := newOutcomeRepairIssueClient, newFutileRecoveryNotifier
	t.Cleanup(func() {
		newOutcomeRepairIssueClient = originalClient
		newFutileRecoveryNotifier = originalNotifier
	})
	newOutcomeRepairIssueClient = func(string, config.ForgeConfig) outcomeRepairIssueClient { return client }
	newFutileRecoveryNotifier = func(*config.Config) futileRecoveryNotifier { return notifier }
}

func futileRecoveryTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Repo:        "owner/repo",
		StateDir:    t.TempDir(),
		IssueLabels: []string{"maestro-ready"},
		Outcome: outcome.Brief{
			DesiredOutcome:            "candidate feed follows main",
			HealthcheckCommand:        "./check-outcome.sh",
			RecoveryMode:              outcome.RecoveryModeAutomatic,
			RecoveryCommand:           "./recover-outcome.sh",
			RecoveryMaxFutileAttempts: 1,
		},
	}
}

func failingRecoveryHealth(at time.Time) outcome.HealthCheckResult {
	return outcome.HealthCheckResult{
		CheckedAt: at,
		Signal:    "healthcheck_command",
		State:     outcome.HealthFailing,
		Summary:   "healthcheck_command: linux-candidate-delivery reported fail",
		Detail:    "Authorization: Bearer should-not-leak\nAPI_TOKEN=also-secret\n" + strings.Repeat("bounded ", 500),
		Checks: []outcome.HealthCheckItem{{
			Name: "linux-candidate-delivery", Blocking: true, Status: "fail",
		}},
	}
}

func seedCappedRecovery(t *testing.T, cfg *config.Config, at time.Time) string {
	t.Helper()
	health := failingRecoveryHealth(at)
	fingerprint := state.OutcomeFailureFingerprint(health)
	st := state.NewState()
	st.OutcomeHealth = &health
	st.OutcomeRecovery = &outcome.RecoveryState{
		Status:              outcome.RecoveryStatusCapped,
		ConsecutiveFailures: 1,
		FailureFingerprint:  fingerprint,
		CappedAt:            at,
		UpdatedAt:           at,
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("seed capped recovery: %v", err)
	}
	return fingerprint
}

func TestEvaluateOutcomeRecoveryOnce_CapFilesIssueAndNotifiesSameCycle(t *testing.T) {
	cfg := futileRecoveryTestConfig(t)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	client := &fakeOutcomeRepairClient{nextIssue: 901}
	notifier := &fakeFutileNotifier{}
	installOutcomeRepairStubs(t, client, notifier)

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	t0 := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	checks := 0
	checkOutcomeForRecovery = func(context.Context, outcome.Brief) outcome.HealthCheckResult {
		checks++
		return failingRecoveryHealth(t0.Add(time.Duration(checks) * time.Second))
	}
	runOutcomeRecovery = func(context.Context, outcome.Brief) outcome.RecoveryExecution {
		return outcome.RecoveryExecution{StartedAt: t0, FinishedAt: t0.Add(time.Second), ExitCode: 0}
	}

	if evaluated, err := EvaluateOutcomeRecoveryOnce(cfg, t0); err != nil || !evaluated {
		t.Fatalf("EvaluateOutcomeRecoveryOnce: evaluated=%t err=%v", evaluated, err)
	}
	if client.createCalls != 1 {
		t.Fatalf("repair issue creates = %d, want 1 in the cap-hit cycle", client.createCalls)
	}
	if len(client.ensured) != 1 || !strings.HasPrefix(client.ensured[0], outcome.OutcomeRepairLabel+":") {
		t.Fatalf("outcome-repair label was not provisioned before create: %v", client.ensured)
	}
	if !containsStringFold(client.created, outcome.OutcomeRepairLabel) || !containsStringFold(client.created, "maestro-ready") {
		t.Fatalf("repair labels = %v, want outcome-repair + ready", client.created)
	}
	if strings.Contains(client.createdBody, "should-not-leak") || strings.Contains(client.createdBody, "also-secret") {
		t.Fatalf("repair issue leaked persisted sensitive excerpt: %q", client.createdBody)
	}
	if len(client.createdBody) > 4000 {
		t.Fatalf("repair issue body is not bounded: %d bytes", len(client.createdBody))
	}
	if len(notifier.alerts) != 1 {
		t.Fatalf("alerts = %d, want one", len(notifier.alerts))
	}
	alert := notifier.alerts[0]
	if alert.class != notify.AlertFutileRecovery || !strings.Contains(alert.body, "linux-candidate-delivery") || !strings.Contains(alert.body, "901") {
		t.Fatalf("alert = %+v, want futile_recovery with gate + issue link", alert)
	}

	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	recovery := loaded.OutcomeRecovery
	if recovery == nil || recovery.Status != outcome.RecoveryStatusCapped || recovery.RepairIssue != 901 {
		t.Fatalf("durable cap/intake state = %+v", recovery)
	}
	if recovery.RepairFingerprint == "" || recovery.NotifiedFingerprint != recovery.RepairFingerprint {
		t.Fatalf("durable dedup markers = %+v", recovery)
	}

	// Replaying inside the health interval consumes the durable markers without
	// another recovery lease, issue, or notification.
	if evaluated, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(10*time.Second)); err != nil || evaluated {
		t.Fatalf("replay: evaluated=%t err=%v", evaluated, err)
	}
	if client.createCalls != 1 || len(notifier.alerts) != 1 {
		t.Fatalf("unchanged replay duplicated side effects: creates=%d alerts=%d", client.createCalls, len(notifier.alerts))
	}
}

func TestEnsureOutcomeRepairIssue_ReusesAndRefreshesFingerprintMarker(t *testing.T) {
	fingerprint := "checks:linux-candidate-delivery=fail"
	client := &fakeOutcomeRepairClient{
		issues: []github.Issue{
			outcomeRepairTestIssue(777, "old title", "old body\n"+outcomeRepairMarker(fingerprint)),
		},
	}
	desired := "new bounded body\n" + outcomeRepairMarker(fingerprint)
	number, created, err := ensureOutcomeRepairIssue(client, "new title", desired, []string{outcome.OutcomeRepairLabel, "maestro-ready"}, fingerprint, nil)
	if err != nil {
		t.Fatalf("ensureOutcomeRepairIssue: %v", err)
	}
	if number != 777 || created || client.createCalls != 0 {
		t.Fatalf("dedup result issue=%d created=%t creates=%d, want existing #777", number, created, client.createCalls)
	}
	if len(client.edited) != 1 || client.issues[0].Body != desired {
		t.Fatalf("existing issue was not refreshed: edited=%v body=%q", client.edited, client.issues[0].Body)
	}
	if !github.HasLabel(client.issues[0], []string{outcome.OutcomeRepairLabel}) || !github.HasLabel(client.issues[0], []string{"maestro-ready"}) {
		t.Fatalf("existing issue labels were not repaired: %+v", client.issues[0].Labels)
	}
}

func TestOutcomeRepairLabels_FallsBackToReadyLabelWhenClassifierCannotBeProvisioned(t *testing.T) {
	cfg := futileRecoveryTestConfig(t)
	client := &fakeOutcomeRepairClient{ensureErr: fmt.Errorf("label permission denied")}

	labels := outcomeRepairLabels(client, cfg)
	if containsStringFold(labels, outcome.OutcomeRepairLabel) {
		t.Fatalf("labels = %v, optional unavailable classifier should be omitted", labels)
	}
	if !containsStringFold(labels, "maestro-ready") {
		t.Fatalf("labels = %v, project ready label must remain", labels)
	}
}

func TestDispatchFutileOutcomeRecovery_RevalidatesBeforeCreate(t *testing.T) {
	cfg := futileRecoveryTestConfig(t)
	t0 := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	seedCappedRecovery(t, cfg, t0)

	client := &fakeOutcomeRepairClient{nextIssue: 902}
	notifier := &fakeFutileNotifier{}
	installOutcomeRepairStubs(t, client, notifier)
	client.onList = func() {
		client.onList = nil
		if err := state.Update(cfg.StateDir, func(latest *state.State) error {
			latest.OutcomeHealth.State = outcome.HealthHealthy
			latest.OutcomeHealth.CheckedAt = t0.Add(time.Second)
			latest.OutcomeRecovery.Status = outcome.RecoveryStatusVerified
			latest.OutcomeRecovery.FailureFingerprint = ""
			latest.OutcomeRecovery.UpdatedAt = t0.Add(time.Second)
			return nil
		}); err != nil {
			t.Fatalf("change cap during dedup read: %v", err)
		}
	}

	dispatchFutileOutcomeRecovery(cfg, t0.Add(2*time.Second))
	if client.createCalls != 0 {
		t.Fatalf("stale capped snapshot created %d issues, want 0", client.createCalls)
	}
	if len(notifier.alerts) != 0 {
		t.Fatalf("stale capped snapshot emitted alerts: %+v", notifier.alerts)
	}
}

func TestDispatchFutileOutcomeRecovery_DeclinedLinkageDoesNotNotify(t *testing.T) {
	cfg := futileRecoveryTestConfig(t)
	t0 := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	fingerprint := seedCappedRecovery(t, cfg, t0)

	client := &fakeOutcomeRepairClient{nextIssue: 903}
	notifier := &fakeFutileNotifier{}
	installOutcomeRepairStubs(t, client, notifier)
	client.onCreate = func() {
		client.onCreate = nil
		if err := state.Update(cfg.StateDir, func(latest *state.State) error {
			latest.OutcomeHealth = &outcome.HealthCheckResult{
				CheckedAt: t0.Add(time.Second),
				Signal:    "healthcheck_command",
				State:     outcome.HealthFailing,
				Checks:    []outcome.HealthCheckItem{{Name: "different-gate", Blocking: true, Status: "fail"}},
			}
			latest.OutcomeRecovery.Status = outcome.RecoveryStatusFailed
			latest.OutcomeRecovery.FailureFingerprint = state.OutcomeFailureFingerprint(*latest.OutcomeHealth)
			latest.OutcomeRecovery.UpdatedAt = t0.Add(time.Second)
			return nil
		}); err != nil {
			t.Fatalf("change fingerprint after create: %v", err)
		}
	}

	dispatchFutileOutcomeRecovery(cfg, t0.Add(2*time.Second))
	if client.createCalls != 1 {
		t.Fatalf("create calls = %d, want the one interleaved create", client.createCalls)
	}
	if len(client.closed) != 1 || client.closed[0] != 903 {
		t.Fatalf("obsolete created issue was not retired: closed=%v", client.closed)
	}
	if len(notifier.alerts) != 0 {
		t.Fatalf("declined linkage emitted alert: %+v", notifier.alerts)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if loaded.OutcomeRecovery.RepairIssue != 0 || loaded.OutcomeRecovery.RepairFingerprint == fingerprint {
		t.Fatalf("declined linkage was mirrored into state: %+v", loaded.OutcomeRecovery)
	}
}

func TestDispatchFutileOutcomeRecovery_CrashWindowReusesGitHubMarker(t *testing.T) {
	cfg := futileRecoveryTestConfig(t)
	t0 := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	fingerprint := seedCappedRecovery(t, cfg, t0)
	health := failingRecoveryHealth(t0)
	client := &fakeOutcomeRepairClient{
		issues: []github.Issue{outcomeRepairTestIssue(
			904,
			"existing repair",
			"created before state linkage\n"+outcomeRepairMarker(fingerprint),
		)},
	}
	notifier := &fakeFutileNotifier{}
	installOutcomeRepairStubs(t, client, notifier)

	dispatchFutileOutcomeRecovery(cfg, t0.Add(time.Second))
	if client.createCalls != 0 {
		t.Fatalf("crash recovery created %d duplicate issues, want 0", client.createCalls)
	}
	if len(client.edited) != 1 || client.edited[0] != 904 {
		t.Fatalf("existing marker issue was not refreshed: edited=%v", client.edited)
	}
	if !strings.Contains(client.issues[0].Body, outcomeRepairIssueBody("linux-candidate-delivery", fingerprint, 1, health)) {
		t.Fatalf("existing marker issue did not receive current bounded body: %q", client.issues[0].Body)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if loaded.OutcomeRecovery.RepairIssue != 904 || len(notifier.alerts) != 1 {
		t.Fatalf("crash recovery did not link/notify once: recovery=%+v alerts=%v", loaded.OutcomeRecovery, notifier.alerts)
	}
}

func TestDispatchFutileOutcomeRecovery_ConcurrentCyclesCreateAndNotifyOnce(t *testing.T) {
	cfg := futileRecoveryTestConfig(t)
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	seedCappedRecovery(t, cfg, t0)
	client := &fakeOutcomeRepairClient{nextIssue: 905}
	notifier := &fakeFutileNotifier{}
	installOutcomeRepairStubs(t, client, notifier)

	start := make(chan struct{})
	errs := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			dispatchFutileOutcomeRecovery(cfg, t0.Add(time.Second))
			errs <- struct{}{}
		}()
	}
	close(start)
	<-errs
	<-errs

	if client.createCalls != 1 || len(notifier.alerts) != 1 {
		t.Fatalf("concurrent cap cycles duplicated side effects: creates=%d alerts=%d", client.createCalls, len(notifier.alerts))
	}
}

func TestOutcomeRepairIssueBody_StripsURLSecretsAndSensitiveHeaders(t *testing.T) {
	health := failingRecoveryHealth(time.Now().UTC())
	health.Signal = "healthcheck_url"
	health.Summary = "GET https://user:password@example.test/health?token=secret#debug returned 500"
	health.Detail = "opaque-private-response-material\nSet-Cookie: session=secret\nX-Api-Key: secret"
	body := outcomeRepairIssueBody("gate", "fingerprint", 3, health)

	for _, secret := range []string{"password", "token=secret", "session=secret", "opaque-private-response-material", "X-Api-Key: secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("published repair body leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "https://example.test") {
		t.Fatalf("repair body lost safe origin context: %s", body)
	}
}

func containsStringFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// #1172 M3 — the futile_recovery issue link must follow the configured forge;
// the `#%d` fallback shape survives a missing repo or number.
func TestOutcomeRepairIssueLinkForgeAware(t *testing.T) {
	gh := config.ForgeConfig{}
	fj := config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example.com/"}
	if got := outcomeRepairIssueLink(gh, "o/r", 12); got != "https://github.com/o/r/issues/12" {
		t.Errorf("github link = %q", got)
	}
	if got := outcomeRepairIssueLink(fj, "o/r", 12); got != "https://forge.example.com/o/r/issues/12" {
		t.Errorf("forgejo link = %q (trailing base_url slash must be trimmed)", got)
	}
	if got := outcomeRepairIssueLink(fj, "   ", 12); got != "#12" {
		t.Errorf("missing repo fallback = %q, want #12", got)
	}
	if got := outcomeRepairIssueLink(gh, "o/r", 0); got != "#0" {
		t.Errorf("missing number fallback = %q, want #0", got)
	}
}
