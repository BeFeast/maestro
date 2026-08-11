// Package forge defines the minimal forge-agnostic client surface the
// llm-review producer needs (#1162): read one PR and its diff, read and write
// plain commit statuses, and post inline or plain PR comments. GitHub and
// Forgejo implement it; everything richer (check runs, labels, merges, issue
// lifecycle) stays on the forge-specific clients because Forgejo has no
// equivalent surface.
//
// The surface is deliberately the exact op set of scripts/llm-review.sh so the
// producer port (S4) is a transliteration, not a redesign. Implementations:
// GitHub in internal/github (this slice), Forgejo in a follow-up (S3).
package forge

import (
	"context"
	"time"
)

// PR is the subset of pull-request metadata the review producer consumes.
type PR struct {
	Number  int
	Title   string
	HeadSHA string
	BaseRef string
}

// StatusState is a commit-status state. The four values below are the write
// vocabulary both GitHub and Forgejo accept; implementations normalize their
// read side to the same strings (Forgejo reports a per-status `.status` field,
// not `.state` — its client maps that here).
type StatusState string

const (
	StatusPending StatusState = "pending"
	StatusSuccess StatusState = "success"
	StatusError   StatusState = "error"
	StatusFailure StatusState = "failure"
)

// Status is one commit status: the write payload of CreateCommitStatus and the
// read shape of CommitStatuses.
type Status struct {
	Context     string
	State       StatusState
	Description string
	TargetURL   string
	// CreatedAt is read-side only: the producer's stale-pending self-heal
	// compares it against the retry threshold. Zero when the forge omitted the
	// timestamp or reported one that does not parse — the producer treats a
	// pending with no usable age as crashed rather than wedging on it.
	CreatedAt time.Time
}

// Client is the forge-agnostic op surface of the review producer. repo is
// always "owner/name"; index is the PR number in the forge's shared PR/issue
// number space.
type Client interface {
	// GetPR returns the PR's current metadata. Implementations must reject a
	// response with an empty head SHA: every downstream op keys on it.
	GetPR(ctx context.Context, repo string, index int) (PR, error)
	// GetPRDiff returns the PR's unified diff, uninterpreted.
	GetPRDiff(ctx context.Context, repo string, index int) ([]byte, error)
	// CommitStatuses returns the commit statuses on one SHA, one entry per
	// context — the latest per context, which both forges' combined-status
	// endpoints already guarantee, so callers never dedup.
	CommitStatuses(ctx context.Context, repo, sha string) ([]Status, error)
	// CreateCommitStatus posts status.State/Description/TargetURL under
	// status.Context on the SHA. CreatedAt is ignored.
	CreateCommitStatus(ctx context.Context, repo, sha string, status Status) error
	// CreateReviewComment posts an inline PR comment anchored to a new-file
	// line of the diff at the given head SHA. A position the forge rejects
	// (line outside the diff, renamed file) surfaces as an error so the
	// caller can fall back to CreateComment instead of losing the finding.
	CreateReviewComment(ctx context.Context, repo string, index int, sha, path string, line int, body string) error
	// CreateComment posts a plain PR comment (the PR/issue comment space is
	// shared on both forges).
	CreateComment(ctx context.Context, repo string, index int, body string) error
}
