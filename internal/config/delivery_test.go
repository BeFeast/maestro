package config

import (
	"fmt"
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

func TestDeliveryConfig_ApprovalDigestBindsCheckoutAndSpec(t *testing.T) {
	base := DeliveryConfig{
		Mode: DeliveryModeApprovalRequired, Command: "./deploy.sh", VerifyCommand: "./verify.sh",
		LocalPath: "/srv/app", Target: "production", Rollback: "restore previous release",
		TargetLabel: "production kiosk", VerificationLabel: "service healthy", RollbackLabel: "previous release",
	}
	mutations := []DeliveryConfig{
		func() DeliveryConfig { d := base; d.Command = "./deploy-v2.sh"; return d }(),
		func() DeliveryConfig { d := base; d.VerifyCommand = "./verify-v2.sh"; return d }(),
		func() DeliveryConfig { d := base; d.LocalPath = "/srv/other"; return d }(),
		func() DeliveryConfig { d := base; d.TimeoutMinutes = 99; return d }(),
		func() DeliveryConfig { d := base; d.ApprovalTimeoutMinutes = 30; return d }(),
		func() DeliveryConfig { d := base; d.Target = "other"; return d }(),
		func() DeliveryConfig { d := base; d.TargetLabel = "other target"; return d }(),
		func() DeliveryConfig { d := base; d.VerificationLabel = "other check"; return d }(),
		func() DeliveryConfig { d := base; d.RollbackLabel = "other rollback"; return d }(),
	}
	for i, changed := range mutations {
		if changed.ApprovalDigest() == base.ApprovalDigest() {
			t.Fatalf("mutation %d did not change approval digest", i)
		}
	}
}

func TestDeliveryConfig_DefaultApprovalTimeout(t *testing.T) {
	if got := (DeliveryConfig{}).EffectiveApprovalTimeout(); got != 24*time.Hour {
		t.Fatalf("default approval timeout = %v, want 24h", got)
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

func TestDeliveryWarnings_AutomaticModeWithoutCommandInert(t *testing.T) {
	cfg := &Config{Delivery: DeliveryConfig{Mode: DeliveryModeAutomatic}}
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
  target_label: "production web"
  verification_label: "public health check"
  rollback_label: "previous release"
  verify_command: "./verify.sh"
`)
	cfg, err := ParseStrict(data)
	if err != nil {
		t.Fatalf("ParseStrict: %v", err)
	}
	d := cfg.EffectiveDelivery()
	if d.Mode != DeliveryModeApprovalRequired || d.Command != "./deploy.sh" ||
		d.TimeoutMinutes != 20 || d.Target != "prod web" || d.Rollback != "helm rollback app" ||
		d.TargetLabel != "production web" || d.VerificationLabel != "public health check" || d.RollbackLabel != "previous release" ||
		d.VerifyCommand != "./verify.sh" {
		t.Fatalf("parsed delivery = %+v", d)
	}
}

func TestParse_ApprovalRequiredNeedsVerifier(t *testing.T) {
	_, err := ParseStrict([]byte(`
repo: owner/app
delivery:
  mode: approval_required
  command: "./deploy.sh"
`))
	if err == nil || !strings.Contains(err.Error(), "delivery.verify_command is required") {
		t.Fatalf("ParseStrict err = %v, want required verifier", err)
	}
}

func TestParse_ApprovalRequiredNeedsCompleteSafeContext(t *testing.T) {
	base := `repo: owner/app
delivery:
  mode: approval_required
  command: "./deploy.sh"
  verify_command: "./verify.sh"
  target_label: "production web"
  verification_label: "public health check"
  rollback_label: "previous release"
`
	cases := []struct {
		name    string
		oldLine string
		newLine string
		want    string
	}{
		{"command", "  command: \"./deploy.sh\"\n", "", "delivery.command is required"},
		{"target label", "  target_label: \"production web\"\n", "", "delivery.target_label is required"},
		{"verification label", "  verification_label: \"public health check\"\n", "", "delivery.verification_label is required"},
		{"rollback label", "  rollback_label: \"previous release\"\n", "", "delivery.rollback_label is required"},
		{"rollback none reason", `  rollback_label: "previous release"`, `  rollback_label: "none"`, `reason after "none:"`},
		{"rollback empty none reason", `  rollback_label: "previous release"`, `  rollback_label: "none: "`, `reason after "none:"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseStrict([]byte(strings.Replace(base, tc.oldLine, tc.newLine, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseStrict err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := ParseStrict([]byte(strings.Replace(base, `  rollback_label: "previous release"`, `  rollback_label: "none: immutable appliance image"`, 1))); err != nil {
		t.Fatalf("explicit none reason rejected: %v", err)
	}
}

func TestParse_ApprovalRequiredRejectsShellAndUnpinnedEntrypoints(t *testing.T) {
	base := `repo: owner/app
delivery:
  mode: approval_required
  command: %q
  verify_command: %q
  target_label: "production web"
  verification_label: "public health check"
  rollback_label: "previous release"
`
	invalid := []string{
		"scripts/deploy.sh",
		"/usr/local/bin/deploy",
		"./.",
		"./..",
		"./scripts/../deploy.sh",
		"./scripts/deploy.sh --prod",
		"./scripts/deploy.sh;reboot",
		"./scripts/deploy.sh\n./other.sh",
		"./scripts/$DEPLOY",
	}
	for _, command := range invalid {
		for _, field := range []string{"command", "verify_command"} {
			t.Run(field+"_"+strings.ReplaceAll(command, "/", "_"), func(t *testing.T) {
				deploy, verify := "./scripts/deploy.sh", "./scripts/verify-delivery.sh"
				if field == "command" {
					deploy = command
				} else {
					verify = command
				}
				_, err := ParseStrict([]byte(fmt.Sprintf(base, deploy, verify)))
				if err == nil || !strings.Contains(err.Error(), "repo-relative executable path") {
					t.Fatalf("ParseStrict(%s=%q) err = %v, want strict entrypoint error", field, command, err)
				}
			})
		}
	}
}

func TestParse_ApprovalRequiredValidatesSafeLabelsBeforePersistence(t *testing.T) {
	base := `repo: owner/app
delivery:
  mode: approval_required
  command: "./scripts/deploy.sh"
  verify_command: "./scripts/verify.sh"
  target_label: %q
  verification_label: "healthy"
  rollback_label: "previous release"
`
	for _, label := range []string{strings.Repeat("ч", 257), "line\nbreak", "prod\u202eevil", "prod\u200bhidden"} {
		_, err := ParseStrict([]byte(fmt.Sprintf(base, label)))
		if err == nil || (!strings.Contains(err.Error(), "at most 256") &&
			!strings.Contains(err.Error(), "control characters") &&
			!strings.Contains(err.Error(), "format characters")) {
			t.Fatalf("label validation err = %v", err)
		}
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
