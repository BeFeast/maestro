package supervisor

import "github.com/befeast/maestro/internal/config"

// BuildSelfCheckPrompt assembles a supervisor prompt from a fixed, held-out
// fixture state packet — exercising the real prompt-assembly path (template
// selection, {{STATE_PACKET}} substitution, and secret redaction) with no
// GitHub, network, or live-state input. It powers the prompt-assembly invariant
// of `maestro selfcheck` (#842): the supervisor prompt is the highest-stakes
// text the fleet-orchestrating binary produces, so a freshly-installed binary
// that cannot assemble it must not be finalized by self-deploy.
//
// It is deterministic: the packet is a constant and buildSupervisorPrompt only
// reads disk when cfg.Supervisor.Prompt names a custom template file, which the
// selfcheck fixture never sets (so the embedded defaultSupervisorPrompt is
// used).
func BuildSelfCheckPrompt(cfg *config.Config) (string, error) {
	return buildSupervisorPrompt(cfg, selfCheckStatePacket(cfg))
}

// selfCheckStatePacket returns a minimal, fully-deterministic state packet for
// the selfcheck prompt-assembly invariant. It carries just enough shape (the
// project repo and a no-op allowed-actions policy) that a correctly-assembled
// prompt provably contains the substituted packet.
func selfCheckStatePacket(cfg *config.Config) supervisorStatePacket {
	repo := ""
	if cfg != nil {
		repo = cfg.Repo
	}
	return supervisorStatePacket{
		ProjectConfig: supervisorProjectConfigPacket{
			Repo:          repo,
			MergeStrategy: "sequential",
			ReviewGate:    "greptile",
		},
		Policy: supervisorPolicyPacket{
			AllowedActions: []string{"none"},
		},
	}
}
