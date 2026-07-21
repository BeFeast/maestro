package outcome

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// DefaultGateFailStreakThreshold is the consecutive-failure count at which a
// scheduled gate failure streak becomes a first-class supervisor event.
const DefaultGateFailStreakThreshold = 2

// maxTrackedGateStreaks bounds the number of retired (passed) gate records the
// streak table retains for visibility, so a project that churns gate names over
// time cannot grow the durable table without limit.
const maxTrackedGateStreaks = 64

// GateStreak is the durable per-gate consecutive-failure counter with a failure
// fingerprint (gate name + reason class). A passing run or a fingerprint change
// resets it, so it always describes exactly one uninterrupted failure run of a
// single scheduled/candidate gate.
type GateStreak struct {
	Gate                string    `json:"gate"`
	ReasonClass         string    `json:"reason_class,omitempty"`
	Fingerprint         string    `json:"fingerprint,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	FirstFailedAt       time.Time `json:"first_failed_at,omitempty"`
	LastFailedAt        time.Time `json:"last_failed_at,omitempty"`
	LastTransitionAt    time.Time `json:"last_transition_at,omitempty"`
	LastRunLink         string    `json:"last_run_link,omitempty"`
	// NotifiedFingerprint records the fingerprint whose streak already produced a
	// gate_fail_streak notification. It dedupes the first-class event so a repeat
	// same-fingerprint failure past the threshold never re-notifies.
	NotifiedFingerprint string `json:"notified_fingerprint,omitempty"`
	// IntakeFingerprint records the fingerprint whose streak already produced a
	// deduped repair issue; IntakeIssue is that issue number (for visibility).
	IntakeFingerprint string `json:"intake_fingerprint,omitempty"`
	IntakeIssue       int    `json:"intake_issue,omitempty"`
}

// GateStreakEvent describes a same-fingerprint failure streak that reached the
// escalation threshold and still has a pending notification and/or intake.
type GateStreakEvent struct {
	Gate                string
	ReasonClass         string
	Fingerprint         string
	ConsecutiveFailures int
	RunLink             string
	FirstFailedAt       time.Time
	LastFailedAt        time.Time
	NotifyPending       bool
	IntakePending       bool
}

// GateStreakFingerprint is the stable failure fingerprint for a gate: its name
// plus the reason class of the failure. A different reason class is a different
// fingerprint, so it starts a fresh streak instead of extending the old one.
func GateStreakFingerprint(gate, reasonClass string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(gate) + "\x00" + strings.TrimSpace(reasonClass)))
	return hex.EncodeToString(sum[:8])
}

type gateOutcome int

const (
	gateIndeterminate gateOutcome = iota
	gatePass
	gateFail
)

type gateObservation struct {
	gate        string
	status      gateOutcome
	reasonClass string
}

// gateObservations derives the per-gate pass/fail observations from a health
// check result. Structured checks yield one observation per named check; a bare
// signal result yields a single gate keyed by the check signal.
func gateObservations(result HealthCheckResult) []gateObservation {
	if len(result.Checks) > 0 {
		obs := make([]gateObservation, 0, len(result.Checks))
		for _, check := range result.Checks {
			name := strings.TrimSpace(check.Name)
			if name == "" {
				name = "structured-check"
			}
			status, reason := classifyGateStatus(check.Status)
			obs = append(obs, gateObservation{gate: name, status: status, reasonClass: reason})
		}
		return obs
	}
	gate := strings.TrimSpace(result.Signal)
	if gate == "" {
		gate = "outcome"
	}
	switch normalizedHealthState(result.State) {
	case HealthFailing:
		return []gateObservation{{gate: gate, status: gateFail, reasonClass: "failing"}}
	case HealthHealthy:
		return []gateObservation{{gate: gate, status: gatePass}}
	default:
		// unknown/pending/unmonitored is neither a definitive pass nor fail, so it
		// must not increment or reset a streak.
		return nil
	}
}

func classifyGateStatus(status string) (gateOutcome, string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "fail", "failing":
		return gateFail, "fail"
	case "error":
		return gateFail, "error"
	case "pass", "healthy":
		return gatePass, ""
	default:
		// warning/unknown/pending/in_progress/queued: indeterminate.
		return gateIndeterminate, ""
	}
}

// RecordGateStreaks folds one scheduled health check result into the per-gate
// streak table. It increments a same-fingerprint failure run, resets on a pass
// or fingerprint change, and returns the streaks that have reached the
// threshold with a still-pending notification or intake. Emission markers are
// carried forward untouched: the caller sets them (via MarkGateStreak*) only
// after the side effect succeeds, so a transient dispatch failure is retried on
// the next scheduled run and a repeat failure never double-fires.
func RecordGateStreaks(prev []GateStreak, result HealthCheckResult, threshold int, runLink string, now time.Time) ([]GateStreak, []GateStreakEvent) {
	if threshold <= 0 {
		threshold = DefaultGateFailStreakThreshold
	}
	now = now.UTC()
	runLink = strings.TrimSpace(runLink)

	byGate := make(map[string]GateStreak, len(prev))
	for _, streak := range prev {
		byGate[streak.Gate] = streak
	}

	for _, obs := range gateObservations(result) {
		existing, had := byGate[obs.gate]
		switch obs.status {
		case gateFail:
			fingerprint := GateStreakFingerprint(obs.gate, obs.reasonClass)
			if had && existing.Fingerprint == fingerprint && existing.ConsecutiveFailures > 0 {
				existing.ConsecutiveFailures++
				existing.LastFailedAt = now
				existing.LastRunLink = runLink
				byGate[obs.gate] = existing
				continue
			}
			byGate[obs.gate] = GateStreak{
				Gate:                obs.gate,
				ReasonClass:         obs.reasonClass,
				Fingerprint:         fingerprint,
				ConsecutiveFailures: 1,
				FirstFailedAt:       now,
				LastFailedAt:        now,
				LastTransitionAt:    now,
				LastRunLink:         runLink,
			}
		case gatePass:
			if !had || existing.ConsecutiveFailures == 0 {
				continue
			}
			byGate[obs.gate] = GateStreak{
				Gate:             obs.gate,
				LastTransitionAt: now,
				LastFailedAt:     existing.LastFailedAt,
			}
		case gateIndeterminate:
			// Carry the prior streak forward unchanged; an inconclusive run neither
			// advances nor clears the failure run.
		}
	}

	next := make([]GateStreak, 0, len(byGate))
	for _, streak := range byGate {
		next = append(next, streak)
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Gate < next[j].Gate })
	next = pruneGateStreaks(next)

	var events []GateStreakEvent
	for _, streak := range next {
		if streak.ConsecutiveFailures < threshold || streak.Fingerprint == "" {
			continue
		}
		notifyPending := streak.NotifiedFingerprint != streak.Fingerprint
		intakePending := streak.IntakeFingerprint != streak.Fingerprint
		if !notifyPending && !intakePending {
			continue
		}
		events = append(events, GateStreakEvent{
			Gate:                streak.Gate,
			ReasonClass:         streak.ReasonClass,
			Fingerprint:         streak.Fingerprint,
			ConsecutiveFailures: streak.ConsecutiveFailures,
			RunLink:             streak.LastRunLink,
			FirstFailedAt:       streak.FirstFailedAt,
			LastFailedAt:        streak.LastFailedAt,
			NotifyPending:       notifyPending,
			IntakePending:       intakePending,
		})
	}
	return next, events
}

// pruneGateStreaks bounds the retired (passed) records the table keeps for
// visibility. Active failure runs are always retained; only count==0 records
// beyond the cap are dropped, oldest transition first.
func pruneGateStreaks(streaks []GateStreak) []GateStreak {
	if len(streaks) <= maxTrackedGateStreaks {
		return streaks
	}
	retired := make([]int, 0, len(streaks))
	for i, streak := range streaks {
		if streak.ConsecutiveFailures == 0 {
			retired = append(retired, i)
		}
	}
	drop := len(streaks) - maxTrackedGateStreaks
	if drop > len(retired) {
		drop = len(retired)
	}
	if drop == 0 {
		return streaks
	}
	sort.SliceStable(retired, func(i, j int) bool {
		return streaks[retired[i]].LastTransitionAt.Before(streaks[retired[j]].LastTransitionAt)
	})
	dropped := make(map[int]struct{}, drop)
	for _, idx := range retired[:drop] {
		dropped[idx] = struct{}{}
	}
	kept := make([]GateStreak, 0, len(streaks)-drop)
	for i, streak := range streaks {
		if _, ok := dropped[i]; ok {
			continue
		}
		kept = append(kept, streak)
	}
	return kept
}
