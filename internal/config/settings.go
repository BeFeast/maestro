package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Fleet-controllable settings: the cost/LLM control knobs (#839) that an
// operator can flip fleet-wide (a default in the config store's settings table)
// or per project (the project's own YAML). Precedence is
// project > fleet default > built-in, resolved by configstore.Load.
//
// SettingSource labels which layer supplied a key's effective value; it is
// surfaced on the fleet API's effective_config so Mission Control can highlight
// non-default overrides.
const (
	SettingSourceBuiltin = "builtin"
	SettingSourceFleet   = "fleet"
	SettingSourceProject = "project"
)

// Kinds a fleet setting value may take. Used to validate `settings set` input
// and to emit a correctly-typed YAML scalar when a fleet default is folded into
// a project document.
const (
	SettingKindBool   = "bool"
	SettingKindInt    = "int"
	SettingKindString = "string"
)

// FleetSettingSpec describes one fleet-controllable knob: its dotted key (the
// CLI / audit identifier), the path to it in a project YAML document (used by
// the config store to detect a per-project override and to inject a fleet
// default), its value kind (validation + scalar tag), and an accessor that
// returns the resolved value as a display string.
type FleetSettingSpec struct {
	Key      string
	YAMLPath []string
	Kind     string
	Value    func(*Config) string
}

// FleetSettingSpecs returns the ordered registry of fleet-controllable settings.
// Adding a knob here makes it settable via `maestro settings`, foldable as a
// fleet default, and visible with its source on effective_config — no other
// wiring required.
func FleetSettingSpecs() []FleetSettingSpec {
	return []FleetSettingSpec{
		{
			Key:      "supervisor.enabled",
			YAMLPath: []string{"supervisor", "enabled"},
			Kind:     SettingKindBool,
			Value:    func(c *Config) string { return strconv.FormatBool(c.Supervisor.Enabled) },
		},
		{
			Key:      "supervisor.backend",
			YAMLPath: []string{"supervisor", "backend"},
			Kind:     SettingKindString,
			Value:    func(c *Config) string { return c.Supervisor.Backend },
		},
		{
			Key:      "supervisor.model",
			YAMLPath: []string{"supervisor", "model"},
			Kind:     SettingKindString,
			Value:    func(c *Config) string { return c.Supervisor.Model },
		},
		{
			Key:      "supervisor.effort",
			YAMLPath: []string{"supervisor", "effort"},
			Kind:     SettingKindString,
			Value:    func(c *Config) string { return c.Supervisor.Effort },
		},
		{
			Key:      "supervisor.allow_metered_backend",
			YAMLPath: []string{"supervisor", "allow_metered_backend"},
			Kind:     SettingKindBool,
			Value:    func(c *Config) string { return strconv.FormatBool(c.Supervisor.AllowMeteredBackend) },
		},
		{
			Key:      "supervisor.always_consult_llm",
			YAMLPath: []string{"supervisor", "always_consult_llm"},
			Kind:     SettingKindBool,
			Value:    func(c *Config) string { return strconv.FormatBool(c.Supervisor.AlwaysConsultLLM) },
		},
		{
			Key:      "supervisor.spec_groom.enabled",
			YAMLPath: []string{"supervisor", "spec_groom", "enabled"},
			Kind:     SettingKindBool,
			Value:    func(c *Config) string { return strconv.FormatBool(c.Supervisor.SpecGroom.Enabled) },
		},
		{
			Key:      "supervisor.spec_groom.require_lint_pass",
			YAMLPath: []string{"supervisor", "spec_groom", "require_lint_pass"},
			Kind:     SettingKindBool,
			Value:    func(c *Config) string { return strconv.FormatBool(c.Supervisor.SpecGroom.RequireLintPass) },
		},
		{
			Key:      "poll_interval_seconds",
			YAMLPath: []string{"poll_interval_seconds"},
			Kind:     SettingKindInt,
			Value:    func(c *Config) string { return strconv.Itoa(c.PollIntervalSeconds) },
		},
		{
			Key:      "worker_max_tokens",
			YAMLPath: []string{"worker_max_tokens"},
			Kind:     SettingKindInt,
			Value:    func(c *Config) string { return strconv.Itoa(c.WorkerMaxTokens) },
		},
		{
			Key:      "stalled_progress_watchdog.enabled",
			YAMLPath: []string{"stalled_progress_watchdog", "enabled"},
			Kind:     SettingKindBool,
			Value:    func(c *Config) string { return strconv.FormatBool(c.StalledProgressWatchdog.IsEnabled()) },
		},
		{
			Key:      "stalled_progress_watchdog.max_silence_minutes",
			YAMLPath: []string{"stalled_progress_watchdog", "max_silence_minutes"},
			Kind:     SettingKindInt,
			Value:    func(c *Config) string { return strconv.Itoa(c.StalledProgressWatchdog.MaxSilenceMinutes) },
		},
	}
}

// FleetSettingSpecByKey returns the spec for a dotted key.
func FleetSettingSpecByKey(key string) (FleetSettingSpec, bool) {
	for _, s := range FleetSettingSpecs() {
		if s.Key == key {
			return s, true
		}
	}
	return FleetSettingSpec{}, false
}

// FleetSettingKeys returns the known dotted keys, in registry order.
func FleetSettingKeys() []string {
	specs := FleetSettingSpecs()
	keys := make([]string, len(specs))
	for i, s := range specs {
		keys[i] = s.Key
	}
	return keys
}

// NormalizeSettingValue validates value against key's kind and returns the
// canonical string form to store (e.g. "true"/"false" for bool, decimal for
// int, trimmed for string). An unknown key or an ill-typed value is an error,
// so a `settings set` typo can never land a value the config parser will choke
// on at reload time.
func NormalizeSettingValue(key, value string) (string, error) {
	spec, ok := FleetSettingSpecByKey(key)
	if !ok {
		return "", fmt.Errorf("unknown setting %q (known: %s)", key, strings.Join(FleetSettingKeys(), ", "))
	}
	v := strings.TrimSpace(value)
	switch spec.Kind {
	case SettingKindBool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return "", fmt.Errorf("setting %q wants a boolean (true/false), got %q", key, value)
		}
		return strconv.FormatBool(b), nil
	case SettingKindInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf("setting %q wants an integer, got %q", key, value)
		}
		return strconv.Itoa(n), nil
	default:
		return v, nil
	}
}
