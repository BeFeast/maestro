package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// #1143: telegram.bot_token was the one credential still stored in plaintext in
// the config store. bot_token_env carries the env var name instead; plaintext
// keeps working but is named in Config.Warnings().

func TestTelegramConfig_TokenPrefersEnv(t *testing.T) {
	t.Setenv("MAESTRO_TEST_TG_TOKEN", "  env-token  ")
	cfg := TelegramConfig{BotToken: "plaintext-token", BotTokenEnv: "MAESTRO_TEST_TG_TOKEN"}
	if got := cfg.Token(); got != "env-token" {
		t.Fatalf("Token() = %q, want %q (env must win and be trimmed)", got, "env-token")
	}
	if cfg.PlaintextTokenActive() {
		t.Fatalf("PlaintextTokenActive() = true when the env var resolves")
	}
}

func TestTelegramConfig_TokenFromEnvOnly(t *testing.T) {
	t.Setenv("MAESTRO_TEST_TG_TOKEN", "env-only-token")
	cfg := TelegramConfig{BotTokenEnv: "MAESTRO_TEST_TG_TOKEN"}
	if got := cfg.Token(); got != "env-only-token" {
		t.Fatalf("Token() = %q, want %q", got, "env-only-token")
	}
	if cfg.PlaintextTokenActive() {
		t.Fatalf("PlaintextTokenActive() = true with no plaintext token set")
	}
}

func TestTelegramConfig_TokenPlaintextStillWorks(t *testing.T) {
	// Back-compat: an unmigrated row must keep notifying.
	cfg := TelegramConfig{BotToken: " plaintext-token "}
	if got := cfg.Token(); got != "plaintext-token" {
		t.Fatalf("Token() = %q, want %q", got, "plaintext-token")
	}
	if !cfg.PlaintextTokenActive() {
		t.Fatalf("PlaintextTokenActive() = false while the plaintext token is the one in use")
	}
}

func TestTelegramConfig_TokenFallsBackWhenEnvVarUnset(t *testing.T) {
	os.Unsetenv("MAESTRO_TEST_TG_MISSING")
	cfg := TelegramConfig{BotToken: "plaintext-token", BotTokenEnv: "MAESTRO_TEST_TG_MISSING"}
	if got := cfg.Token(); got != "plaintext-token" {
		t.Fatalf("Token() = %q, want the plaintext fallback %q", got, "plaintext-token")
	}
	if !cfg.PlaintextTokenActive() {
		t.Fatalf("PlaintextTokenActive() = false while the env var is unset")
	}
}

func TestTelegramConfig_TokenEmptyWhenNothingConfigured(t *testing.T) {
	if got := (TelegramConfig{}).Token(); got != "" {
		t.Fatalf("Token() = %q, want empty", got)
	}
}

func TestTelegramConfig_BotTokenEnvRoundTripsThroughYAML(t *testing.T) {
	const doc = "telegram:\n  target: \"123\"\n  bot_token_env: OK_GOBOT_TELEGRAM_TOKEN\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Telegram.BotTokenEnv != "OK_GOBOT_TELEGRAM_TOKEN" {
		t.Fatalf("BotTokenEnv = %q, want OK_GOBOT_TELEGRAM_TOKEN", cfg.Telegram.BotTokenEnv)
	}
	if cfg.Telegram.BotToken != "" {
		t.Fatalf("BotToken = %q, want empty", cfg.Telegram.BotToken)
	}

	// A migrated row must export no credential material.
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "bot_token_env: OK_GOBOT_TELEGRAM_TOKEN") {
		t.Fatalf("marshalled config lost bot_token_env:\n%s", out)
	}
}

func hasWarningContaining(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestWarnings_NamesPlaintextBotToken(t *testing.T) {
	cfg := &Config{Telegram: TelegramConfig{BotToken: "12345:secret-bot-token"}}
	warnings := cfg.Warnings()
	if !hasWarningContaining(warnings, "telegram.bot_token") {
		t.Fatalf("Warnings() = %v, want one naming telegram.bot_token", warnings)
	}
	if !hasWarningContaining(warnings, "telegram.bot_token_env") {
		t.Fatalf("Warnings() = %v, want the migration target telegram.bot_token_env named", warnings)
	}
	for _, w := range warnings {
		if strings.Contains(w, "12345:secret-bot-token") {
			t.Fatalf("warning leaked the credential value: %q", w)
		}
	}
}

func TestWarnings_SilentWhenBotTokenEnvOnly(t *testing.T) {
	t.Setenv("MAESTRO_TEST_TG_TOKEN", "env-token")
	cfg := &Config{Telegram: TelegramConfig{BotTokenEnv: "MAESTRO_TEST_TG_TOKEN"}}
	if hasWarningContaining(cfg.Warnings(), "telegram.bot_token") {
		t.Fatalf("Warnings() = %v, want no credential warning for a migrated row", cfg.Warnings())
	}
}

func TestWarnings_LeftoverPlaintextAlongsideResolvingEnv(t *testing.T) {
	t.Setenv("MAESTRO_TEST_TG_TOKEN", "env-token")
	cfg := &Config{Telegram: TelegramConfig{BotToken: "stale", BotTokenEnv: "MAESTRO_TEST_TG_TOKEN"}}
	if !hasWarningContaining(cfg.Warnings(), "delete telegram.bot_token") {
		t.Fatalf("Warnings() = %v, want the leftover-plaintext warning", cfg.Warnings())
	}
}

func TestWarnings_NamesInlineMCPCredentials(t *testing.T) {
	cfg := &Config{Model: ModelConfig{Backends: map[string]BackendDef{
		"claude": {MCP: MCPConfig{Servers: map[string]MCPServerDef{
			"symbols": {
				URL:     "https://symbols.example/mcp",
				Headers: map[string]string{"Authorization": "Bearer literal-secret", "X-Trace": "on"},
			},
			"local": {
				Command: "./mcp",
				Env:     map[string]string{"SERVICE_API_KEY": "literal-key", "REGION": "eu", "OTHER_TOKEN": "${FROM_ENV}"},
			},
		}}},
	}}}
	warnings := cfg.Warnings()
	want := []string{
		"model.backends.claude.mcp.servers.symbols.headers.Authorization",
		"model.backends.claude.mcp.servers.local.env.SERVICE_API_KEY",
	}
	for _, w := range want {
		if !hasWarningContaining(warnings, w) {
			t.Fatalf("Warnings() = %v, want one naming %s", warnings, w)
		}
	}
	for _, w := range warnings {
		for _, leak := range []string{"literal-secret", "literal-key"} {
			if strings.Contains(w, leak) {
				t.Fatalf("warning leaked a credential value: %q", w)
			}
		}
		for _, quiet := range []string{"X-Trace", "REGION", "OTHER_TOKEN"} {
			if strings.Contains(w, quiet) {
				t.Fatalf("warning fired on a non-credential / env-referenced field %s: %q", quiet, w)
			}
		}
	}
}

// TestNoProductionCodeReadsPlaintextBotToken keeps the notifier call sites on
// TelegramConfig.Token(). Reading cfg.Telegram.BotToken directly would silently
// bypass bot_token_env and re-break acceptance criterion 1 of #1143.
func TestNoProductionCodeReadsPlaintextBotToken(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	selfPkg, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
	}
	var offenders []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Dir(path) == selfPkg {
			return nil // TelegramConfig itself must read the field
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), "Telegram.BotToken") {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these files read the plaintext telegram bot token directly instead of config.TelegramConfig.Token(): %v", offenders)
	}
}
