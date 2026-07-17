package worker

import (
	"testing"
	"time"
)

func TestDetectCredentialRotationResult_StructuredModelCooldown(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	output := `API Error: Request rejected (429) · {"error":{"code":"model_cooldown","message":"All credentials for model claude-fable-5 are cooling down via provider claude","model":"claude-fable-5","provider":"claude","candidate_count":2,"usable_count":0,"aggregate_reason":"all_model_credentials_cooling_down","reset_seconds":75}}`

	result, ok := DetectCredentialRotationResult(output, now)
	if !ok {
		t.Fatal("expected structured model cooldown to parse")
	}
	if result.Provider != "claude" || result.Model != "claude-fable-5" {
		t.Fatalf("route = %q/%q, want claude/claude-fable-5", result.Provider, result.Model)
	}
	if !result.CandidatesKnown || result.Candidates != 2 || !result.UsableKnown || result.Usable != 0 {
		t.Fatalf("pool counts = %+v, want 2 candidates / 0 usable", result)
	}
	if result.AggregateReason != "all_model_credentials_cooling_down" {
		t.Fatalf("aggregate reason = %q", result.AggregateReason)
	}
	wantRetry := now.Add(75 * time.Second)
	if result.RetryAfter == nil || !result.RetryAfter.Equal(wantRetry) {
		t.Fatalf("retry after = %v, want %v", result.RetryAfter, wantRetry)
	}
}

func TestDetectCredentialRotationResult_CurrentProxyPayloadKeepsUnknownCandidateTotal(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	output := `{"error":{"code":"model_cooldown","message":"All credentials for model claude-fable-5 are cooling down","model":"claude-fable-5","reset_time":"30s","reset_seconds":30}}`

	result, ok := DetectCredentialRotationResult(output, now)
	if !ok {
		t.Fatal("expected current CLIProxyAPI payload to parse")
	}
	if result.CandidatesKnown {
		t.Fatalf("candidate total must remain unknown when proxy omitted it: %+v", result)
	}
	if !result.UsableKnown || result.Usable != 0 {
		t.Fatalf("model_cooldown must surface 0 usable credentials: %+v", result)
	}
	if result.RetryAfter == nil || !result.RetryAfter.Equal(now.Add(30*time.Second)) {
		t.Fatalf("retry after = %v", result.RetryAfter)
	}
}

func TestDetectCredentialRotationResult_DoesNotClassifySingleCredentialCooldown(t *testing.T) {
	output := "credential candidate 1 entered cooldown for claude-fable-5; selecting next candidate"
	if result, ok := DetectCredentialRotationResult(output, time.Now()); ok {
		t.Fatalf("single credential event must not gate the model route: %+v", result)
	}
}

func TestDetectCredentialRotationResult_PlainTextCompatibility(t *testing.T) {
	result, ok := DetectCredentialRotationResult(
		"API Error: Request rejected (429) · All credentials for model claude-fable-5 are cooling down",
		time.Now(),
	)
	if !ok || result.Model != "claude-fable-5" || !result.UsableKnown || result.Usable != 0 {
		t.Fatalf("plain-text result = %+v ok=%v", result, ok)
	}
	if result.Structured {
		t.Fatal("plain-text compatibility result must not claim structured metadata")
	}
}

func TestDetectCredentialRotationResult_DropsUnsafeAggregateDetail(t *testing.T) {
	output := `{"error":{"code":"model_cooldown","model":"claude-fable-5","aggregate_reason":"credential detail unavailable","reset_seconds":30}}`
	result, ok := DetectCredentialRotationResult(output, time.Now())
	if !ok {
		t.Fatal("expected model cooldown to parse")
	}
	if result.AggregateReason != "model_cooldown" {
		t.Fatalf("unsafe aggregate detail survived: %q", result.AggregateReason)
	}
}
