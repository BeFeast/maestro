// Package orphan detects and reaps orphaned container-client wrapper processes
// left behind by a container-backed verification command whose worker/session
// has already finished (#977).
//
// Incident that motivated it: a completed packaging worker left a parentless
// three-process `sudo docker run` / `docker run` wrapper chain alive for more
// than 11 hours. The named container itself had exited (code 127) about 17
// minutes after start and the retained worktree was gone, but the client
// wrappers remained with PPID=1. Maestro had already completed the authoritative
// later session and neither surfaced nor reaped the orphan.
//
// The invariant this package enforces: a container-backed verification command
// must not outlive its worker/session once the container is terminal. Parentless
// client wrappers tied to a terminal container and a non-live/superseded session
// are zombies, not active work.
//
// Design mirrors internal/progress: a pure leaf package (standard library only)
// with an injected clock so both the durable state layer and the runtime loops
// can share one decision without an import cycle, and so every decision is
// deterministic and unit-testable. No command line, mount, environment value,
// host path, or secret is ever modeled or persisted here — only bounded process
// and container identity plus lifecycle state.
//
// The package deliberately separates three concerns:
//
//   - Plan is a pure decision: given an observed snapshot of wrappers,
//     containers, and session liveness it returns exactly which process groups
//     to terminate and which exited containers to remove, and — just as
//     importantly — which observations were deliberately preserved and why.
//   - Reaper.Reap executes a Plan against an injected Actuator, so the host
//     side effects (signalling a process group, `docker rm`) are testable with a
//     fake and swappable for the real OSActuator.
//   - Cleanup is the durable, secret-free evidence record of what was reaped.
//     It is intentionally distinct from a progress watermark: reaping a zombie is
//     a cleanup, never material progress, so an orphan can never be miscounted as
//     the owning session making headway.
package orphan

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ContainerState is the observed lifecycle state of a named Docker container
// that backs a verification command. ContainerUnknown means the container was
// not observable this tick, which is loss of observability, not proof that it
// is terminal — the reaper treats it as a reason to preserve, never to reap.
type ContainerState string

const (
	ContainerRunning ContainerState = "running"
	ContainerCreated ContainerState = "created"
	ContainerExited  ContainerState = "exited"
	ContainerDead    ContainerState = "dead"
	ContainerUnknown ContainerState = ""
)

// IsTerminal reports whether the container has stopped executing and can never
// resume, so any client wrapper still blocked on it is waiting on nothing.
func (s ContainerState) IsTerminal() bool {
	return s == ContainerExited || s == ContainerDead
}

// IsActive reports whether the container is still (or not yet finished) running
// real work. An active container is always preserved: its wrappers are doing
// genuine work, not lingering.
func (s ContainerState) IsActive() bool {
	return s == ContainerRunning || s == ContainerCreated
}

// Container is the observed identity and lifecycle state of one named Docker
// container. It carries only bounded identity (the --name and correlated
// session/issue) and state — never the command line, mounts, env, or host path.
type Container struct {
	// Name is the docker --name assigned to the container. It is the join key
	// between a wrapper process and the container it launched.
	Name string
	// State is the observed lifecycle state.
	State ContainerState
	// ExitCode is the container's exit status when terminal (nil while running,
	// created, or unobservable). Recorded for evidence only; a non-zero exit is
	// not required for reaping — a terminal container is terminal regardless.
	ExitCode *int
	// SessionID is the owning maestro session, when the container could be
	// correlated back to one. Empty means the correlation is unknown, which the
	// reaper treats as non-live (a container no live session claims is not
	// active work).
	SessionID string
	// IssueNumber is the owning issue, for evidence only.
	IssueNumber int
	// FinishedAt is when the container reached a terminal state; zero while it
	// is still running or unobservable.
	FinishedAt time.Time
}

// Wrapper is one observed Docker client wrapper process in the run chain
// (`sudo docker run` / `docker run`). The incident chain was three such
// processes sharing one process group; the reaper terminates the group, not an
// individual pid, so a partial chain can never be left behind.
type Wrapper struct {
	// PID is the wrapper process id.
	PID int
	// PPID is the wrapper's parent pid. PPID==1 means it was reparented to init:
	// its launching worker/session process is gone.
	PPID int
	// PGID is the wrapper's process-group id. The reaper signals the whole group.
	PGID int
	// ContainerName is the --name of the container this wrapper is running, i.e.
	// the join key back to a Container.
	ContainerName string
	// SessionID is the owning maestro session, when known.
	SessionID string
	// StartedAt is when the wrapper process started, used only for the grace
	// window that avoids racing a just-launched command.
	StartedAt time.Time
}

// Parentless reports whether the wrapper has been reparented to init (PPID==1),
// i.e. the worker/session that launched it has already exited. A wrapper that
// still has a live, non-init parent is part of an active process tree and is
// never reaped by this package.
func (w Wrapper) Parentless() bool {
	return w.PPID == 1
}

// Snapshot is one complete, atomically-collected observation of the container
// client wrappers, the containers they name, and which sessions are still live.
// A complete snapshot is required so a single missing container reading cannot
// make an active wrapper look orphaned.
type Snapshot struct {
	// Wrappers is every observed docker client wrapper process.
	Wrappers []Wrapper
	// Containers is every observed named container relevant to those wrappers.
	Containers []Container
	// LiveSessions maps sessionID -> true for every session that is still doing
	// live work. A session absent from this map (or mapped to false) is
	// non-live/superseded: its container work, if terminal, may be reaped.
	LiveSessions map[string]bool
	// Now is the injected evaluation clock.
	Now time.Time
	// MinAge is the grace window: a parentless wrapper younger than MinAge is
	// preserved so the reaper never races a command that was launched moments
	// ago. Zero disables the grace window.
	MinAge time.Duration
}

// ReapReason is a bounded, secret-free token explaining why a group was reaped.
type ReapReason string

const (
	// ReapTerminalContainerNonLiveSession is the canonical incident: the named
	// container is terminal (exited/dead) and no live session owns it, yet a
	// parentless wrapper is still blocked on it.
	ReapTerminalContainerNonLiveSession ReapReason = "terminal_container_nonlive_session"
	// ReapContainerGoneParentlessWrapper covers a parentless wrapper whose named
	// container has already been removed (not observable at all): there is no
	// container left to wait on, so the wrapper is a definite zombie.
	ReapContainerGoneParentlessWrapper ReapReason = "container_gone_parentless_wrapper"
)

// PreserveReason is a bounded token explaining why an observation was kept.
type PreserveReason string

const (
	// PreserveContainerActive keeps every wrapper whose container is still
	// running/created: that is genuinely active work.
	PreserveContainerActive PreserveReason = "container_active"
	// PreserveSessionLive keeps a wrapper whose owning session is still live,
	// even if the container reading looked terminal — a live session is the
	// authority on whether its own container work is done.
	PreserveSessionLive PreserveReason = "session_live"
	// PreserveWrapperHasLiveParent keeps a wrapper that still has a non-init
	// parent: it is part of an active process tree, not an orphan.
	PreserveWrapperHasLiveParent PreserveReason = "wrapper_has_live_parent"
	// PreserveWithinGrace keeps a parentless wrapper younger than the grace
	// window so a just-launched command is never raced.
	PreserveWithinGrace PreserveReason = "within_grace_period"
	// PreserveContainerStateUnknown keeps a wrapper whose container could not be
	// observed this tick: loss of observability is not proof of a terminal
	// container, so the reaper fails safe.
	PreserveContainerStateUnknown PreserveReason = "container_state_unknown"
)

// WrapperGroup is one process group selected for termination. The reaper always
// operates on the whole group so a partial wrapper chain is never left behind.
type WrapperGroup struct {
	PGID          int
	PIDs          []int
	ContainerName string
	SessionID     string
	Reason        ReapReason
}

// Preservation records one observation the plan deliberately kept, with the
// bounded reason, so operator evidence can show the reaper is conservative:
// active containers and live sessions are never touched.
type Preservation struct {
	ContainerName string
	SessionID     string
	PID           int
	Reason        PreserveReason
}

// Plan is the pure decision for one snapshot: the process groups to terminate,
// the terminal containers safe to remove, and the observations preserved. It
// performs no side effects; Reaper.Reap actuates it.
type Plan struct {
	EvaluatedAt      time.Time
	TerminateGroups  []WrapperGroup
	RemoveContainers []Container
	Preserved        []Preservation
}

// Empty reports whether the plan reaps nothing.
func (p Plan) Empty() bool {
	return len(p.TerminateGroups) == 0 && len(p.RemoveContainers) == 0
}

// Decide computes the pure reap plan for a snapshot. It never mutates the
// snapshot and never performs a side effect.
//
// A wrapper is reaped only when every one of these holds:
//
//   - it is parentless (PPID==1) — its launching worker/session is gone;
//   - it is older than the grace window (so a just-launched command is safe);
//   - its named container is terminal (exited/dead) or has already been removed
//     — an active or unobservable container always preserves the wrapper;
//   - no live session owns the wrapper or its container — a live session is the
//     authority on its own in-flight container work.
//
// A terminal named container is scheduled for removal only when it is terminal,
// no live session owns it, and every wrapper still naming it is itself a reap
// candidate. That keeps removal safe even if the container reading and the
// process reading momentarily disagree.
func Decide(s Snapshot) Plan {
	now := s.Now.UTC()
	plan := Plan{EvaluatedAt: now}

	containersByName := make(map[string]Container, len(s.Containers))
	for _, c := range s.Containers {
		containersByName[strings.TrimSpace(c.Name)] = c
	}

	// A session is live only if it is explicitly present and true. Empty session
	// ids are never live (a wrapper/container no session claims is not work).
	sessionLive := func(id string) bool {
		id = strings.TrimSpace(id)
		if id == "" {
			return false
		}
		return s.LiveSessions[id]
	}

	// Group wrappers by process group so we terminate whole chains and can prove
	// every wrapper naming a container is reapable before removing it.
	type groupAcc struct {
		pgid          int
		containerName string
		sessionID     string
		pids          []int
		reason        ReapReason
		reap          bool
	}
	groups := make(map[int]*groupAcc)
	groupOrder := make([]int, 0)

	// containerReapable tracks, per container name, whether every wrapper naming
	// it was a reap candidate (starts true, cleared by any preserved wrapper).
	containerReapable := make(map[string]bool)
	containerSeenByWrapper := make(map[string]bool)

	for _, w := range s.Wrappers {
		name := strings.TrimSpace(w.ContainerName)
		containerSeenByWrapper[name] = true
		if _, ok := containerReapable[name]; !ok {
			containerReapable[name] = true
		}

		reap, reapReason, preserveReason := classifyWrapper(w, containersByName, sessionLive, now, s.MinAge)
		if !reap {
			containerReapable[name] = false
			plan.Preserved = append(plan.Preserved, Preservation{
				ContainerName: name,
				SessionID:     strings.TrimSpace(w.SessionID),
				PID:           w.PID,
				Reason:        preserveReason,
			})
			continue
		}

		acc := groups[w.PGID]
		if acc == nil {
			acc = &groupAcc{pgid: w.PGID, containerName: name, sessionID: strings.TrimSpace(w.SessionID), reason: reapReason}
			groups[w.PGID] = acc
			groupOrder = append(groupOrder, w.PGID)
		}
		acc.pids = append(acc.pids, w.PID)
		acc.reap = true
		if acc.containerName == "" {
			acc.containerName = name
		}
		if acc.sessionID == "" {
			acc.sessionID = strings.TrimSpace(w.SessionID)
		}
	}

	for _, pgid := range groupOrder {
		acc := groups[pgid]
		if !acc.reap {
			continue
		}
		pids := append([]int(nil), acc.pids...)
		sort.Ints(pids)
		plan.TerminateGroups = append(plan.TerminateGroups, WrapperGroup{
			PGID:          acc.pgid,
			PIDs:          pids,
			ContainerName: acc.containerName,
			SessionID:     acc.sessionID,
			Reason:        acc.reason,
		})
	}
	sort.Slice(plan.TerminateGroups, func(i, j int) bool {
		return plan.TerminateGroups[i].PGID < plan.TerminateGroups[j].PGID
	})

	// Schedule terminal, unowned containers for removal only when every wrapper
	// that still named them was reapable.
	names := make([]string, 0, len(containersByName))
	for name := range containersByName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := containersByName[name]
		if !c.State.IsTerminal() || sessionLive(c.SessionID) {
			continue
		}
		if containerSeenByWrapper[name] && !containerReapable[name] {
			// A wrapper naming this container was preserved (e.g. within grace):
			// leave the container until its wrappers are safe to reap.
			continue
		}
		plan.RemoveContainers = append(plan.RemoveContainers, c)
	}

	return plan
}

// classifyWrapper decides one wrapper in isolation. It returns whether to reap,
// the reap reason, and (when preserved) the preserve reason.
func classifyWrapper(w Wrapper, containers map[string]Container, sessionLive func(string) bool, now time.Time, minAge time.Duration) (bool, ReapReason, PreserveReason) {
	// A live parent means the wrapper is in an active process tree: never reap.
	if !w.Parentless() {
		return false, "", PreserveWrapperHasLiveParent
	}
	// A live owning session is the authority on its own in-flight work.
	if sessionLive(w.SessionID) {
		return false, "", PreserveSessionLive
	}

	name := strings.TrimSpace(w.ContainerName)
	c, known := containers[name]
	if known && sessionLive(c.SessionID) {
		return false, "", PreserveSessionLive
	}
	if known && c.State.IsActive() {
		return false, "", PreserveContainerActive
	}
	if known && c.State == ContainerUnknown {
		// The container name was reported but its state was unobservable: fail
		// safe, do not assume terminal.
		return false, "", PreserveContainerStateUnknown
	}

	// Grace window: a very recently started wrapper is preserved so a
	// just-launched command is never raced. Applied only after we know the
	// container is terminal/gone and the session is non-live.
	if minAge > 0 && !w.StartedAt.IsZero() && now.Sub(w.StartedAt.UTC()) < minAge {
		return false, "", PreserveWithinGrace
	}

	if !known {
		// The named container has already been removed: nothing left to wait on.
		return true, ReapContainerGoneParentlessWrapper, ""
	}
	// Known and terminal, non-live session.
	return true, ReapTerminalContainerNonLiveSession, ""
}

// String renders a bounded, secret-free one-line summary of the plan for
// operator evidence: counts only, plus the affected container names and process
// groups. No host path, command, or secret is included.
func (p Plan) String() string {
	if p.Empty() {
		return fmt.Sprintf("orphan reap: nothing to reap (%d preserved)", len(p.Preserved))
	}
	names := make([]string, 0, len(p.TerminateGroups))
	for _, g := range p.TerminateGroups {
		if g.ContainerName != "" {
			names = append(names, g.ContainerName)
		}
	}
	sort.Strings(names)
	return fmt.Sprintf("orphan reap: terminate %d wrapper group(s), remove %d exited container(s), %d preserved; containers=[%s]",
		len(p.TerminateGroups), len(p.RemoveContainers), len(p.Preserved), strings.Join(names, ","))
}
