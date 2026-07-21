package orphan

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Actuator performs the host side effects of a reap Plan. The runtime injects a
// real implementation (OSActuator: signal a process group, `docker rm` an
// exited container); tests inject a fake so the decision-to-actuation contract
// is verifiable without touching real processes or Docker.
//
// TerminateGroup must target the whole process group (a negative pgid signal),
// not an individual pid, so a partial wrapper chain is never left behind. Both
// methods must be safe to call when the target has already vanished (a
// concurrent exit is a success, not an error).
type Actuator interface {
	// TerminateGroup terminates the process group pgid. pids is the observed
	// membership, passed for evidence and as a fallback; the primary action is
	// on the group.
	TerminateGroup(pgid int, pids []int) error
	// RemoveContainer removes the exited named container. Removing an
	// already-absent container is not an error.
	RemoveContainer(name string) error
}

// CleanupAction is the bounded kind of a recorded cleanup.
type CleanupAction string

const (
	// CleanupTerminateGroup records terminating one wrapper process group.
	CleanupTerminateGroup CleanupAction = "terminate_wrapper_group"
	// CleanupRemoveContainer records removing one exited named container.
	CleanupRemoveContainer CleanupAction = "remove_exited_container"
)

// CleanupOutcome is the bounded result of a cleanup action.
type CleanupOutcome string

const (
	CleanupSucceeded CleanupOutcome = "succeeded"
	CleanupFailed    CleanupOutcome = "failed"
)

// Cleanup is the durable, secret-free evidence record of one reaping action.
//
// It is deliberately a distinct type from a progress watermark. Reaping a
// zombie wrapper/container is a cleanup — never material progress — so recording
// it here (and never as a progress Observation) is what stops an orphan from
// being miscounted as the owning session making headway (#977 acceptance 4).
type Cleanup struct {
	At            time.Time      `json:"at"`
	Action        CleanupAction  `json:"action"`
	Outcome       CleanupOutcome `json:"outcome"`
	ContainerName string         `json:"container_name,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
	IssueNumber   int            `json:"issue_number,omitempty"`
	PGID          int            `json:"pgid,omitempty"`
	WrapperPIDs   []int          `json:"wrapper_pids,omitempty"`
	Reason        ReapReason     `json:"reason,omitempty"`
	// Detail is a bounded, secret-free note — an actuator error class at most,
	// never a host path, command line, or raw error containing one.
	Detail string `json:"detail,omitempty"`
}

// Reaper executes reap Plans against an Actuator and returns the durable
// Cleanup evidence for what it did. It holds no host state itself, so it is safe
// to construct per tick.
type Reaper struct {
	act Actuator
}

// NewReaper returns a Reaper bound to act.
func NewReaper(act Actuator) *Reaper {
	return &Reaper{act: act}
}

// Reap actuates plan and returns one Cleanup per attempted action, in a stable
// order (wrapper groups first, by pgid; then containers, by name). It terminates
// wrapper groups before removing containers so a wrapper cannot re-hold a
// container that is about to be removed. An actuator failure is recorded as a
// failed Cleanup and does not abort the remaining actions: one stuck orphan must
// not block reaping the others.
func (r *Reaper) Reap(plan Plan) []Cleanup {
	now := plan.EvaluatedAt.UTC()
	cleanups := make([]Cleanup, 0, len(plan.TerminateGroups)+len(plan.RemoveContainers))

	groups := append([]WrapperGroup(nil), plan.TerminateGroups...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].PGID < groups[j].PGID })
	for _, g := range groups {
		c := Cleanup{
			At:            now,
			Action:        CleanupTerminateGroup,
			ContainerName: g.ContainerName,
			SessionID:     g.SessionID,
			PGID:          g.PGID,
			WrapperPIDs:   append([]int(nil), g.PIDs...),
			Reason:        g.Reason,
		}
		if err := r.act.TerminateGroup(g.PGID, g.PIDs); err != nil {
			c.Outcome = CleanupFailed
			c.Detail = boundedErr(err)
		} else {
			c.Outcome = CleanupSucceeded
		}
		cleanups = append(cleanups, c)
	}

	containers := append([]Container(nil), plan.RemoveContainers...)
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	for _, ct := range containers {
		c := Cleanup{
			At:            now,
			Action:        CleanupRemoveContainer,
			ContainerName: ct.Name,
			SessionID:     ct.SessionID,
			IssueNumber:   ct.IssueNumber,
			Reason:        ReapTerminalContainerNonLiveSession,
		}
		if err := r.act.RemoveContainer(ct.Name); err != nil {
			c.Outcome = CleanupFailed
			c.Detail = boundedErr(err)
		} else {
			c.Outcome = CleanupSucceeded
		}
		cleanups = append(cleanups, c)
	}

	return cleanups
}

// boundedErr reduces an actuator error to a short, secret-free class token. The
// full error may embed a host path or command; only the leading context segment
// — the part maestro's own wrapping controls, before the first ':' — is kept.
//
// Go's error-wrapping convention is "context: cause", so the leading segment of
// an actuator error is a path-free class string this package authors ("sigterm
// group", "docker rm"). If that segment still contains a path separator (an
// error that leads with a host path rather than our context), it is dropped for
// a generic class so no host path can ever reach durable evidence.
func boundedErr(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "error"
	}
	if i := strings.IndexByte(msg, ':'); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	// Defense in depth: an empty or path-like leading segment is redacted to a
	// generic class rather than persisted.
	if msg == "" || strings.ContainsAny(msg, "/\\") {
		return "error"
	}
	if len(msg) > 48 {
		msg = msg[:48]
	}
	return msg
}

// SummarizeCleanups renders a bounded, secret-free operator summary of a batch
// of cleanups: counts of terminated groups and removed containers, how many
// failed, and the affected container names. Suitable for a Fleet evidence line
// or a runbook note — it contains no host path, command, or secret.
func SummarizeCleanups(cleanups []Cleanup) string {
	if len(cleanups) == 0 {
		return "orphan cleanup: none"
	}
	var terminated, removed, failed int
	nameSet := make(map[string]struct{})
	for _, c := range cleanups {
		if c.Outcome == CleanupFailed {
			failed++
		}
		switch c.Action {
		case CleanupTerminateGroup:
			terminated++
		case CleanupRemoveContainer:
			removed++
		}
		if c.ContainerName != "" {
			nameSet[c.ContainerName] = struct{}{}
		}
	}
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Sprintf("orphan cleanup: terminated %d wrapper group(s), removed %d exited container(s), %d failed; containers=[%s]",
		terminated, removed, failed, strings.Join(names, ","))
}
