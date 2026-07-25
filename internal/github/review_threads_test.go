package github

import (
	"strings"
	"testing"
)

func TestParseReviewThreadsGraphQL_ReturnsOnlyCurrentUnresolvedThreads(t *testing.T) {
	data := []byte(`{
  "data": {"repository": {"pullRequest": {
    "headRefOid": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "reviewThreads": {
      "nodes": [
        {"id":"live","isResolved":false,"isOutdated":false,"path":"merge.go","line":42,"comments":{"nodes":[{"body":"P1: honor the hold","author":{"login":"chatgpt-codex-connector"}}]}},
        {"id":"resolved","isResolved":true,"isOutdated":false,"path":"old.go","line":1,"comments":{"nodes":[]}},
        {"id":"outdated","isResolved":false,"isOutdated":true,"path":"old.go","line":2,"comments":{"nodes":[]}}
      ],
      "pageInfo":{"hasNextPage":true,"endCursor":"next-page"}
    }
  }}}
}`)

	head, threads, next, hasNext, err := parseReviewThreadsGraphQL(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if head != strings.Repeat("a", 40) || next != "next-page" || !hasNext {
		t.Fatalf("page = head %q next %q hasNext %t", head, next, hasNext)
	}
	if len(threads) != 1 {
		t.Fatalf("threads = %+v, want one unresolved current thread", threads)
	}
	if got := threads[0]; got.ID != "live" || got.Path != "merge.go" || got.Line != 42 || got.Author != "chatgpt-codex-connector" {
		t.Fatalf("thread = %+v", got)
	}
}

func TestParseReviewThreadsGraphQL_FailsClosedOnGraphQLError(t *testing.T) {
	_, _, _, _, err := parseReviewThreadsGraphQL([]byte(`{"errors":[{"message":"reviewThreads unavailable"}]}`))
	if err == nil || !strings.Contains(err.Error(), "reviewThreads unavailable") {
		t.Fatalf("err = %v", err)
	}
}
