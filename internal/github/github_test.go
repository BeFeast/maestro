package github

import (
	"encoding/json"
	"testing"
)

// parseGreptileReview extracts greptile review status from raw JSON,
// mirroring the logic in GreptileReviewStatus.
func parseGreptileReview(data []byte) (string, string) {
	var result struct {
		Reviews []Review `json:"reviews"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", ""
	}
	for i := len(result.Reviews) - 1; i >= 0; i-- {
		r := result.Reviews[i]
		if r.Author.Login == "greptile-apps[bot]" {
			if r.State == "CHANGES_REQUESTED" {
				return "CHANGES_REQUESTED", r.Body
			}
			return "", ""
		}
	}
	return "", ""
}

func TestParseGreptileReview_ChangesRequested(t *testing.T) {
	data := []byte(`{"reviews":[
		{"author":{"login":"greptile-apps[bot]"},"state":"CHANGES_REQUESTED","body":"Please fix the error handling."}
	]}`)
	state, body := parseGreptileReview(data)
	if state != "CHANGES_REQUESTED" {
		t.Errorf("state = %q, want CHANGES_REQUESTED", state)
	}
	if body != "Please fix the error handling." {
		t.Errorf("body = %q, want review body", body)
	}
}

func TestParseGreptileReview_Commented(t *testing.T) {
	data := []byte(`{"reviews":[
		{"author":{"login":"greptile-apps[bot]"},"state":"COMMENTED","body":"Looks reasonable."}
	]}`)
	state, body := parseGreptileReview(data)
	if state != "" {
		t.Errorf("state = %q, want empty (COMMENTED should not block)", state)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestParseGreptileReview_NoGreptileReview(t *testing.T) {
	data := []byte(`{"reviews":[
		{"author":{"login":"some-human"},"state":"APPROVED","body":"LGTM"}
	]}`)
	state, body := parseGreptileReview(data)
	if state != "" {
		t.Errorf("state = %q, want empty (non-greptile reviews should be ignored)", state)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestParseGreptileReview_EmptyReviews(t *testing.T) {
	data := []byte(`{"reviews":[]}`)
	state, body := parseGreptileReview(data)
	if state != "" {
		t.Errorf("state = %q, want empty", state)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestParseGreptileReview_LatestReviewWins(t *testing.T) {
	// greptile first requests changes, then comments (approves implicitly)
	data := []byte(`{"reviews":[
		{"author":{"login":"greptile-apps[bot]"},"state":"CHANGES_REQUESTED","body":"Fix X"},
		{"author":{"login":"greptile-apps[bot]"},"state":"COMMENTED","body":"Looks good now"}
	]}`)
	state, _ := parseGreptileReview(data)
	if state != "" {
		t.Errorf("state = %q, want empty (latest COMMENTED review should not block)", state)
	}
}

func TestParseGreptileReview_LatestIsChangesRequested(t *testing.T) {
	// greptile comments first, then requests changes
	data := []byte(`{"reviews":[
		{"author":{"login":"greptile-apps[bot]"},"state":"COMMENTED","body":"Initial review"},
		{"author":{"login":"greptile-apps[bot]"},"state":"CHANGES_REQUESTED","body":"Actually, fix Y"}
	]}`)
	state, body := parseGreptileReview(data)
	if state != "CHANGES_REQUESTED" {
		t.Errorf("state = %q, want CHANGES_REQUESTED", state)
	}
	if body != "Actually, fix Y" {
		t.Errorf("body = %q, want 'Actually, fix Y'", body)
	}
}

func TestParseGreptileReview_MixedReviewers(t *testing.T) {
	// Other reviewers request changes, but greptile only commented — should not block
	data := []byte(`{"reviews":[
		{"author":{"login":"human-reviewer"},"state":"CHANGES_REQUESTED","body":"I don't like this"},
		{"author":{"login":"greptile-apps[bot]"},"state":"COMMENTED","body":"Automated review done"}
	]}`)
	state, _ := parseGreptileReview(data)
	if state != "" {
		t.Errorf("state = %q, want empty (only greptile matters, and it only COMMENTED)", state)
	}
}
