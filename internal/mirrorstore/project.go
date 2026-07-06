package mirrorstore

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ProjectWebhook projects one ingested GitHub webhook delivery into the mirror
// tables. eventType is the X-GitHub-Event value, payload is the raw request body
// (exactly what internal/webhookstore persisted), and receivedAt is when the
// daemon accepted the delivery — used as the ordering timestamp only when the
// payload carries no resource timestamp of its own.
//
// It is safe to call repeatedly with the same delivery (idempotent) and safe to
// call with deliveries out of order: each upsert is guarded by last_seen_at, so a
// replay or an out-of-order redelivery never regresses a row (#825 AC 2). Event
// types the mirror does not model (ping, the repo-level label definition event,
// check_suite roll-ups) are a no-op — the raw delivery still lives in
// webhookstore, so a later phase can widen coverage without re-ingesting.
//
// A malformed payload for a modelled event returns the JSON error; the caller
// (the ingestor's projection hook) logs it and moves on, because the raw
// delivery is already durably stored and the projection can be re-run.
func (s *Store) ProjectWebhook(ctx context.Context, eventType string, payload []byte, receivedAt time.Time) error {
	switch strings.TrimSpace(eventType) {
	case "issues":
		return s.projectIssue(ctx, payload, receivedAt)
	case "pull_request":
		return s.projectPullRequest(ctx, payload, receivedAt)
	case "issue_comment":
		return s.projectIssueComment(ctx, payload, receivedAt)
	case "pull_request_review_comment":
		return s.projectReviewComment(ctx, payload, receivedAt)
	case "pull_request_review":
		return s.projectReview(ctx, payload, receivedAt)
	case "check_run":
		return s.projectCheckRun(ctx, payload, receivedAt)
	case "status":
		return s.projectStatus(ctx, payload, receivedAt)
	case "projects_v2_item":
		return s.projectProjectItem(ctx, payload, receivedAt)
	default:
		// ping, label (definition), check_suite, and any not-yet-modelled event
		// are intentionally not projected. The raw delivery is still stored.
		return nil
	}
}

// repoEnvelope is the repository name every repo-scoped event carries.
type repoEnvelope struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type labeled struct {
	Name string `json:"name"`
}

func labelNames(ls []labeled) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		if n := strings.TrimSpace(l.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) projectIssue(ctx context.Context, payload []byte, receivedAt time.Time) error {
	var p struct {
		repoEnvelope
		Issue struct {
			Number    int       `json:"number"`
			Title     string    `json:"title"`
			Body      string    `json:"body"`
			State     string    `json:"state"`
			UpdatedAt string    `json:"updated_at"`
			CreatedAt string    `json:"created_at"`
			Labels    []labeled `json:"labels"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	repo := strings.TrimSpace(p.Repository.FullName)
	ts := pickTime(receivedAt, p.Issue.UpdatedAt, p.Issue.CreatedAt)
	applied, err := s.UpsertIssue(ctx, Issue{
		Repo:       repo,
		Number:     p.Issue.Number,
		Title:      p.Issue.Title,
		State:      p.Issue.State,
		Body:       p.Issue.Body,
		LastSeenAt: ts,
		Source:     SourceWebhook,
	})
	if err != nil {
		return err
	}
	// Only reconcile the label set when this event was not stale — otherwise a
	// late-arriving old event would clobber a fresher label set.
	if applied {
		return s.ReplaceLabels(ctx, repo, SubjectIssue, p.Issue.Number, labelNames(p.Issue.Labels), ts, SourceWebhook)
	}
	return nil
}

func (s *Store) projectPullRequest(ctx context.Context, payload []byte, receivedAt time.Time) error {
	var p struct {
		repoEnvelope
		PullRequest struct {
			Number    int    `json:"number"`
			Title     string `json:"title"`
			State     string `json:"state"`
			Draft     bool   `json:"draft"`
			Merged    bool   `json:"merged"`
			UpdatedAt string `json:"updated_at"`
			CreatedAt string `json:"created_at"`
			Head      struct {
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Labels []labeled `json:"labels"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	repo := strings.TrimSpace(p.Repository.FullName)
	ts := pickTime(receivedAt, p.PullRequest.UpdatedAt, p.PullRequest.CreatedAt)
	applied, err := s.UpsertPullRequest(ctx, PullRequest{
		Repo:       repo,
		Number:     p.PullRequest.Number,
		Title:      p.PullRequest.Title,
		State:      p.PullRequest.State,
		Draft:      p.PullRequest.Draft,
		Merged:     p.PullRequest.Merged,
		HeadSHA:    strings.TrimSpace(p.PullRequest.Head.SHA),
		BaseRef:    strings.TrimSpace(p.PullRequest.Base.Ref),
		LastSeenAt: ts,
		Source:     SourceWebhook,
	})
	if err != nil {
		return err
	}
	if applied {
		return s.ReplaceLabels(ctx, repo, SubjectPullRequest, p.PullRequest.Number, labelNames(p.PullRequest.Labels), ts, SourceWebhook)
	}
	return nil
}

func (s *Store) projectIssueComment(ctx context.Context, payload []byte, receivedAt time.Time) error {
	var p struct {
		repoEnvelope
		Issue struct {
			Number      int              `json:"number"`
			PullRequest *json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		Comment commentShape `json:"comment"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	// GitHub delivers comments on a PR as issue_comment too; the issue object then
	// carries a pull_request field. Record the correct subject type so a reader can
	// tell an issue-thread comment from a PR-conversation comment.
	subjectType := SubjectIssue
	if p.Issue.PullRequest != nil {
		subjectType = SubjectPullRequest
	}
	ts := pickTime(receivedAt, p.Comment.UpdatedAt, p.Comment.CreatedAt)
	_, err := s.UpsertComment(ctx, Comment{
		Repo:          strings.TrimSpace(p.Repository.FullName),
		CommentID:     p.Comment.ID,
		SubjectType:   subjectType,
		SubjectNumber: p.Issue.Number,
		Author:        strings.TrimSpace(p.Comment.User.Login),
		Body:          p.Comment.Body,
		LastSeenAt:    ts,
		Source:        SourceWebhook,
	})
	return err
}

func (s *Store) projectReviewComment(ctx context.Context, payload []byte, receivedAt time.Time) error {
	var p struct {
		repoEnvelope
		PullRequest struct {
			Number int `json:"number"`
		} `json:"pull_request"`
		Comment commentShape `json:"comment"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	ts := pickTime(receivedAt, p.Comment.UpdatedAt, p.Comment.CreatedAt)
	_, err := s.UpsertComment(ctx, Comment{
		Repo:          strings.TrimSpace(p.Repository.FullName),
		CommentID:     p.Comment.ID,
		SubjectType:   SubjectPullRequest,
		SubjectNumber: p.PullRequest.Number,
		Author:        strings.TrimSpace(p.Comment.User.Login),
		Body:          p.Comment.Body,
		LastSeenAt:    ts,
		Source:        SourceWebhook,
	})
	return err
}

// commentShape is shared by issue_comment and pull_request_review_comment.
type commentShape struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	UpdatedAt string `json:"updated_at"`
	CreatedAt string `json:"created_at"`
}

func (s *Store) projectReview(ctx context.Context, payload []byte, receivedAt time.Time) error {
	var p struct {
		repoEnvelope
		Review struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
			Body  string `json:"body"`
			User  struct {
				Login string `json:"login"`
			} `json:"user"`
			SubmittedAt string `json:"submitted_at"`
		} `json:"review"`
		PullRequest struct {
			Number int `json:"number"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	ts := pickTime(receivedAt, p.Review.SubmittedAt)
	_, err := s.UpsertReview(ctx, Review{
		Repo:       strings.TrimSpace(p.Repository.FullName),
		ReviewID:   p.Review.ID,
		PRNumber:   p.PullRequest.Number,
		Author:     strings.TrimSpace(p.Review.User.Login),
		State:      strings.TrimSpace(p.Review.State),
		Body:       p.Review.Body,
		LastSeenAt: ts,
		Source:     SourceWebhook,
	})
	return err
}

func (s *Store) projectCheckRun(ctx context.Context, payload []byte, receivedAt time.Time) error {
	var p struct {
		repoEnvelope
		CheckRun struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			Conclusion  string `json:"conclusion"`
			HeadSHA     string `json:"head_sha"`
			StartedAt   string `json:"started_at"`
			CompletedAt string `json:"completed_at"`
		} `json:"check_run"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	ts := pickTime(receivedAt, p.CheckRun.CompletedAt, p.CheckRun.StartedAt)
	_, err := s.UpsertCheck(ctx, Check{
		Repo:       strings.TrimSpace(p.Repository.FullName),
		HeadSHA:    strings.TrimSpace(p.CheckRun.HeadSHA),
		Name:       strings.TrimSpace(p.CheckRun.Name),
		Kind:       CheckKindRun,
		Status:     strings.TrimSpace(p.CheckRun.Status),
		Conclusion: strings.TrimSpace(p.CheckRun.Conclusion),
		LastSeenAt: ts,
		Source:     SourceWebhook,
	})
	return err
}

func (s *Store) projectStatus(ctx context.Context, payload []byte, receivedAt time.Time) error {
	var p struct {
		repoEnvelope
		SHA       string `json:"sha"`
		Context   string `json:"context"`
		State     string `json:"state"`
		UpdatedAt string `json:"updated_at"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	// A commit-status event has no separate status/conclusion split; its single
	// state (success/failure/pending/error) is recorded as both so a reader can
	// treat mirror_checks uniformly across check_run and status rows.
	state := strings.TrimSpace(p.State)
	ts := pickTime(receivedAt, p.UpdatedAt, p.CreatedAt)
	_, err := s.UpsertCheck(ctx, Check{
		Repo:       strings.TrimSpace(p.Repository.FullName),
		HeadSHA:    strings.TrimSpace(p.SHA),
		Name:       strings.TrimSpace(p.Context),
		Kind:       CheckKindStatus,
		Status:     state,
		Conclusion: state,
		LastSeenAt: ts,
		Source:     SourceWebhook,
	})
	return err
}

func (s *Store) projectProjectItem(ctx context.Context, payload []byte, receivedAt time.Time) error {
	var p struct {
		repoEnvelope
		ProjectsV2Item struct {
			NodeID      string `json:"node_id"`
			ContentType string `json:"content_type"`
			UpdatedAt   string `json:"updated_at"`
			CreatedAt   string `json:"created_at"`
		} `json:"projects_v2_item"`
		// The item's status is not on the item object; an "edited" delivery carries
		// the new value under changes.field_value.to.name. Best-effort: record it
		// when present, otherwise leave status empty (the row still marks presence).
		Changes struct {
			FieldValue struct {
				To struct {
					Name string `json:"name"`
				} `json:"to"`
			} `json:"field_value"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	itemID := strings.TrimSpace(p.ProjectsV2Item.NodeID)
	if itemID == "" {
		// Without a stable item id there is nothing to key on; skip rather than
		// collapse every unkeyed item onto one row.
		return nil
	}
	ts := pickTime(receivedAt, p.ProjectsV2Item.UpdatedAt, p.ProjectsV2Item.CreatedAt)
	_, err := s.UpsertProjectItem(ctx, ProjectItem{
		Repo:        strings.TrimSpace(p.Repository.FullName),
		ItemID:      itemID,
		ContentType: strings.TrimSpace(p.ProjectsV2Item.ContentType),
		Status:      strings.TrimSpace(p.Changes.FieldValue.To.Name),
		LastSeenAt:  ts,
		Source:      SourceWebhook,
	})
	return err
}

// pickTime returns the first parseable RFC3339 timestamp from candidates,
// falling back to fallback (the delivery's received_at) when none parse. GitHub
// resource timestamps (updated_at, submitted_at, completed_at, …) are the
// authoritative ordering key — two deliveries reordered in flight still resolve
// correctly because the newer resource state carries the later timestamp. Only
// when a payload omits every candidate does ordering fall back to arrival time.
func pickTime(fallback time.Time, candidates ...string) time.Time {
	for _, c := range candidates {
		if t, err := parseTS(c); err == nil && !t.IsZero() {
			return t.UTC()
		}
	}
	if fallback.IsZero() {
		return time.Time{}.UTC()
	}
	return fallback.UTC()
}
