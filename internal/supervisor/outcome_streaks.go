package supervisor

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

// gateFailStreakNotify and gateFailStreakIntake are the side-effecting dispatch
// points for a first-class gate_fail_streak event. They are package variables so
// tests can observe the deduped notification/intake without a Telegram relay or
// live GitHub repo.
var (
	gateFailStreakNotify = defaultGateFailStreakNotify
	gateFailStreakIntake = defaultGateFailStreakIntake
)

// EvaluateGateFailureStreaksOnce folds one scheduled outcome health result into
// the per-gate consecutive-failure streak table and escalates a streak that
// reached the configured threshold into a first-class supervisor event: a
// gate_fail_streak notification plus one deduped repair issue. It reuses a
// health result another evaluator already produced this interval so the shared
// scheduled clock never double-reads the runtime.
//
// The returned bool is true only when a scheduled run was folded this call. A
// call inside the interval with no fresh unfolded result is a cheap no-op.
func EvaluateGateFailureStreaksOnce(cfg *config.Config, now time.Time) (bool, error) {
	if cfg == nil {
		return false, fmt.Errorf("gate-fail-streak evaluator: nil config")
	}
	brief := cfg.Outcome.Normalized()
	if !brief.GateFailStreakEnabled() {
		return false, nil
	}
	// Automatic-recovery projects have a more specific bounded-futility
	// controller: it owns retries until K unchanged failing verifications, then
	// files the outcome-repair issue and emits futile_recovery. Running the
	// generic gate-streak intake in parallel would file/notify before that fence
	// expires and could create a second repair issue for the same red gate.
	if brief.AutomaticRecoveryEnabled() {
		return false, nil
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return false, fmt.Errorf("gate-fail-streak evaluator: empty state_dir")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		return false, fmt.Errorf("gate-fail-streak evaluator: load state: %w", err)
	}
	interval := brief.EffectiveRecoveryInterval()

	// Prefer a scheduled health result the outcome-recovery evaluator already
	// produced this interval; only read the runtime ourselves when none is
	// available and the interval has elapsed. RecordOutcomeGateStreaks folds each
	// distinct run (by CheckedAt) exactly once regardless of which evaluator read it.
	var result outcome.HealthCheckResult
	haveFresh := st.OutcomeHealth != nil && st.OutcomeHealth.CheckedAt.After(st.OutcomeGateStreakCheckedAt)
	if haveFresh {
		result = *st.OutcomeHealth
	} else {
		if !st.OutcomeGateStreakCheckedAt.IsZero() && now.Sub(st.OutcomeGateStreakCheckedAt) < interval {
			return false, nil
		}
		result = checkOutcomeForRecovery(context.Background(), brief)
	}

	threshold := brief.EffectiveGateFailStreakThreshold()
	runLink := gateFailStreakRunLink(brief)
	var events []outcome.GateStreakEvent
	if err := state.Update(cfg.StateDir, func(latest *state.State) error {
		if !haveFresh {
			setOutcomeHealthIfNewer(latest, result)
		}
		events = latest.RecordOutcomeGateStreaks(result, threshold, runLink, now)
		return nil
	}); err != nil {
		return false, fmt.Errorf("gate-fail-streak evaluator: record streaks: %w", err)
	}
	if len(events) > 0 {
		dispatchGateFailStreakEvents(cfg, events, now)
	}
	return true, nil
}

// dispatchGateFailStreakEvents performs the notification and repair intake for
// each escalated streak, persisting a dedup mark only after the side effect
// succeeds. A transient failure leaves the mark unset so the next scheduled run
// retries; once a fingerprint is marked, a repeat same-fingerprint failure never
// re-fires.
func dispatchGateFailStreakEvents(cfg *config.Config, events []outcome.GateStreakEvent, now time.Time) {
	for _, event := range events {
		if event.NotifyPending {
			if err := gateFailStreakNotify(cfg, event); err != nil {
				log.Printf("[outcome/gate-streak] gate_fail_streak notify failed for gate %q (fingerprint=%s): %v", event.Gate, event.Fingerprint, err)
			} else if err := state.Update(cfg.StateDir, func(latest *state.State) error {
				latest.MarkGateStreakNotified(event.Gate, event.Fingerprint, now)
				return nil
			}); err != nil {
				log.Printf("[outcome/gate-streak] persist notify mark failed for gate %q: %v", event.Gate, err)
			}
		}
		if event.IntakePending {
			issue, err := gateFailStreakIntake(cfg, event)
			if err != nil {
				log.Printf("[outcome/gate-streak] repair intake failed for gate %q (fingerprint=%s): %v", event.Gate, event.Fingerprint, err)
				continue
			}
			if err := state.Update(cfg.StateDir, func(latest *state.State) error {
				latest.MarkGateStreakIntaken(event.Gate, event.Fingerprint, issue, now)
				return nil
			}); err != nil {
				log.Printf("[outcome/gate-streak] persist intake mark failed for gate %q: %v", event.Gate, err)
			}
		}
	}
}

func defaultGateFailStreakNotify(cfg *config.Config, event outcome.GateStreakEvent) error {
	if cfg == nil {
		return nil
	}
	notifier := notify.NewWithToken(cfg.Telegram.BotToken, cfg.Telegram.Target, cfg.Telegram.Mode, cfg.Telegram.OpenclawURL)
	return notifier.Send(gateFailStreakMessage(event))
}

func defaultGateFailStreakIntake(cfg *config.Config, event outcome.GateStreakEvent) (int, error) {
	if cfg == nil || strings.TrimSpace(cfg.Repo) == "" {
		return 0, fmt.Errorf("gate-fail-streak intake: no repo configured")
	}
	client := github.New(cfg.Repo)
	title := fmt.Sprintf("gate failure streak: %s failing %d consecutive scheduled runs", event.Gate, event.ConsecutiveFailures)
	return client.CreateIssue(title, gateFailStreakIssueBody(event), nil)
}

// gateFailStreakMessage is the first-class notification body: gate name, streak
// count, reason class, and the latest run link. It carries no command text,
// output, or host-side path.
func gateFailStreakMessage(event outcome.GateStreakEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 maestro: scheduled gate `%s` failed %d consecutive runs", event.Gate, event.ConsecutiveFailures)
	if strings.TrimSpace(event.ReasonClass) != "" {
		fmt.Fprintf(&b, " (reason: %s)", event.ReasonClass)
	}
	if link := strings.TrimSpace(event.RunLink); link != "" {
		fmt.Fprintf(&b, "\nlatest run: %s", link)
	}
	return b.String()
}

// gateFailStreakIssueBody embeds a hidden fingerprint marker so the deduped
// repair-issue path can recognize an existing streak issue. It records only
// bounded, non-secret streak evidence.
func gateFailStreakIssueBody(event outcome.GateStreakEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- maestro:gate-fail-streak %s -->\n\n", event.Fingerprint)
	fmt.Fprintf(&b, "Scheduled gate `%s` failed %d consecutive runs.\n\n", event.Gate, event.ConsecutiveFailures)
	if strings.TrimSpace(event.ReasonClass) != "" {
		fmt.Fprintf(&b, "- Reason class: `%s`\n", event.ReasonClass)
	}
	fmt.Fprintf(&b, "- Fingerprint: `%s`\n", event.Fingerprint)
	fmt.Fprintf(&b, "- Consecutive failures: %d\n", event.ConsecutiveFailures)
	if !event.FirstFailedAt.IsZero() {
		fmt.Fprintf(&b, "- First failed: %s\n", event.FirstFailedAt.UTC().Format(time.RFC3339))
	}
	if !event.LastFailedAt.IsZero() {
		fmt.Fprintf(&b, "- Latest failure: %s\n", event.LastFailedAt.UTC().Format(time.RFC3339))
	}
	if link := strings.TrimSpace(event.RunLink); link != "" {
		fmt.Fprintf(&b, "- Latest run: %s\n", link)
	}
	b.WriteString("\nThe scheduled gate is failing repeatedly with the same fingerprint; investigate and repair the gate. This issue was opened automatically by Maestro's gate failure-streak escalation.\n")
	return b.String()
}

// gateFailStreakRunLink is a non-secret pointer to the latest scheduled run's
// runtime/healthcheck surface, used in the notification and repair issue.
func gateFailStreakRunLink(brief outcome.Brief) string {
	if link := strings.TrimSpace(brief.HealthcheckURL); link != "" {
		return link
	}
	if link := strings.TrimSpace(brief.RuntimeURL); link != "" {
		return link
	}
	return strings.TrimSpace(brief.RuntimeTarget)
}
