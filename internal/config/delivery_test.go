package config

import (
	"strings"
	"testing"
	"time"
)

// #872: a fresh project that configures a delivery command without naming a
// mode defaults to approval_required — the safe default that creates an
// auditable approval instead of an unattended deploy.
func TestEffectiveDelivery_DefaultsApprovalRequired(t *testing.T) {
	cfg := &Config{Delivery: DeliveryConfig{Command: "./deploy.sh"}}
	got := cfg.EffectiveDelivery()
	if got.Mode != DeliveryModeApprovalRequired {
		t.Fatalf("Mode = %q, want approval_required", got.Mode)
	}
	if got.TimeoutMinutes != deliveryTimeoutDefaultMinutes {
		t.Fatalf("TimeoutMinutes = %d, want %d", got.TimeoutMinutes, deliveryTimeoutDefaultMinutes)
	}
}

// A delivery block with no command and no mode resolves to disabled, not a
// surprising approval_required with nothing to run.
func TestEffectiveDelivery_EmptyBlockDisabled(t *testing.T) {
	cfg := &Config{Delivery: DeliveryConfig{Target: "prod"}}
	if got := cfg.EffectiveDelivery(); got.Mode != DeliveryModeDisabled {
		t.Fatalf("Mode = %q, want disabled", got.Mode)
	}
}

// Legacy deploy_cmd (no delivery block) folds into automatic mode so existing
// fleet projects keep their immediate post-merge deploy.
func TestEffectiveDelivery_LegacyDeployCmdIsAutomatic(t *testing.T) {
	cfg := &Config{DeployCmd: "make deploy", DeployTimeoutMinutes: 7}
	got := cfg.EffectiveDelivery()
	if got.Mode != DeliveryModeAutomatic {
		t.Fatalf("Mode = %q, want automatic", got.Mode)
	}
	if got.Command != "make deploy" {
		t.Fatalf("Command = %q, want make deploy", got.Command)
	}
	if got.TimeoutMinutes != 7 {
		t.Fatalf("TimeoutMinutes = %d, want 7", got.TimeoutMinutes)
	}
}

// An explicit delivery block wins over a legacy deploy_cmd — no silent
// double-deploy, and the explicit mode is honored.
func TestEffectiveDelivery_ExplicitBlockOverridesLegacy(t *testing.T) {
	cfg := &Config{
		DeployCmd: "make deploy",
		Delivery:  DeliveryConfig{Mode: DeliveryModeApprovalRequired, Command: "./ship.sh"},
	}
	got := cfg.EffectiveDelivery()
	if got.Mode != DeliveryModeApprovalRequired {
		t.Fatalf("Mode = %q, want approval_required", got.Mode)
	}
	if got.Command != "./ship.sh" {
		t.Fatalf("Command = %q, want ./ship.sh", got.Command)
	}
}

func TestEffectiveDelivery_NoDeliveryDisabled(t *testing.T) {
	cfg := &Config{}
	if got := cfg.EffectiveDelivery(); got.Mode != DeliveryModeDisabled {
		t.Fatalf("Mode = %q, want disabled", got.Mode)
	}
}

func TestDeliveryConfig_EffectiveTimeout(t *testing.T) {
	if d := (DeliveryConfig{}); d.EffectiveTimeout() != 15*time.Minute {
		t.Fatalf("default timeout = %v, want 15m", d.EffectiveTimeout())
	}
	if d := (DeliveryConfig{TimeoutMinutes: 3}); d.EffectiveTimeout() != 3*time.Minute {
		t.Fatalf("timeout = %v, want 3m", d.EffectiveTimeout())
	}
}

// Legacy deploy_cmd raises a deprecation warning steering to approval_required.
func TestDeliveryWarnings_LegacyDeployCmdDeprecated(t *testing.T) {
	cfg := &Config{DeployCmd: "make deploy"}
	if !hasWarning(cfg.Warnings(), "deploy_cmd is deprecated") {
		t.Fatalf("Warnings() = %v, want a deploy_cmd deprecation warning", cfg.Warnings())
	}
}

// A project migrated to delivery.mode: approval_required raises no legacy
// deprecation warning even when it keeps a deploy_cmd around.
func TestDeliveryWarnings_MigratedNoLegacyWarning(t *testing.T) {
	cfg := &Config{
		DeployCmd: "make deploy",
		Delivery:  DeliveryConfig{Mode: DeliveryModeApprovalRequired, Command: "./ship.sh"},
	}
	if hasWarning(cfg.Warnings(), "deploy_cmd is deprecated") {
		t.Fatalf("Warnings() = %v, want no deprecation warning once migrated", cfg.Warnings())
	}
}

// delivery.mode: automatic (the approval-gate opt-out) is surfaced loudly.
func TestDeliveryWarnings_AutomaticModeWarns(t *testing.T) {
	cfg := &Config{Delivery: DeliveryConfig{Mode: DeliveryModeAutomatic, Command: "./ship.sh"}}
	if !hasWarning(cfg.Warnings(), "delivery.mode: automatic") {
		t.Fatalf("Warnings() = %v, want an automatic-mode warning", cfg.Warnings())
	}
}

// approval_required is the safe default and raises no delivery warning.
func TestDeliveryWarnings_ApprovalRequiredSilent(t *testing.T) {
	cfg := &Config{Delivery: DeliveryConfig{Mode: DeliveryModeApprovalRequired, Command: "./ship.sh"}}
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "delivery") || strings.Contains(w, "deploy_cmd") {
			t.Fatalf("unexpected delivery warning: %q", w)
		}
	}
}

func TestDeliveryWarnings_InvalidMode(t *testing.T) {
	cfg := &Config{Delivery: DeliveryConfig{Mode: DeliveryMode("yolo"), Command: "x"}}
	if !hasWarning(cfg.Warnings(), "is invalid") {
		t.Fatalf("Warnings() = %v, want an invalid-mode warning", cfg.Warnings())
	}
}

func TestDeliveryWarnings_ModeWithoutCommandInert(t *testing.T) {
	cfg := &Config{Delivery: DeliveryConfig{Mode: DeliveryModeApprovalRequired}}
	if !hasWarning(cfg.Warnings(), "delivery.command is empty") {
		t.Fatalf("Warnings() = %v, want an inert-delivery warning", cfg.Warnings())
	}
}

// The delivery block parses off real YAML with strict decoding.
func TestParse_DeliveryBlock(t *testing.T) {
	data := []byte(`
repo: owner/app
delivery:
  mode: approval_required
  command: "./deploy.sh"
  timeout_minutes: 20
  target: "prod web"
  rollback: "helm rollback app"
  verify_command: "./verify.sh"
`)
	cfg, err := ParseStrict(data)
	if err != nil {
		t.Fatalf("ParseStrict: %v", err)
	}
	d := cfg.EffectiveDelivery()
	if d.Mode != DeliveryModeApprovalRequired || d.Command != "./deploy.sh" ||
		d.TimeoutMinutes != 20 || d.Target != "prod web" || d.Rollback != "helm rollback app" ||
		d.VerifyCommand != "./verify.sh" {
		t.Fatalf("parsed delivery = %+v", d)
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
