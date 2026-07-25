package github

import (
	"testing"
	"time"
)

// Workflow review catch: commentScopedToHead must distinguish "the comment is
// older than this head" (proof it says nothing about the head) from "the
// timestamp could not be read" (nothing proven). Collapsing the two lets a
// rate-limited lookup look exactly like a silent reviewer.
func TestCommentScopedToHead_UnknownCommentTimeIsNotALookupFailure(t *testing.T) {
	c := &Client{Repo: "owner/repo"}
	scoped, lookupFailed := c.commentScopedToHead(issueComment{}, "deadbeef")
	if scoped {
		t.Fatal("a comment with no timestamp cannot be scoped to a head")
	}
	if lookupFailed {
		t.Fatal("a missing comment timestamp is a genuine 'not scoped', not a degraded read")
	}
}

func TestCommentScopedToHead_EmptySHAIsALookupFailure(t *testing.T) {
	c := &Client{Repo: "owner/repo"}
	comment := issueComment{CreatedAt: time.Now().UTC()}
	scoped, lookupFailed := c.commentScopedToHead(comment, "  ")
	if scoped {
		t.Fatal("no head to scope against")
	}
	if !lookupFailed {
		t.Fatal("without a head SHA nothing is proven — this must read as a degraded lookup")
	}
}

// The verdict must never claim silence when the read that would prove it failed.
func TestReviewGateVerdict_LookupFailurePropagates(t *testing.T) {
	v := ReviewGateVerdict{Passed: true}
	streams := []ReviewStreamVerdict{
		{Name: "greptile", Pending: true, Observed: false, LookupFailed: true},
		{Name: "simplicity", Pending: false, Passed: true, Observed: true},
	}
	for _, sv := range streams {
		if sv.Observed {
			v.Observed = true
		}
		if sv.LookupFailed {
			v.LookupFailed = true
		}
	}
	if !v.LookupFailed {
		t.Fatal("a degraded stream read must mark the whole gate verdict as unproven")
	}
}
