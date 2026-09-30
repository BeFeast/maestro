package aiexecution

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/google/uuid"
)

// Revision invalidates captured per-project launch config at reload. An
// in-flight reviewer may hold a snapshot, but cannot start another child from
// it after the controller observed a newer config or stopped the flow.
type Revision struct {
	generation atomic.Uint64
	mu         sync.Mutex
	live       *os.File
}

var writeControllerRevision = replaceRevisionFile

func (r *Revision) Bind(p Policy) Policy {
	r.Invalidate()
	p.controllerPin = nil
	p.controllerLease = nil
	p.controllerUnavailable = false
	p.revision = r
	p.generation = r.generation.Add(1)
	return p
}
func (r *Revision) Invalidate() {
	r.generation.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live != nil {
		_ = r.live.Close()
		r.live = nil
	}
}
func (p Policy) CheckCurrent() error {
	if p.controllerUnavailable {
		return Held("controller_revision_unavailable")
	}
	if p.revision != nil && p.revision.generation.Load() != p.generation {
		return Held("project_config_changed")
	}
	if p.controllerPin != nil {
		if p.controllerLease == nil || p.controllerLease.Verify() != nil {
			return Held("controller_lease_unavailable")
		}
		if err := VerifyFile(*p.controllerPin); err != nil {
			return Held("project_config_changed")
		}
	}
	return nil
}
func (p Policy) Invalidate() error {
	if p.revision != nil {
		p.revision.Invalidate()
	}
	if p.controllerPin != nil {
		if err := writeControllerRevision(p.controllerPin.Path, []byte("invalidated")); err != nil {
			return Held("controller_revision_invalidation_failed")
		}
	}
	return nil
}

type ControllerLease struct {
	Live       FileProof `json:"live"`
	PID        int       `json:"pid"`
	UID        uint32    `json:"uid"`
	BootID     string    `json:"boot_id"`
	StartTicks string    `json:"start_ticks"`
}

func controllerIdentity(pid int) (ControllerLease, error) {
	var lease ControllerLease
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return lease, err
	}
	root := "/proc/" + strconv.Itoa(pid)
	info, err := os.Stat(root)
	if err != nil {
		return lease, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return lease, fmt.Errorf("controller identity unsupported")
	}
	b, err := os.ReadFile(root + "/stat")
	if err != nil {
		return lease, err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return lease, fmt.Errorf("controller identity invalid")
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 {
		return lease, fmt.Errorf("controller identity invalid")
	}
	return ControllerLease{PID: pid, UID: st.Uid, BootID: strings.TrimSpace(string(boot)), StartTicks: fields[19]}, nil
}

func (l ControllerLease) Verify() error {
	observed, err := controllerIdentity(l.PID)
	if err != nil || observed.UID != l.UID || observed.BootID != l.BootID || observed.StartTicks != l.StartTicks || filepath.Dir(l.Live.Path) != "/proc/"+strconv.Itoa(l.PID)+"/fd" {
		return Held("controller_instance_drift")
	}
	if err := VerifyFile(l.Live); err != nil {
		return Held("controller_lease_unavailable")
	}
	return nil
}

func (p Policy) LiveControllerLease() (ControllerLease, error) {
	if err := p.CheckCurrent(); err != nil {
		return ControllerLease{}, err
	}
	if p.controllerLease == nil {
		return ControllerLease{}, Held("controller_lease_unavailable")
	}
	return *p.controllerLease, nil
}

func (p Policy) ControllerPin() (FileProof, error) {
	if err := p.CheckCurrent(); err != nil {
		return FileProof{}, err
	}
	if p.controllerPin == nil {
		return FileProof{}, Held("controller_revision_unavailable")
	}
	return *p.controllerPin, nil
}

// BindController writes a fresh durable revision before any leaf uses the new
// snapshot. Detached worker exec processes compare the exact same pin.
func (p Policy) BindController(next Policy, stateDir string) Policy {
	bound := p.BindNext(next)
	if !bound.RequireVerifiedRoute {
		return bound
	}
	if !filepath.IsAbs(stateDir) {
		bound.controllerUnavailable = true
		return bound
	}
	path := filepath.Join(stateDir, ".ai-execution-revision")
	data := []byte(uuid.NewString())
	lease, err := controllerIdentity(os.Getpid())
	if err != nil {
		bound.controllerUnavailable = true
		return bound
	}
	live, err := newLiveRevisionPin(data)
	if err != nil {
		bound.controllerUnavailable = true
		return bound
	}
	bound.revision.mu.Lock()
	bound.revision.live = live
	bound.revision.mu.Unlock()
	lease.Live = FileProof{Path: fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), live.Fd()), SHA256: digest(data)}
	bound.controllerLease = &lease
	if err := writeControllerRevision(path, data); err != nil {
		bound.revision.Invalidate()
		bound.controllerUnavailable = true
		return bound
	}
	bound.controllerPin = &FileProof{Path: path, SHA256: digest(data)}
	return bound
}

func replaceRevisionFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".ai-revision-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (p Policy) BindNext(next Policy) Policy {
	if p.revision == nil {
		return new(Revision).Bind(next)
	}
	return p.revision.Bind(next)
}
