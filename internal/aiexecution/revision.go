package aiexecution

import "sync/atomic"

// Revision invalidates captured per-project launch config at reload. An
// in-flight reviewer may hold a snapshot, but cannot start another child from
// it after the controller observed a newer config or stopped the flow.
type Revision struct{ generation atomic.Uint64 }

func (r *Revision) Bind(p Policy) Policy {
	p.revision = r
	p.generation = r.generation.Add(1)
	return p
}
func (r *Revision) Invalidate() { r.generation.Add(1) }
func (p Policy) CheckCurrent() error {
	if p.revision != nil && p.revision.generation.Load() != p.generation {
		return Held("project_config_changed")
	}
	return nil
}
func (p Policy) Invalidate() {
	if p.revision != nil {
		p.revision.Invalidate()
	}
}
func (p Policy) BindNext(next Policy) Policy {
	if p.revision == nil {
		return new(Revision).Bind(next)
	}
	return p.revision.Bind(next)
}
