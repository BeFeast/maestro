package worker

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CredentialRotationResult is the secret-free aggregate result returned when
// CLIProxyAPI has tried the credential pool for one requested model. It never
// carries credential identifiers or raw provider output.
type CredentialRotationResult struct {
	Provider        string
	Model           string
	Candidates      int
	CandidatesKnown bool
	Usable          int
	UsableKnown     bool
	AggregateReason string
	RetryAfter      *time.Time
	Structured      bool
}

var proxyModelCooldownRe = regexp.MustCompile(`(?i)all credentials for model\s+(\S+)\s+are cooling down(?:\s+via provider\s+(\S+))?`)
var safeProviderModelTokenRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)
var safeAggregateReasonRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,119}$`)

// DetectCredentialRotationResult parses CLIProxyAPI's structured
// {"error":{"code":"model_cooldown",...}} response. Optional pool counters
// are accepted under the names used by proxy variants; when older proxies omit
// them, usable=0 remains known from model_cooldown semantics while the candidate
// total stays explicitly unknown. The plain-text message is supported as a
// compatibility fallback, without attempting to retain any credential detail.
func DetectCredentialRotationResult(output string, now time.Time) (CredentialRotationResult, bool) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if result, ok := parseCredentialRotationJSON(lines[i], now); ok {
			return result, true
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		match := proxyModelCooldownRe.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if len(match) == 0 {
			continue
		}
		provider := safeProviderModelToken(strings.TrimRight(match[2], `.,;:)]}`))
		model := safeProviderModelToken(strings.TrimRight(match[1], `.,;:)]}`))
		if model == "" {
			continue
		}
		return CredentialRotationResult{
			Provider:        provider,
			Model:           model,
			Usable:          0,
			UsableKnown:     true,
			AggregateReason: "model_cooldown",
		}, true
	}
	return CredentialRotationResult{}, false
}

func parseCredentialRotationJSON(line string, now time.Time) (CredentialRotationResult, bool) {
	for offset := 0; offset < len(line); offset++ {
		if line[offset] != '{' {
			continue
		}
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		decoder := json.NewDecoder(bytes.NewReader([]byte(line[offset:])))
		if err := decoder.Decode(&envelope); err != nil || len(envelope.Error) == 0 {
			continue
		}
		var detail map[string]json.RawMessage
		if err := json.Unmarshal(envelope.Error, &detail); err != nil {
			continue
		}
		if !strings.EqualFold(jsonString(detail["code"]), "model_cooldown") {
			continue
		}
		result := CredentialRotationResult{
			Provider:        safeProviderModelToken(jsonString(detail["provider"])),
			Model:           safeProviderModelToken(jsonString(detail["model"])),
			Usable:          0,
			UsableKnown:     true,
			AggregateReason: safeAggregateReason(firstNonEmpty(jsonString(detail["aggregate_reason"]), jsonString(detail["reason"]), "model_cooldown")),
			Structured:      true,
		}
		if value, ok := firstJSONInt(detail, "candidate_count", "candidates", "total_candidates", "total"); ok && value >= 0 {
			result.Candidates = value
			result.CandidatesKnown = true
		}
		if value, ok := firstJSONInt(detail, "usable_count", "usable", "usable_candidates", "available"); ok && value >= 0 {
			result.Usable = value
			result.UsableKnown = true
		}
		if retryAfter, ok := credentialRotationRetryAfter(detail, now); ok {
			result.RetryAfter = &retryAfter
		}
		if result.Model == "" {
			message := jsonString(detail["message"])
			if match := proxyModelCooldownRe.FindStringSubmatch(message); len(match) > 0 {
				result.Model = safeProviderModelToken(strings.TrimRight(match[1], `.,;:)]}`))
				if result.Provider == "" {
					result.Provider = safeProviderModelToken(strings.TrimRight(match[2], `.,;:)]}`))
				}
			}
		}
		if result.Model != "" {
			return result, true
		}
	}
	return CredentialRotationResult{}, false
}

func credentialRotationRetryAfter(detail map[string]json.RawMessage, now time.Time) (time.Time, bool) {
	const maxRetrySeconds = int((10 * 365 * 24 * time.Hour) / time.Second)
	if seconds, ok := firstJSONInt(detail, "reset_seconds", "retry_after_seconds"); ok && seconds >= 0 && seconds <= maxRetrySeconds {
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	if raw := jsonString(detail["retry_after"]); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 && seconds <= maxRetrySeconds {
			return now.Add(time.Duration(seconds) * time.Second), true
		}
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			return parsed.UTC(), true
		}
	}
	if raw := jsonString(detail["reset_time"]); raw != "" {
		if duration, err := time.ParseDuration(raw); err == nil && duration >= 0 {
			return now.Add(duration), true
		}
	}
	return time.Time{}, false
}

func firstJSONInt(values map[string]json.RawMessage, keys ...string) (int, bool) {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		var value int
		if err := json.Unmarshal(raw, &value); err == nil {
			return value, true
		}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			if parsed, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

func jsonString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func safeProviderModelToken(value string) string {
	value = strings.TrimSpace(value)
	if !safeProviderModelTokenRe.MatchString(value) {
		return ""
	}
	return value
}

func safeAggregateReason(value string) string {
	value = strings.TrimSpace(value)
	if !safeAggregateReasonRe.MatchString(value) {
		return "model_cooldown"
	}
	return value
}

// IsCredentialRotationUnavailable reads only the terminal worker-log tail so
// prompt text or earlier diagnostic output cannot create a model cooldown.
func IsCredentialRotationUnavailable(logFile string, now time.Time) (CredentialRotationResult, bool) {
	tail, ok := logTail(logFile, authFailureTailLines)
	if !ok {
		return CredentialRotationResult{}, false
	}
	return DetectCredentialRotationResult(tail, now)
}
