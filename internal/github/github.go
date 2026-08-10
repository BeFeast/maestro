package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type greptileCheckRun struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CreatedAt   string `json:"created_at"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	HTMLURL     string `json:"html_url"`
	DetailsURL  string `json:"details_url"`
	App         struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	} `json:"app"`
	Output struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Text    string `json:"text"`
	} `json:"output"`
}

type checkRunsResponse struct {
	CheckRuns []greptileCheckRun `json:"check_runs"`
}

type combinedStatusResponse struct {
	State    string `json:"state"`
	Statuses []struct {
		Context     string `json:"context"`
		State       string `json:"state"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	} `json:"statuses"`
}

type greptileReviewComment struct {
	Body             string `json:"body"`
	Path             string `json:"path"`
	Line             int    `json:"line"`
	CommitID         string `json:"commit_id"`
	OriginalCommitID string `json:"original_commit_id"`
	User             struct {
		Login string `json:"login"`
	} `json:"user"`
}

type issueComment struct {
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	// CreatedAt scopes a comment to a head. Issue comments carry no SHA, so a
	// bot comment from an earlier head is otherwise indistinguishable from one
	// about the current head.
	CreatedAt time.Time `json:"created_at"`
}

// IssueComment is the public view of a single issue comment. Unlike the
// internal issueComment (used only for PR Greptile scanning) it carries the
// comment ID and creation time so callers can track "have I already handled
// this comment" idempotently — required by the spec-groom mention trigger
// (#851), which reacts to `@maestro groom` at most once per comment.
type IssueComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	// State is the issue's lifecycle state ("open"/"closed") as GitHub reports it.
	// The REST issue payload always carries it; consumers that mirror issue state
	// (e.g. internal/mirrorstore hydration) rely on it so an already-closed issue
	// is not recorded as open. Empty when a payload omits it.
	State  string `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	ProjectItems []IssueProjectItem `json:"projectItems,omitempty"`
}

type IssueProjectItem struct {
	Title  string                  `json:"title,omitempty"`
	Status *IssueProjectItemStatus `json:"status,omitempty"`
}

type IssueProjectItemStatus struct {
	Name     string `json:"name,omitempty"`
	OptionID string `json:"optionId,omitempty"`
}

type PR struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	State       string `json:"state"`
	Mergeable   string `json:"mergeable"`
	Title       string `json:"title"`
	Body        string `json:"body,omitempty"`
	IsDraft     bool   `json:"isDraft"`
	MergedAt    string `json:"mergedAt,omitempty"`
}

type Client struct {
	Repo string
}

type RateLimitBucket struct {
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
	Reset     int `json:"reset"`
	Used      int `json:"used"`
}

type RateLimitStatus struct {
	Core    RateLimitBucket `json:"core"`
	GraphQL RateLimitBucket `json:"graphql"`
}

type restIssue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   *string `json:"body"`
	State  string  `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
}

type restPull struct {
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           *string `json:"body"`
	State          string  `json:"state"`
	Draft          bool    `json:"draft"`
	Mergeable      *bool   `json:"mergeable"`
	MergeableState string  `json:"mergeable_state"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
}

type prLabel struct {
	Name string `json:"name"`
}

type prCommit struct {
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

func (ri restIssue) issue() Issue {
	body := ""
	if ri.Body != nil {
		body = *ri.Body
	}
	return Issue{
		Number: ri.Number,
		Title:  ri.Title,
		Body:   body,
		State:  ri.State,
		Labels: ri.Labels,
	}
}

func (rp restPull) pr() PR {
	body := ""
	if rp.Body != nil {
		body = *rp.Body
	}
	mergedAt := ""
	if rp.MergedAt != nil {
		mergedAt = *rp.MergedAt
	}
	return PR{
		Number:      rp.Number,
		HeadRefName: rp.Head.Ref,
		State:       strings.ToUpper(rp.State),
		Title:       rp.Title,
		Body:        body,
		IsDraft:     rp.Draft,
		MergedAt:    mergedAt,
	}
}

func New(repo string) *Client {
	return &Client{Repo: repo}
}

func ghAPI(endpoint string) ([]byte, error) {
	return ghAPIWithArgs(endpoint)
}

// gh CLI rate-limit backoff (#794). The fleet runs 8 in-process flows polling
// GitHub on a single shared PAT. A synchronized, redundant burst trips GitHub's
// *secondary* (abuse / burst-concurrency) rate limit, which answers HTTP 403
// even when the primary quota is healthy. Before this, ghAPIWithArgs was a
// single-shot exec whose .Output() discarded stderr, so the real "403 rate
// limit" message degraded to an opaque `exit status 1` and the first failed
// read aborted the entire supervise / orchestrator cycle. We now:
//   - use CombinedOutput so gh's real error text (HTTP status + the
//     "rate limit" / "secondary rate limit" message) surfaces in logs, and
//   - retry a bounded number of times on a detected rate-limit response,
//     honoring an explicit Retry-After hint when gh surfaces one and otherwise
//     backing off exponentially (with jitter, capped) — so a transient 403
//     degrades to a brief stall + retry instead of a failed cycle.
//
// #797 layers three more protections on top: conditional (ETag/If-None-Match)
// requests so unchanged polling reads answer 304 and consume no quota, a
// per-process usage counter with an hourly journal digest, and a shutdown
// fail-fast mode (BeginShutdown) that skips retries so a rate-limited window
// cannot stall a drain.
const (
	ghAPIMaxAttempts = 4
	ghAPIBaseBackoff = 2 * time.Second
	ghAPIMaxBackoff  = 20 * time.Second
)

// ghAPIRunner runs `gh <args...>` and returns combined stdout+stderr plus the
// process error. Indirection point so tests can drive the retry loop without a
// real gh binary.
var ghAPIRunner = func(args ...string) ([]byte, error) {
	return ghCommand(args...).CombinedOutput()
}

// ghTimeout is the maximum wall time allowed for a single gh subprocess call.
// It is the package-wide bound: ghCommand applies it to every plain `gh` call
// site, and the GraphQL Projects calls in github_projects.go pass it to their
// own contexts. A var rather than a const only so tests can shrink it;
// production never reassigns it.
var ghTimeout = 30 * time.Second

// ghExecutable is the gh binary, resolved through PATH exactly like the bare
// exec.Command call it replaced. A var only so a test can point the bounded
// wiring at a stub instead of the real CLI.
var ghExecutable = "gh"

// ghCommand builds a bounded exec.Cmd for `gh <args...>` with GitHub App auth
// injected when configured. All direct `gh` call sites in this package use it
// instead of exec.Command so installation-token auth is uniform (#823) and so
// no single gh invocation can hang a supervise cycle forever (#1119): this was
// a plain exec.Command with no deadline behind 20 call sites — the entire read
// surface the supervisor cycle depends on — so a gh that never returned wedged
// RunOnce with nothing above it able to abort the subprocess.
//
// The deadline context deliberately outlives this function: exec.CommandContext
// requires its context to stay live until Wait returns, so cancel cannot be
// deferred here. It is invoked from Cancel when the deadline fires; when the
// process exits first the context is released by its own timer at the deadline,
// so the retained resource is one armed timer for at most ghTimeout. Note the
// clock starts when the command is built, not when it is started — every call
// site in this package builds and runs in a single expression.
func ghCommand(args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	cmd := ghCommandContext(ctx, args...)
	kill := cmd.Cancel
	cmd.Cancel = func() error {
		defer cancel()
		return kill()
	}
	return cmd
}

// ghCommandContext is the context-aware sibling of ghCommand, used by the
// GraphQL Projects calls in github_projects.go so they authenticate with the
// installation token when App auth is active (#823).
func ghCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, ghExecutable, args...)
	// Kill the whole process group, not just gh itself (#1119). gh spawns
	// helpers (credential, pager, hooks) and a surviving child that inherited
	// the output pipe keeps Wait blocked long after the leader is gone — the
	// same hang the deadline exists to prevent. Mirrors the delivery-freshness
	// fence in trustedGHCommandContext.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	// Bound the post-kill wait too: Wait blocks until the output pipes close,
	// so a stray descriptor holder must not turn a deadline into a hang.
	cmd.WaitDelay = 2 * time.Second
	ghApplyAuth(cmd)
	return cmd
}

// ghApplyAuth sets GH_TOKEN on cmd's environment when GitHub App installation
// auth is active. When App auth is not configured, or a refresh failed, appToken
// returns an empty token and cmd is left untouched — the process falls back to
// the ambient gh auth (PAT path), so behavior is byte-identical to pre-#823. A
// refresh failure was already logged loudly by appToken.
func ghApplyAuth(cmd *exec.Cmd) {
	_ = ghApplyAuthContext(context.Background(), cmd)
}

// ghApplyAuthContext is the delivery-safe sibling of ghApplyAuth. It lets the
// caller's freshness deadline cancel both an installation-token refresh and a
// wait for another goroutine's in-flight refresh. Non-context callers keep the
// legacy PAT fallback behavior through ghApplyAuth.
func ghApplyAuthContext(ctx context.Context, cmd *exec.Cmd) error {
	token, err := appTokenContext(ctx)
	if token == "" {
		return err
	}
	// Inherit the current environment and override GH_TOKEN.
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "GH_TOKEN="+token)
	return err
}

// ghAPISleep is the backoff sleeper, injectable so tests do not actually wait.
// The default waits on a timer but returns early once BeginShutdown fires, so
// an in-flight backoff cannot hold up daemon stop (#797).
var ghAPISleep = func(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ghShutdownChan():
	}
}

// ghAPIJitterFrac yields a random fraction in [0,1) used to spread retry
// backoff so the fleet's flows do not re-burst in lockstep after a shared 403.
var ghAPIJitterFrac = rand.Float64

var ghRetryAfterRe = regexp.MustCompile(`(?i)retry-after:\s*(\d+)`)

// X-RateLimit-* headers appear in a `gh api --include` exchange (the shape the
// conditional/ETag layer already uses for polling reads). A primary (hourly)
// exhaustion answers X-RateLimit-Remaining: 0 with X-RateLimit-Reset carrying
// the epoch the bucket refills at — the two signals #812 uses to fail fast and
// pause REST polling until reset instead of retrying a doomed call.
var (
	ghRateLimitRemainingRe = regexp.MustCompile(`(?im)^\s*x-ratelimit-remaining:\s*(\d+)`)
	ghRateLimitResetRe     = regexp.MustCompile(`(?im)^\s*x-ratelimit-reset:\s*(\d+)`)
)

// ---------------------------------------------------------------------------
// Shutdown fail-fast (#797). During the 2026-07-02 incident a drain that had
// ZERO in-flight workers took 20+ minutes to stop because the still-live flows
// kept polling a rate-limited GitHub and every read sat in the #794 backoff
// loop. Once shutdown begins, a rate-limited response must degrade to a failed
// cycle (retried after restart / next tick), never to minutes of sleeping.
// ---------------------------------------------------------------------------

var (
	ghShutdownMu    sync.Mutex
	ghShutdownCh    = make(chan struct{})
	ghShutdownBegun bool
)

// BeginShutdown switches the gh wrapper into fail-fast mode for the rest of
// the process lifetime: rate-limited responses are no longer retried and a
// backoff already sleeping is cut short. The daemon calls it when drain or
// teardown starts so a rate-limited window cannot block flow stop (#797).
// Idempotent.
func BeginShutdown() {
	ghShutdownMu.Lock()
	defer ghShutdownMu.Unlock()
	if !ghShutdownBegun {
		ghShutdownBegun = true
		close(ghShutdownCh)
	}
}

// ShuttingDown reports whether BeginShutdown has been called.
func ShuttingDown() bool {
	select {
	case <-ghShutdownChan():
		return true
	default:
		return false
	}
}

func ghShutdownChan() <-chan struct{} {
	ghShutdownMu.Lock()
	defer ghShutdownMu.Unlock()
	return ghShutdownCh
}

// resetShutdownForTest re-arms the one-way shutdown flag so tests can exercise
// the fail-fast path without poisoning the rest of the test binary.
func resetShutdownForTest() {
	ghShutdownMu.Lock()
	defer ghShutdownMu.Unlock()
	ghShutdownCh = make(chan struct{})
	ghShutdownBegun = false
}

// ---------------------------------------------------------------------------
// Conditional (ETag) request layer (#797). The fleet's steady-state REST
// consumption is dominated by per-cycle polling reads (open-PR lists, per-PR
// lookups, check-runs, combined status) whose responses rarely change between
// cycles. GitHub answers a conditional GET whose ETag still matches with
// 304 Not Modified WITHOUT consuming the hourly REST quota, so the wrapper
// remembers each endpoint's last ETag + body and replays the request with
// If-None-Match. A 304 is served from the local copy for free; anything else
// refreshes the cache. Merge-gating correctness is unchanged: a 304 is
// GitHub's own guarantee that a 200 would have returned identical content.
// ---------------------------------------------------------------------------

const (
	// etagCacheMaxEntries bounds the endpoint cache. The steady-state working
	// set is a few dozen endpoints per flow, so this covers a large fleet;
	// eviction is a safety valve, not a tuning knob.
	etagCacheMaxEntries = 1024
	// etagCacheMaxBody skips caching pathologically large responses so a
	// long-lived daemon's memory stays bounded.
	etagCacheMaxBody = 1 << 20
)

type etagEntry struct {
	etag     string
	body     []byte
	lastUsed time.Time
}

var (
	etagMu    sync.Mutex
	etagCache = map[string]*etagEntry{}
)

func etagLookup(key string) (etag string, body []byte) {
	etagMu.Lock()
	defer etagMu.Unlock()
	e, ok := etagCache[key]
	if !ok {
		return "", nil
	}
	e.lastUsed = time.Now()
	return e.etag, e.body
}

func etagStore(key, etag string, body []byte) {
	if etag == "" || len(body) > etagCacheMaxBody {
		etagDrop(key)
		return
	}
	etagMu.Lock()
	defer etagMu.Unlock()
	if _, exists := etagCache[key]; !exists && len(etagCache) >= etagCacheMaxEntries {
		evictOldestETagLocked()
	}
	etagCache[key] = &etagEntry{etag: etag, body: body, lastUsed: time.Now()}
}

func etagDrop(key string) {
	etagMu.Lock()
	defer etagMu.Unlock()
	delete(etagCache, key)
}

func evictOldestETagLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range etagCache {
		if oldestKey == "" || e.lastUsed.Before(oldest) {
			oldestKey, oldest = k, e.lastUsed
		}
	}
	if oldestKey != "" {
		delete(etagCache, oldestKey)
	}
}

// ghConditionalEligible reports whether a gh api call can carry conditional
// request headers. Plain GETs — no extra args, or --paginate only — are
// eligible; whether a paginated response may actually be cached is decided per
// response by paginatedCacheable, and multi-page responses are never cached.
// rate_limit is excluded: its body changes on every read, so a conditional
// fetch would never hit.
func ghConditionalEligible(endpoint string, args []string) bool {
	if endpoint == "rate_limit" {
		return false
	}
	for _, a := range args {
		if a != "--paginate" {
			return false
		}
	}
	return true
}

// etagCacheKey namespaces the conditional cache by whether the call paginates.
// A --paginate reconcile read and a plain single-page read can target the SAME
// endpoint string — ListAllOpenIssues and ListOpenIssues both hit
// `.../issues?state=open&per_page=100` — but their cached bodies are NOT
// interchangeable. A non-paginated read caches only page one; replaying that
// page-one ETag for a --paginate read could earn a 304 whose cached body is
// just those first 100 rows, silently truncating the authoritative open set the
// reconciler closes rows against (a still-open issue past #100 would look
// missing and be stamped closed — #827 review). Separate namespaces guarantee a
// paginated read only ever revalidates against a body a paginated read itself
// stored, and paginated bodies are cached only when paginatedCacheable proved
// one page holds the whole collection.
func etagCacheKey(endpoint string, paginated bool) string {
	if paginated {
		return "\x00paginate\x00" + endpoint
	}
	return endpoint
}

// ghHTTPStatusLine matches the status line gh prints at the start of each
// header block under --include, anchored at line start. It cannot collide with
// response content: GitHub's JSON escapes control characters inside strings,
// so a raw newline followed by "HTTP/" only ever comes from gh itself.
var ghHTTPStatusLine = regexp.MustCompile(`(?m)^HTTP/[0-9.]+ (\d{3})`)

// ghHTTPResponse is a parsed `gh api --include` exchange. With --paginate gh
// prints one header block per page; body is every page's body concatenated
// (the same shape a plain --paginate call yields) while status/etag describe
// the FIRST block.
type ghHTTPResponse struct {
	status int
	etag   string
	body   []byte
	blocks int
	allOK  bool // every block's status was 2xx
}

// parseGHConditionalResponse splits combined --include output into status,
// ETag, and body. ok=false when out does not start with a header block (e.g.
// gh did not honor --include, or the output is only an error message); the
// caller should then treat out as a plain body / error text.
func parseGHConditionalResponse(out []byte) (ghHTTPResponse, bool) {
	locs := ghHTTPStatusLine.FindAllSubmatchIndex(out, -1)
	if len(locs) == 0 || locs[0][0] != 0 {
		return ghHTTPResponse{}, false
	}
	resp := ghHTTPResponse{blocks: len(locs), allOK: true}
	var body []byte
	for i, loc := range locs {
		status, convErr := strconv.Atoi(string(out[loc[2]:loc[3]]))
		if convErr != nil {
			return ghHTTPResponse{}, false
		}
		blockEnd := len(out)
		if i+1 < len(locs) {
			blockEnd = locs[i+1][0]
		}
		headers, blockBody := splitHeadersAndBody(out[loc[0]:blockEnd])
		if i == 0 {
			resp.status = status
			resp.etag = headerValue(headers, "etag")
		}
		if status < 200 || status > 299 {
			resp.allOK = false
		}
		body = append(body, blockBody...)
	}
	resp.body = body
	return resp, true
}

// splitHeadersAndBody splits one header block from its body at the first blank
// line. A block with no blank line (e.g. a bodyless 304) is all headers.
func splitHeadersAndBody(block []byte) (headers, body []byte) {
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if idx := bytes.Index(block, sep); idx >= 0 {
			return block[:idx], block[idx+len(sep):]
		}
	}
	return block, nil
}

func headerValue(headers []byte, name string) string {
	for _, line := range strings.Split(string(headers), "\n") {
		line = strings.TrimRight(line, "\r")
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// resolveConditional interprets a --include exchange for a conditional GET.
// served=true means GitHub answered 304 Not Modified and body is the cached
// copy — the request consumed no quota. resolved=false means the exchange did
// not yield a trustworthy body: with runErr != nil the caller falls through to
// normal rate-limit/error handling; with runErr == nil (an inconsistent
// conditional shape, e.g. a paginated response embedding a non-2xx page) the
// caller must drop the cache entry and refetch plain. paginated marks a
// --paginate call, whose responses are cached only when paginatedCacheable
// proves a later 304 cannot hide pages beyond the cached one. cacheKey is the
// namespaced storage key (see etagCacheKey); endpoint is the raw path, used
// only to read the requested per_page.
func resolveConditional(cacheKey, endpoint string, out []byte, runErr error, cachedBody []byte, paginated bool) (body []byte, served bool, resolved bool) {
	resp, ok := parseGHConditionalResponse(out)
	if !ok {
		// No header block. Trust a successful run's output as the plain body
		// (gh ignored --include); a 304 whose headers were swallowed still
		// shows up in gh's error text and is safely served from cache.
		if runErr == nil {
			return out, false, true
		}
		if cachedBody != nil && strings.Contains(string(out), "HTTP 304") {
			return cachedBody, true, true
		}
		return nil, false, false
	}
	if resp.status == 304 && cachedBody != nil {
		return cachedBody, true, true
	}
	if runErr == nil && resp.allOK {
		if resp.blocks == 1 && (!paginated || paginatedCacheable(resp.body, endpointPerPage(endpoint))) {
			etagStore(cacheKey, resp.etag, resp.body)
		} else {
			// A multi-page response cannot be cached: the first page's ETag
			// does not validate the concatenated whole. A full single page is
			// not cached either — see paginatedCacheable.
			etagDrop(cacheKey)
		}
		return resp.body, false, true
	}
	return nil, false, false
}

// paginatedCacheable reports whether a single-page --paginate body may be
// cached for If-None-Match revalidation. A 304 only certifies that page ONE is
// unchanged: if the collection has meanwhile grown past the page size, the new
// items live on a page the bodyless 304 carries no Link header to discover,
// and serving the cache would hide them (e.g. review comments appended past
// item 100). Caching is therefore limited to shapes where an unchanged first
// page proves the whole collection is unchanged:
//
//   - a JSON array with fewer than per_page items — any growth lands on this
//     page and changes its bytes;
//   - a JSON object carrying GitHub's total_count (check-runs et al.) — growth
//     anywhere changes the count on page one.
//
// A full single page (exactly per_page array items) is the one shape that can
// overflow invisibly, so it is never cached.
func paginatedCacheable(body []byte, perPage int) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return false
		}
		return len(items) < perPage
	case '{':
		var probe struct {
			TotalCount *int64 `json:"total_count"`
		}
		return json.Unmarshal(trimmed, &probe) == nil && probe.TotalCount != nil
	}
	return false
}

var ghPerPageRE = regexp.MustCompile(`[?&]per_page=(\d+)`)

// endpointPerPage returns the page size a paginated endpoint requested,
// defaulting to GitHub's 30 when the query string does not say.
func endpointPerPage(endpoint string) int {
	if m := ghPerPageRE.FindStringSubmatch(endpoint); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	return 30
}

// ghConditionalErrorDetail returns the body portion of a --include response so
// error logs surface GitHub's message rather than a screenful of headers; out
// is returned unchanged when it carries no header block.
func ghConditionalErrorDetail(out []byte) []byte {
	if resp, ok := parseGHConditionalResponse(out); ok {
		return resp.body
	}
	return out
}

func ghAPIWithArgs(endpoint string, args ...string) ([]byte, error) {
	// #812: while the core REST bucket is known-exhausted, skip the call
	// entirely — issuing it would only fail against an empty budget. rate_limit
	// is exempt: it does not consume the core quota and is exactly the probe an
	// operator/guard uses to confirm the bucket has refilled.
	if endpoint != "rate_limit" {
		if paused, resetAt := primaryRateLimitActive(); paused {
			notePrimarySkip()
			return nil, &PrimaryRateLimitError{Endpoint: endpoint, ResetAt: resetAt}
		}
	}
	conditional := ghConditionalEligible(endpoint, args)
	paginated := false
	for _, a := range args {
		if a == "--paginate" {
			paginated = true
		}
	}
	cacheKey := etagCacheKey(endpoint, paginated)
	cmdArgs := append([]string{"api", "--hostname", "github.com", endpoint}, args...)
	var cachedBody []byte
	if conditional {
		var cachedETag string
		cachedETag, cachedBody = etagLookup(cacheKey)
		cmdArgs = append(cmdArgs, "--include")
		if cachedETag != "" {
			cmdArgs = append(cmdArgs, "-H", "If-None-Match: "+cachedETag)
		}
	}
	var out []byte
	var err error
	anomaly := false
	for attempt := 0; attempt < ghAPIMaxAttempts; attempt++ {
		anomaly = false
		out, err = ghAPIRunner(cmdArgs...)
		if conditional {
			body, served, resolved := resolveConditional(cacheKey, endpoint, out, err, cachedBody, paginated)
			if resolved {
				noteAPIRequest(served, false)
				return body, nil
			}
		} else if err == nil {
			noteAPIRequest(false, false)
			return out, nil
		}
		if err == nil {
			// A conditional exchange in a shape we refuse to trust: drop the
			// cache entry and refetch plain so behavior matches the
			// unconditional path.
			anomaly = true
			noteAPIRequest(false, false)
			etagDrop(cacheKey)
			conditional = false
			cachedBody = nil
			cmdArgs = append([]string{"api", endpoint}, args...)
			continue
		}
		rl := classifyGHRateLimit(out)
		rateLimited := rl.kind != ghRateLimitNone
		noteAPIRequest(false, rateLimited)
		if rl.kind == ghRateLimitPrimary {
			// #812: a primary (hourly) exhaustion does not refill for up to an
			// hour, so retrying is guaranteed to fail and burns more of an
			// already-empty budget. Fail fast after this one request and arm the
			// shared gate so sibling flows skip their REST cycle until reset —
			// replacing the old 4×-per-call amplification across all flows.
			notePrimaryRateLimit(rl.resetAt, rl.remaining)
			resetLabel := "a short fallback window"
			if paused, resetAt := primaryRateLimitActive(); paused {
				resetLabel = resetAt.UTC().Format(time.RFC3339)
			}
			log.Printf("[github] gh api %s: PRIMARY rate limit exhausted (%s); failing fast without retry, pausing REST polling until %s",
				endpoint, ghErrorDetail(ghConditionalErrorDetail(out)), resetLabel)
			break
		}
		if rl.kind != ghRateLimitSecondary || attempt == ghAPIMaxAttempts-1 {
			break
		}
		// #797: once shutdown has begun a rate-limited read fails fast — the
		// backoff loop was observed stretching a 0-worker drain past 20
		// minutes. The post-sleep check catches a shutdown that began while
		// the (interruptible) backoff was already sleeping.
		if ShuttingDown() {
			log.Printf("[github] gh api %s: rate-limited during shutdown (%s); failing fast without retry",
				endpoint, ghErrorDetail(ghConditionalErrorDetail(out)))
			break
		}
		wait := rl.retryAfter
		if wait <= 0 {
			wait = ghBackoffDelay(attempt)
		}
		log.Printf("[github] gh api %s: secondary rate-limited (%s); backing off %s before retry %d/%d",
			endpoint, ghErrorDetail(ghConditionalErrorDetail(out)), wait.Round(time.Millisecond), attempt+1, ghAPIMaxAttempts-1)
		ghAPISleep(wait)
		if ShuttingDown() {
			break
		}
	}
	if err != nil {
		if detail := ghErrorDetail(ghConditionalErrorDetail(out)); detail != "" {
			return nil, fmt.Errorf("gh api %s: %w: %s", endpoint, err, detail)
		}
		return nil, fmt.Errorf("gh api %s: %w", endpoint, err)
	}
	if anomaly {
		return nil, fmt.Errorf("gh api %s: inconsistent conditional response", endpoint)
	}
	return out, nil
}

// ghAPIWithArgsContext is the bounded freshness-read sibling of the general
// polling wrapper. Delivery calls it while holding an execution lease, so the
// subprocess and secondary-rate-limit backoff must honor cancellation; a hung
// gh process must never strand a row in executing. Errors deliberately omit
// provider output because this path is adjacent to deployment authorization.
func ghAPIWithArgsContext(ctx context.Context, endpoint string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmdArgs := append([]string{"api", endpoint}, args...)
	var lastErr error
	for attempt := 0; attempt < ghAPIMaxAttempts; attempt++ {
		cmd := ghFreshnessCommandContext(ctx, cmdArgs...)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := cmd.Output()
		if err == nil {
			noteAPIRequest(false, false)
			return out, nil
		}
		lastErr = err
		failure := out
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			failure = append(failure, exitErr.Stderr...)
		}
		rl := classifyGHRateLimit(failure)
		noteAPIRequest(false, rl.kind != ghRateLimitNone)
		if ctx.Err() != nil || rl.kind != ghRateLimitSecondary || attempt == ghAPIMaxAttempts-1 {
			break
		}
		wait := rl.retryAfter
		if wait <= 0 {
			wait = ghBackoffDelay(attempt)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if lastErr == nil {
		lastErr = errors.New("github freshness request failed")
	}
	return nil, fmt.Errorf("github freshness request failed: %w", lastErr)
}

func ghFreshnessCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := trustedGHCommandContext(ctx, args...)
	// Preserve the existing fallback semantics for a non-context auth failure:
	// the sanitized ambient PAT remains usable. A canceled/deadline context is
	// observed by ghAPIWithArgsContext before the process is started.
	_ = ghApplyAuthContext(ctx, cmd)
	return cmd
}

// trustedGHCommandContext is the process fence shared by delivery freshness
// reads and their GitHub App installation-token exchange. It never resolves gh
// through ambient PATH, carries only the narrow GitHub/network environment,
// and kills the entire subprocess group on cancellation so a child retaining a
// pipe cannot strand an executing delivery lease.
func trustedGHCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	path := "/nonexistent/maestro-trusted-gh"
	if trusted, ok := trustedGHExecutableWithinRoots("gh", []string{"/usr/bin", "/bin", "/usr/local/bin"}); ok {
		path = trusted
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = githubFreshnessEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// trustedGHExecutableWithinRoots accepts an executable only when its canonical
// allowlisted root, every parent below that root, and the executable itself are
// root-owned and not group/world writable. A fixed absolute path is not a
// sufficient process fence when an unprivileged user can replace a component.
func trustedGHExecutableWithinRoots(name string, allowedRoots []string) (string, bool) {
	if name == "" || strings.ContainsRune(name, filepath.Separator) {
		return "", false
	}
	for _, root := range allowedRoots {
		canonicalRoot, err := filepath.EvalSymlinks(root)
		if err != nil || !filepath.IsAbs(canonicalRoot) || !trustedGHRootOwnedPath(canonicalRoot, canonicalRoot) {
			continue
		}
		candidate := filepath.Join(canonicalRoot, name)
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || !filepath.IsAbs(resolved) {
			continue
		}
		rel, relErr := filepath.Rel(canonicalRoot, resolved)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 && trustedGHRootOwnedPath(resolved, canonicalRoot) {
			return resolved, true
		}
	}
	return "", false
}

func trustedGHRootOwnedPath(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil || info.Mode().Perm()&0o022 != 0 {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return false
		}
		if current == root {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func githubFreshnessEnv(env []string) []string {
	out := make([]string, 0, 16)
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		allowed := strings.HasPrefix(key, "LC_")
		if !allowed {
			switch key {
			case "HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "GH_CONFIG_DIR",
				"GH_TOKEN", "GITHUB_TOKEN",
				"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
				"http_proxy", "https_proxy", "all_proxy", "no_proxy",
				"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE",
				"LANG", "LANGUAGE", "TMPDIR":
				allowed = true
			}
		}
		if allowed {
			out = append(out, entry)
		}
	}
	return append(out, "PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/usr/local/sbin")
}

// ---------------------------------------------------------------------------
// REST usage accounting (#797). The acceptance criterion asks for measured
// calls/hour before/after via a counter in the gh wrapper: every exchange is
// counted here and a one-line digest lands in the journal every hour, so an
// operator can read consumption straight from journalctl and cross-check it
// against `gh api rate_limit`.
// ---------------------------------------------------------------------------

// APIStats counts gh REST exchanges. Requests-NotModified approximates what
// was billed against the core quota (a 304 is free). A --paginate call counts
// once even when gh fetched several pages, so the digest is a lower bound.
type APIStats struct {
	Requests    int64
	NotModified int64
	RateLimited int64
}

// apiUsageLogEvery is how often the wrapper writes the one-line usage digest.
const apiUsageLogEvery = time.Hour

var (
	apiStatsMu     sync.Mutex
	apiStatsTotal  APIStats
	apiStatsWindow APIStats
	apiWindowStart time.Time
	apiUsageNow    = time.Now // injectable for tests
)

func noteAPIRequest(notModified, rateLimited bool) {
	apiStatsMu.Lock()
	defer apiStatsMu.Unlock()
	now := apiUsageNow()
	if apiWindowStart.IsZero() {
		apiWindowStart = now
	} else if elapsed := now.Sub(apiWindowStart); elapsed >= apiUsageLogEvery {
		// Roll the window before counting so the request that crossed the
		// boundary opens the new window instead of vanishing with the digest.
		w := apiStatsWindow
		log.Printf("[github] REST usage last %s: %d requests, ~%d billed against core quota, %d served free by 304, %d rate-limited%s%s%s",
			elapsed.Round(time.Minute), w.Requests, w.Requests-w.NotModified, w.NotModified, w.RateLimited, authModeDigest(), primaryLimitDigest(), mirrorReadDigest())
		apiStatsWindow = APIStats{}
		apiWindowStart = now
	}
	for _, s := range []*APIStats{&apiStatsTotal, &apiStatsWindow} {
		s.Requests++
		if notModified {
			s.NotModified++
		}
		if rateLimited {
			s.RateLimited++
		}
	}
}

// APIUsage returns the process-lifetime REST exchange counters, the same
// numbers the hourly journal digest reports per window.
func APIUsage() APIStats {
	apiStatsMu.Lock()
	defer apiStatsMu.Unlock()
	return apiStatsTotal
}

// mirrorReadDigestFn, when set by the daemon, returns a fragment describing how
// many supervisor/orchestrator reads the local mirror served vs how many fell
// back to the API (#826). github cannot import internal/mirrorstore — that would
// be an import cycle, since mirrorstore imports github — so the fragment is
// injected as a hook. Unset appends nothing, so the pre-#826 line is unchanged.
var mirrorReadDigestFn func() string

// SetMirrorReadDigest installs the mirror-usage fragment appended to the hourly
// REST usage journal line. The daemon wires it to the mirrorstore read counters
// so the "API calls/hour" journal line also reports mirror hit/fallback counts
// (#826 AC 5). Passing nil clears it.
func SetMirrorReadDigest(fn func() string) { mirrorReadDigestFn = fn }

func mirrorReadDigest() string {
	if mirrorReadDigestFn == nil {
		return ""
	}
	return mirrorReadDigestFn()
}

// ghRateLimitKind distinguishes the two GitHub rate limits, which need opposite
// handling (#812). The secondary (abuse / burst-concurrency) limit is short and
// clears in seconds, so #794's retry-with-backoff is correct. The primary
// (hourly, 5000/hr) limit does not refill for up to an hour, so every retry is
// guaranteed to fail and only burns more of an already-empty budget — it must
// fail fast and pause polling until reset instead.
type ghRateLimitKind int

const (
	ghRateLimitNone ghRateLimitKind = iota
	ghRateLimitSecondary
	ghRateLimitPrimary
)

// ghRateLimitInfo is the classified verdict for a failed gh exchange.
type ghRateLimitInfo struct {
	kind       ghRateLimitKind
	retryAfter time.Duration // explicit Retry-After hint (secondary); 0 if none
	resetAt    time.Time     // X-RateLimit-Reset (primary); zero if unknown
	remaining  int           // last-seen X-RateLimit-Remaining; -1 if unknown
}

// classifyGHRateLimit inspects gh's combined output (and, on a conditional
// --include exchange, its response headers) and reports whether the failure is
// a primary or secondary rate limit, plus any reset/Retry-After timing.
//
// Signals, in priority order:
//   - X-RateLimit-Remaining: 0 → the core bucket is empty: PRIMARY. Definitive
//     even under a secondary-worded message, because a retry cannot succeed
//     until the reset regardless of what tripped the response.
//   - "secondary rate limit" / "abuse", or an explicit Retry-After (which the
//     primary limit never sends) → SECONDARY: retry with backoff, unchanged.
//   - "api rate limit exceeded" (the primary signature, e.g. "API rate limit
//     exceeded for user ID <n>") → PRIMARY.
//   - any other rate-limit-ish text (bare "rate limit", HTTP 429) with no
//     primary evidence → SECONDARY, preserving pre-#812 retry behavior; only a
//     clear primary signal short-circuits to fail-fast.
func classifyGHRateLimit(out []byte) ghRateLimitInfo {
	text := strings.ToLower(string(out))
	info := ghRateLimitInfo{remaining: -1}

	if m := ghRateLimitRemainingRe.FindSubmatch(out); m != nil {
		if n, convErr := strconv.Atoi(string(m[1])); convErr == nil {
			info.remaining = n
		}
	}
	if m := ghRateLimitResetRe.FindSubmatch(out); m != nil {
		if secs, convErr := strconv.ParseInt(string(m[1]), 10, 64); convErr == nil && secs > 0 {
			info.resetAt = time.Unix(secs, 0)
		}
	}
	if m := ghRetryAfterRe.FindSubmatch(out); m != nil {
		if secs, convErr := strconv.Atoi(string(m[1])); convErr == nil && secs > 0 {
			info.retryAfter = time.Duration(secs) * time.Second
		}
	}

	primaryByHeader := info.remaining == 0
	primaryBySignature := strings.Contains(text, "api rate limit exceeded")
	secondary := strings.Contains(text, "secondary rate limit") || strings.Contains(text, "abuse")
	anyLimit := primaryByHeader || primaryBySignature || secondary || info.retryAfter > 0 ||
		strings.Contains(text, "rate limit") || strings.Contains(text, "http 429")

	switch {
	case !anyLimit:
		info.kind = ghRateLimitNone
	case primaryByHeader:
		info.kind = ghRateLimitPrimary
	case secondary || info.retryAfter > 0:
		info.kind = ghRateLimitSecondary
	case primaryBySignature:
		info.kind = ghRateLimitPrimary
	default:
		info.kind = ghRateLimitSecondary
	}
	return info
}

// parseGHRateLimit reports whether gh's combined output looks like a rate-limit
// (or 429) response and, when gh surfaced an explicit Retry-After hint, how long
// to wait. retryAfter is 0 when no hint is present, in which case the caller
// falls back to exponential backoff. It intentionally does not distinguish
// primary from secondary — callers that need that use classifyGHRateLimit.
func parseGHRateLimit(out []byte) (retryAfter time.Duration, rateLimited bool) {
	info := classifyGHRateLimit(out)
	return info.retryAfter, info.kind != ghRateLimitNone
}

// ---------------------------------------------------------------------------
// Primary (hourly) rate-limit fail-fast + shared pause gate (#812). During the
// 2026-07-03 storm a primary exhaustion was misclassified as retryable, so
// every doomed call fanned out to ghAPIMaxAttempts retries across all in-process
// flows — a 4× amplifier against an already-empty budget. When the wrapper now
// recognises a primary exhaustion it arms this process-wide gate with the
// X-RateLimit-Reset instant; every subsequent core-REST call from any flow
// short-circuits until the reset instead of issuing a call that cannot succeed.
// This is the REST analogue of the orchestrator's GraphQL budget guard
// (projectGraphQLBudgetAvailable).
// ---------------------------------------------------------------------------

// primaryLimitFallbackPause is how long the gate stays armed when a primary
// exhaustion carried no parseable X-RateLimit-Reset (a non-conditional call has
// no headers to read). Short by design: one doomed call per flow per window is a
// negligible drip next to the 4×-per-call burst this replaces, and it bounds an
// over-long stall if the true reset was never surfaced.
const primaryLimitFallbackPause = 60 * time.Second

var (
	primaryLimitMu      sync.Mutex
	primaryLimitReset   time.Time  // gate expiry; zero = never armed
	primaryLimitSince   time.Time  // start of the current pause window
	primaryLimitRemain  = -1       // last-seen X-RateLimit-Remaining (-1 unknown)
	primaryLimitSkipped int64      // core-REST calls short-circuited by the gate
	primaryLimitHits    int64      // primary exhaustions observed
	primaryLimitNow     = time.Now // injectable for tests
)

// PrimaryLimitState is a diagnostic snapshot of the shared primary-rate-limit
// pause gate (#812): whether REST polling is paused, until when, the pause's
// start, the last-seen remaining budget, how many calls were skipped, and how
// many primary exhaustions were observed.
type PrimaryLimitState struct {
	Paused    bool
	ResetAt   time.Time
	Since     time.Time
	Remaining int
	Skipped   int64
	Hits      int64
}

// notePrimaryRateLimit arms (or refreshes) the gate after the wrapper observed a
// primary exhaustion. resetAt is X-RateLimit-Reset when gh surfaced it; a
// zero/past reset falls back to a short fixed pause. A refresh only ever extends
// the window (keeps the furthest reset) so a stale header cannot shorten it.
func notePrimaryRateLimit(resetAt time.Time, remaining int) {
	primaryLimitMu.Lock()
	defer primaryLimitMu.Unlock()
	now := primaryLimitNow()
	if resetAt.IsZero() || !resetAt.After(now) {
		resetAt = now.Add(primaryLimitFallbackPause)
	}
	primaryLimitHits++
	if remaining >= 0 {
		primaryLimitRemain = remaining
	}
	alreadyPaused := primaryLimitReset.After(now)
	if resetAt.After(primaryLimitReset) {
		primaryLimitReset = resetAt
	}
	if !alreadyPaused {
		primaryLimitSince = now
	}
}

// notePrimarySkip records that a core-REST call was short-circuited by the gate.
func notePrimarySkip() {
	primaryLimitMu.Lock()
	primaryLimitSkipped++
	primaryLimitMu.Unlock()
}

// primaryRateLimitActive reports whether the gate is currently armed and, if so,
// the reset instant to wait for.
func primaryRateLimitActive() (paused bool, resetAt time.Time) {
	primaryLimitMu.Lock()
	defer primaryLimitMu.Unlock()
	if !primaryLimitReset.IsZero() && primaryLimitNow().Before(primaryLimitReset) {
		return true, primaryLimitReset
	}
	return false, time.Time{}
}

// PrimaryRateLimitPaused reports whether the shared core-REST bucket is
// currently known-exhausted and, if so, the reset instant callers should wait
// for before polling again (#812). Flows consult it to skip their REST poll
// cycle instead of issuing doomed calls.
func PrimaryRateLimitPaused() (paused bool, resetAt time.Time) {
	return primaryRateLimitActive()
}

// PrimaryRateLimitState returns a diagnostic snapshot of the primary-limit pause
// gate for status/diagnostic surfaces (#812).
func PrimaryRateLimitState() PrimaryLimitState {
	primaryLimitMu.Lock()
	defer primaryLimitMu.Unlock()
	paused := !primaryLimitReset.IsZero() && primaryLimitNow().Before(primaryLimitReset)
	return PrimaryLimitState{
		Paused:    paused,
		ResetAt:   primaryLimitReset,
		Since:     primaryLimitSince,
		Remaining: primaryLimitRemain,
		Skipped:   primaryLimitSkipped,
		Hits:      primaryLimitHits,
	}
}

// primaryLimitDigest renders the primary-limit pause state for the hourly REST
// usage digest so an operator reading journalctl sees, alongside consumption,
// whether a primary exhaustion paused polling — last-seen remaining, reset
// time, and how many calls were skipped (#812). Empty when nothing to report.
func primaryLimitDigest() string {
	st := PrimaryRateLimitState()
	if st.Hits == 0 && !st.Paused {
		return ""
	}
	status := "clear"
	if st.Paused {
		status = "PAUSED until " + st.ResetAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("; primary-limit %s (hits=%d, calls skipped=%d, last remaining=%d)",
		status, st.Hits, st.Skipped, st.Remaining)
}

// resetPrimaryLimitForTest clears the shared gate so a test that arms it does
// not leak state into the rest of the (serially-run) package tests.
func resetPrimaryLimitForTest() {
	primaryLimitMu.Lock()
	defer primaryLimitMu.Unlock()
	primaryLimitReset = time.Time{}
	primaryLimitSince = time.Time{}
	primaryLimitRemain = -1
	primaryLimitSkipped = 0
	primaryLimitHits = 0
}

// PrimaryRateLimitError is returned when a core-REST call is skipped because the
// shared primary-limit gate is armed (#812): the call was never issued because
// it could not succeed until ResetAt.
type PrimaryRateLimitError struct {
	Endpoint string
	ResetAt  time.Time
}

func (e *PrimaryRateLimitError) Error() string {
	if e == nil {
		return "primary GitHub rate limit exhausted"
	}
	if e.ResetAt.IsZero() {
		return fmt.Sprintf("gh api %s: skipped — primary GitHub rate limit exhausted", e.Endpoint)
	}
	return fmt.Sprintf("gh api %s: skipped — primary GitHub rate limit exhausted (resets %s)",
		e.Endpoint, e.ResetAt.UTC().Format(time.RFC3339))
}

// ghBackoffDelay computes the exponential backoff for the given zero-based retry
// attempt, capped at ghAPIMaxBackoff, with full jitter over the lower half so
// concurrent flows do not retry in lockstep.
func ghBackoffDelay(attempt int) time.Duration {
	d := ghAPIBaseBackoff << attempt // base * 2^attempt
	if d <= 0 || d > ghAPIMaxBackoff {
		d = ghAPIMaxBackoff
	}
	return d/2 + time.Duration(ghAPIJitterFrac()*float64(d/2))
}

// ghErrorDetail renders gh's combined output as a single-line, length-bounded
// diagnostic so the actual GitHub error reaches the logs without flooding them.
func ghErrorDetail(out []byte) string {
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return ""
	}
	detail = strings.Join(strings.Fields(detail), " ")
	const max = 400
	if len(detail) > max {
		detail = detail[:max] + "…"
	}
	return detail
}

func parseRateLimitStatus(out []byte) (RateLimitStatus, error) {
	var payload struct {
		Resources struct {
			Core    RateLimitBucket `json:"core"`
			GraphQL RateLimitBucket `json:"graphql"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return RateLimitStatus{}, err
	}
	return RateLimitStatus{
		Core:    payload.Resources.Core,
		GraphQL: payload.Resources.GraphQL,
	}, nil
}

func (c *Client) RateLimit() (RateLimitStatus, error) {
	out, err := ghAPI("rate_limit")
	if err != nil {
		return RateLimitStatus{}, err
	}
	status, err := parseRateLimitStatus(out)
	if err != nil {
		return RateLimitStatus{}, fmt.Errorf("parse rate limit: %w", err)
	}
	return status, nil
}

// mergePaginatedJSONArrays flattens the body of a `gh api --paginate` call over
// an ARRAY endpoint into a single JSON array. gh does not merge array pages the
// way it merges object endpoints that carry total_count (check-runs, search):
// for a plain array endpoint (repos/*/issues, repos/*/pulls) it emits each page
// as its own back-to-back `[...]` document, so a repo with more than one page of
// results yields `[...][...]`, which a single json.Unmarshal rejects with a
// syntax error at the second `[`. The `gh api` manual notes `--slurp` as the
// wrapper for this, but --slurp would take the call out of the conditional
// (ETag) path — ghConditionalEligible only allows --paginate — and cost the
// reconciler its 304s, so instead we decode the concatenated documents as a
// stream here and concatenate their elements. A single-page (or 304-served)
// body is one `[...]` document and passes through as that same one array.
//
// #827 review: the reconciliation loop treats an issue/PR absent from this list
// as a missed close, so a parse failure on the first multi-page reconcile would
// strand exactly the >100-item repos this loop exists to heal.
func mergePaginatedJSONArrays(out []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	merged := []json.RawMessage{}
	for {
		var page []json.RawMessage
		if err := dec.Decode(&page); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		merged = append(merged, page...)
	}
	return json.Marshal(merged)
}

func parseRESTIssues(out []byte) ([]Issue, error) {
	var restIssues []restIssue
	if err := json.Unmarshal(out, &restIssues); err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(restIssues))
	for _, issue := range restIssues {
		if issue.PullRequest != nil {
			continue
		}
		issues = append(issues, issue.issue())
	}
	return issues, nil
}

func parseRESTIssue(out []byte) (Issue, error) {
	var issue restIssue
	if err := json.Unmarshal(out, &issue); err != nil {
		return Issue{}, err
	}
	return issue.issue(), nil
}

func restIssueStateClosed(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "closed")
}

func parseRESTPulls(out []byte) ([]PR, error) {
	var restPulls []restPull
	if err := json.Unmarshal(out, &restPulls); err != nil {
		return nil, err
	}
	prs := make([]PR, 0, len(restPulls))
	for _, pr := range restPulls {
		prs = append(prs, pr.pr())
	}
	return prs, nil
}

func issueRefRegexp(issueNumber int) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`(?i)(^|[^0-9])#%d([^0-9]|$)`, issueNumber))
}

func prReferencesIssue(pr PR, issueNumber int) bool {
	if issueNumber <= 0 {
		return false
	}
	// Match the title verbatim, but strip Markdown code (fenced blocks and
	// inline spans) from the body first. PR bodies routinely paste log lines,
	// command output, and tracebacks that mention OTHER issue numbers
	// (e.g. "[orch] starting worker for issue #353"); those incidental
	// mentions must not link the PR to that issue. See #468.
	return issueRefRegexp(issueNumber).MatchString(pr.Title + "\n" + stripCodeForRefMatch(pr.Body))
}

// PRReferencesIssue reports whether the PR title/body references issueNumber.
// It exposes the same code/log-safe matcher used by HasOpenPRForIssue so
// control-plane reconcilers can match an already-fetched open-PR snapshot
// without issuing another GitHub request.
func PRReferencesIssue(pr PR, issueNumber int) bool {
	return prReferencesIssue(pr, issueNumber)
}

// PRClosesIssue reports whether a PR uses a GitHub closing keyword for the
// issue. It is exported so an orchestrator can evaluate one shared closed-PR
// snapshot for many terminal sessions without re-fetching that snapshot.
func PRClosesIssue(pr PR, issueNumber int) bool {
	return prClosesIssue(pr, issueNumber)
}

// prClosesIssue is the STRICT variant for "this merged PR closed issue N".
// Unlike prReferencesIssue, it requires one of GitHub's recognised closing
// keywords (close/closes/closed, fix/fixes/fixed, resolve/resolves/resolved)
// directly in front of `#N`. A bare mention of `#N` somewhere in the title
// or body — pasted from a context-style commit message such as
// "P0 #487: add HTTP auth" — does NOT count.
//
// This matches GitHub's own "Linked pull requests" semantics. We can't ask
// GitHub the question via REST (the GraphQL `closedByPullRequestsReferences`
// connection is the canonical source but we don't have a typed wrapper for
// it yet); the keyword scan is a faithful local approximation, identical
// to what GitHub's web UI uses to populate the "Linked issues" panel.
//
// Background: #520. Caller HasMergedPRForIssue used prReferencesIssue
// before this helper existed and false-positively linked four merged PRs
// to issue #487 — none of which actually closed it.
func prClosesIssue(pr PR, issueNumber int) bool {
	if issueNumber <= 0 {
		return false
	}
	corpus := pr.Title + "\n" + stripCodeForRefMatch(pr.Body)
	return closingKeywordRegexp(issueNumber).MatchString(corpus)
}

// closingKeywordRegexp returns a compiled regex that matches any of the
// recognised GitHub closing keywords directly preceding `#N`, with optional
// whitespace and an optional `:` between the keyword and the hash.
//
// Examples that match (issueNumber = 487):
//
//	"Closes #487"        "fixes: #487"        "Resolved #487."
//	"...closes #487\nThis PR ..."             "RESOLVES #487"
//
// Examples that do NOT match:
//
//	"P0 #487: add HTTP auth ..."     (bare mention)
//	"Refs #487"                      (Refs is not a closing keyword)
//	"see #487 for context"
//	"ticket-487"                     (no `#`, also no closing keyword)
func closingKeywordRegexp(issueNumber int) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(
		`(?i)(?:^|[^a-z])(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s*#%d(?:[^0-9]|$)`,
		issueNumber,
	))
}

var (
	// A fence line is 3+ backticks or 3+ tildes, optionally indented, with an
	// optional info string (only valid on the OPENING fence).
	fenceLineRegexp = regexp.MustCompile("^\\s*(`{3,}|~{3,})\\s*(\\S.*)?$")
	// Inline code spans: one or more backticks, content without a backtick or
	// newline, then the same-or-more backticks. Approximate but sufficient for
	// stripping `#123`-style mentions out of prose.
	inlineCodeRegexp = regexp.MustCompile("`+[^`\\n]*`+")
)

// stripCodeForRefMatch removes fenced code blocks and inline code spans from
// Markdown text so issue references buried in pasted logs/output do not produce
// false positives in prReferencesIssue. Prose references such as "Refs #123"
// or "Closes #123" (the Maestro worker convention) are preserved.
func stripCodeForRefMatch(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	var fenceChar byte
	var fenceLen int
	for _, line := range lines {
		if m := fenceLineRegexp.FindStringSubmatch(line); m != nil {
			marker := m[1]
			info := strings.TrimSpace(m[2])
			ch := marker[0]
			n := len(marker)
			if !inFence {
				// Opening fence — drop it and start skipping content.
				inFence = true
				fenceChar = ch
				fenceLen = n
				continue
			}
			// Inside a fence: a valid closing fence uses the same character,
			// is at least as long as the opener, and carries no info string.
			if ch == fenceChar && n >= fenceLen && info == "" {
				inFence = false
				continue
			}
			// A fence-looking line that is not a valid closer is fence content.
			continue
		}
		if inFence {
			continue
		}
		out = append(out, line)
	}
	cleaned := strings.Join(out, "\n")
	return inlineCodeRegexp.ReplaceAllString(cleaned, " ")
}

func parseCheckRuns(out []byte) ([]greptileCheckRun, error) {
	var payload checkRunsResponse
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, err
	}
	return latestLogicalCheckRuns(payload.CheckRuns), nil
}

// latestLogicalCheckRuns collapses historical rerun attempts into GitHub's
// current logical check contexts. The checks API returns every attempt for a
// commit, so treating all rows as simultaneously authoritative lets an old
// failure override a newer successful rerun on the same head (#998). GitHub
// check contexts are identified by app + name; creation/start time and the
// public monotonic check-run ID select the newest attempt deterministically.
func latestLogicalCheckRuns(checks []greptileCheckRun) []greptileCheckRun {
	if len(checks) < 2 {
		return checks
	}
	latest := make(map[string]greptileCheckRun, len(checks))
	order := make([]string, 0, len(checks))
	for _, check := range checks {
		key := logicalCheckRunKey(check)
		current, ok := latest[key]
		if !ok {
			latest[key] = check
			order = append(order, key)
			continue
		}
		if checkRunNewer(check, current) {
			latest[key] = check
		}
	}
	result := make([]greptileCheckRun, 0, len(order))
	for _, key := range order {
		result = append(result, latest[key])
	}
	return result
}

func logicalCheckRunKey(check greptileCheckRun) string {
	name := strings.ToLower(strings.TrimSpace(check.Name))
	if name == "" {
		return fmt.Sprintf("id:%d", check.ID)
	}
	app := fmt.Sprintf("id:%d", check.App.ID)
	if check.App.ID == 0 {
		app = "slug:" + strings.ToLower(strings.TrimSpace(check.App.Slug))
	}
	return app + "\x00" + name
}

func checkRunNewer(candidate, current greptileCheckRun) bool {
	candidateAt := checkRunCreatedAt(candidate)
	currentAt := checkRunCreatedAt(current)
	if !candidateAt.Equal(currentAt) {
		return candidateAt.After(currentAt)
	}
	return candidate.ID > current.ID
}

func checkRunCreatedAt(check greptileCheckRun) time.Time {
	for _, raw := range []string{check.StartedAt, check.CreatedAt, check.CompletedAt} {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func parseCombinedStatus(out []byte) (combinedStatusResponse, error) {
	var payload combinedStatusResponse
	if err := json.Unmarshal(out, &payload); err != nil {
		return combinedStatusResponse{}, err
	}
	return payload, nil
}

func ciStatusFromREST(checks []greptileCheckRun, combined combinedStatusResponse) string {
	checks = latestLogicalCheckRuns(checks)
	// GitHub's combined commit-status API returns state:"pending" with
	// statuses:[] for any commit that has zero legacy commit statuses — the
	// normal case for repos that report CI exclusively via check-runs.
	// An empty combined status carries no signal and must not override the
	// check-runs verdict; only honor pending/failure when statuses exist.
	if len(combined.Statuses) > 0 {
		if strings.EqualFold(combined.State, "pending") {
			return "pending"
		}
		if strings.EqualFold(combined.State, "failure") || strings.EqualFold(combined.State, "error") {
			return "failure"
		}
	}

	hasSignal := len(checks) > 0 || len(combined.Statuses) > 0
	for _, check := range checks {
		status := strings.ToLower(strings.TrimSpace(check.Status))
		conclusion := strings.ToLower(strings.TrimSpace(check.Conclusion))
		if status == "queued" || status == "in_progress" || status == "waiting" || status == "requested" || (status != "completed" && conclusion == "") {
			return "pending"
		}
		switch conclusion {
		case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
			return "failure"
		}
	}
	if !hasSignal {
		return "success"
	}
	return "success"
}

func formatChecksOverview(checks []greptileCheckRun, combined combinedStatusResponse) string {
	checks = latestLogicalCheckRuns(checks)
	var lines []string
	for _, check := range checks {
		state := strings.TrimSpace(check.Conclusion)
		if state == "" {
			state = strings.TrimSpace(check.Status)
		}
		if state == "" {
			state = "unknown"
		}
		lines = append(lines, fmt.Sprintf("%s\t%s", check.Name, state))
	}
	for _, status := range combined.Statuses {
		name := status.Context
		if name == "" {
			name = "commit-status"
		}
		state := status.State
		if state == "" {
			state = "unknown"
		}
		if status.Description != "" {
			lines = append(lines, fmt.Sprintf("%s\t%s\t%s", name, state, status.Description))
		} else {
			lines = append(lines, fmt.Sprintf("%s\t%s", name, state))
		}
	}
	if len(lines) == 0 {
		return "no checks\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func mergeableFromRESTPull(pr restPull) string {
	if pr.Mergeable != nil {
		if *pr.Mergeable {
			return "MERGEABLE"
		}
		return "CONFLICTING"
	}
	switch strings.ToLower(strings.TrimSpace(pr.MergeableState)) {
	case "dirty":
		return "CONFLICTING"
	case "", "unknown":
		return "UNKNOWN"
	default:
		return "MERGEABLE"
	}
}

func parseIssueComments(out []byte) ([]issueComment, error) {
	var comments []issueComment
	if err := json.Unmarshal(out, &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

func parsePRLabels(out []byte) ([]string, error) {
	var labels []prLabel
	if err := json.Unmarshal(out, &labels); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	return names, nil
}

func parsePRCommits(out []byte) ([]string, error) {
	var commits []prCommit
	if err := json.Unmarshal(out, &commits); err != nil {
		return nil, err
	}
	msgs := make([]string, 0, len(commits))
	for _, commit := range commits {
		message := strings.TrimSpace(commit.Commit.Message)
		if message == "" {
			continue
		}
		headline := strings.SplitN(message, "\n", 2)[0]
		msgs = append(msgs, headline)
	}
	return msgs, nil
}

// ListOpenIssues returns open issues matching any of the given labels (OR filter).
// If labels is empty, all open issues are returned.
func (c *Client) ListOpenIssues(labels []string) ([]Issue, error) {
	if len(labels) <= 1 {
		// Single label or no labels — one call suffices
		label := ""
		if len(labels) == 1 {
			label = labels[0]
		}
		return c.listOpenIssuesByLabel(label)
	}

	// Multiple labels: fetch per-label and deduplicate (OR semantics)
	seen := make(map[int]struct{})
	var result []Issue
	for _, label := range labels {
		issues, err := c.listOpenIssuesByLabel(label)
		if err != nil {
			return nil, err
		}
		for _, issue := range issues {
			if _, ok := seen[issue.Number]; !ok {
				seen[issue.Number] = struct{}{}
				result = append(result, issue)
			}
		}
	}
	return result, nil
}

func (c *Client) listOpenIssuesByLabel(label string) ([]Issue, error) {
	endpoint := fmt.Sprintf("repos/%s/issues?state=open&per_page=100", c.Repo)
	if label != "" {
		endpoint += "&labels=" + url.QueryEscape(label)
	}

	out, err := ghAPI(endpoint)
	if err != nil {
		return nil, fmt.Errorf("list open issues: %w", err)
	}

	issues, err := parseRESTIssues(out)
	if err != nil {
		return nil, fmt.Errorf("parse issues: %w", err)
	}
	return issues, nil
}

// ListAllOpenIssues is ListOpenIssues that follows pagination: it fetches EVERY
// page of the repo's open issues, not just the first 100. The reconciliation
// loop (#827) needs the AUTHORITATIVE open set — it treats a mirrored-open issue
// absent from this list as a missed close and stamps it closed, so a truncated
// first page would wrongly close every still-open issue past #100. Ordinary
// callers that only need a working set can stay on the cheaper single-page
// ListOpenIssues. The read still flows through the conditional (ETag) layer, so
// an unchanged repo whose open set fits one partial page answers 304 for free.
func (c *Client) ListAllOpenIssues(labels []string) ([]Issue, error) {
	if len(labels) <= 1 {
		label := ""
		if len(labels) == 1 {
			label = labels[0]
		}
		return c.listAllOpenIssuesByLabel(label)
	}

	seen := make(map[int]struct{})
	var result []Issue
	for _, label := range labels {
		issues, err := c.listAllOpenIssuesByLabel(label)
		if err != nil {
			return nil, err
		}
		for _, issue := range issues {
			if _, ok := seen[issue.Number]; !ok {
				seen[issue.Number] = struct{}{}
				result = append(result, issue)
			}
		}
	}
	return result, nil
}

func (c *Client) listAllOpenIssuesByLabel(label string) ([]Issue, error) {
	endpoint := fmt.Sprintf("repos/%s/issues?state=open&per_page=100", c.Repo)
	if label != "" {
		endpoint += "&labels=" + url.QueryEscape(label)
	}

	out, err := ghAPIWithArgs(endpoint, "--paginate")
	if err != nil {
		return nil, fmt.Errorf("list all open issues: %w", err)
	}

	merged, err := mergePaginatedJSONArrays(out)
	if err != nil {
		return nil, fmt.Errorf("merge paginated issues: %w", err)
	}
	issues, err := parseRESTIssues(merged)
	if err != nil {
		return nil, fmt.Errorf("parse issues: %w", err)
	}
	return issues, nil
}

// GetIssue fetches a single issue by number
func (c *Client) GetIssue(number int) (Issue, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/issues/%d", c.Repo, number))
	if err != nil {
		return Issue{}, fmt.Errorf("get issue %d: %w", number, err)
	}
	issue, err := parseRESTIssue(out)
	if err != nil {
		return Issue{}, fmt.Errorf("parse issue %d: %w", number, err)
	}
	return issue, nil
}

// IssueBody returns just the current body of an issue. The approver executor
// uses it to re-read the live body before applying a groomed edit_issue_body
// rewrite, so an edit made after the proposal was minted is not clobbered
// (#851 review).
func (c *Client) IssueBody(number int) (string, error) {
	issue, err := c.GetIssue(number)
	if err != nil {
		return "", err
	}
	return issue.Body, nil
}

// IsIssueClosed returns true if the issue is closed
func (c *Client) IsIssueClosed(number int) (bool, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/issues/%d", c.Repo, number))
	if err != nil {
		return false, fmt.Errorf("get issue %d: %w", number, err)
	}
	var result struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return false, err
	}
	return restIssueStateClosed(result.State), nil
}

// ListOpenPRs returns all open PRs
func (c *Client) ListOpenPRs() ([]PR, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/pulls?state=open&per_page=100", c.Repo))
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}

	prs, err := parseRESTPulls(out)
	if err != nil {
		return nil, fmt.Errorf("parse prs: %w", err)
	}
	return prs, nil
}

// ListAllOpenPRs is ListOpenPRs that follows pagination — see ListAllOpenIssues
// for why the reconciliation loop (#827) needs the full open set rather than
// page one before it uses absence as a close signal.
func (c *Client) ListAllOpenPRs() ([]PR, error) {
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/pulls?state=open&per_page=100", c.Repo), "--paginate")
	if err != nil {
		return nil, fmt.Errorf("list all open PRs: %w", err)
	}

	merged, err := mergePaginatedJSONArrays(out)
	if err != nil {
		return nil, fmt.Errorf("merge paginated prs: %w", err)
	}
	prs, err := parseRESTPulls(merged)
	if err != nil {
		return nil, fmt.Errorf("parse prs: %w", err)
	}
	return prs, nil
}

func (c *Client) listClosedPRs() ([]PR, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/pulls?state=closed&per_page=100&sort=updated&direction=desc", c.Repo))
	if err != nil {
		return nil, fmt.Errorf("list closed PRs: %w", err)
	}
	prs, err := parseRESTPulls(out)
	if err != nil {
		return nil, fmt.Errorf("parse closed prs: %w", err)
	}
	return prs, nil
}

// ListClosedPRs returns the newest closed PR page. Orchestrator cycles use this
// as one shared snapshot for terminal reconciliation instead of spawning one
// `gh api ... pulls?state=closed` process per historical session.
func (c *Client) ListClosedPRs() ([]PR, error) {
	return c.listClosedPRs()
}

func (c *Client) getRESTPull(prNumber int) (restPull, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/pulls/%d", c.Repo, prNumber))
	if err != nil {
		return restPull{}, err
	}
	var pr restPull
	if err := json.Unmarshal(out, &pr); err != nil {
		return restPull{}, err
	}
	return pr, nil
}

// PRDetails returns the current forge metadata for one PR. Unlike a cached
// open-PR list, this exact read is suitable for an actuation guard whose safety
// depends on mutable metadata such as draft/open state.
func (c *Client) PRDetails(prNumber int) (PR, error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return PR{}, err
	}
	return pr.pr(), nil
}

func (c *Client) pullHeadSHA(prNumber int) (string, error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(pr.Head.SHA)
	if sha == "" {
		return "", fmt.Errorf("empty head sha for PR %d", prNumber)
	}
	return sha, nil
}

// PRHeadSHA returns the current head commit SHA of a PR.
func (c *Client) PRHeadSHA(prNumber int) (string, error) {
	return c.pullHeadSHA(prNumber)
}

// PRMergeInfo is the immutable merge identity/order GitHub records for a PR.
type PRMergeInfo struct {
	SHA      string
	HeadSHA  string
	MergedAt time.Time
}

// PRMergeInfo returns the exact merge commit and GitHub merge timestamp in one
// REST read. MergedAt orders delivery generations when an older code_landed
// session is reconciled after a newer merge.
func (c *Client) PRMergeInfo(prNumber int) (PRMergeInfo, error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return PRMergeInfo{}, err
	}
	if pr.MergedAt == nil {
		return PRMergeInfo{}, fmt.Errorf("PR %d is not merged", prNumber)
	}
	sha := strings.TrimSpace(pr.MergeCommitSHA)
	if len(sha) != 40 {
		return PRMergeInfo{}, fmt.Errorf("PR %d has invalid merge_commit_sha %q", prNumber, sha)
	}
	mergedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(*pr.MergedAt))
	if err != nil {
		return PRMergeInfo{}, fmt.Errorf("PR %d has invalid merged_at: %w", prNumber, err)
	}
	return PRMergeInfo{SHA: sha, HeadSHA: strings.TrimSpace(pr.Head.SHA), MergedAt: mergedAt.UTC()}, nil
}

// LatestMergedPRGenerations returns every merged PR tied at the repository's
// latest merged_at second. GitHub exposes merged_at with second precision, so
// choosing one tied PR by number or list order would invent a merge order. The
// delivery executor resolves a tie with canonical-remote commit ancestry and
// otherwise fails closed.
func (c *Client) LatestMergedPRGenerations(ctx context.Context) ([]PRMergeInfo, error) {
	deliveryBase, err := c.RepositoryDefaultBranch(ctx)
	if err != nil {
		return nil, err
	}
	out, err := ghAPIWithArgsContext(ctx, fmt.Sprintf("repos/%s/pulls?state=closed&base=%s&per_page=100&sort=updated&direction=desc", c.Repo, url.QueryEscape(deliveryBase)), "--paginate")
	if err != nil {
		return nil, errors.New("list merged delivery generations failed")
	}
	merged, err := mergePaginatedJSONArrays(out)
	if err != nil {
		return nil, errors.New("decode merged delivery generations failed")
	}
	var pulls []restPull
	if err := json.Unmarshal(merged, &pulls); err != nil {
		return nil, errors.New("parse merged delivery generations failed")
	}
	return latestMergedPRGenerations(pulls, deliveryBase)
}

// RepositoryDefaultBranch resolves the current authoritative delivery base.
// Repositories using trunk/master are supported, and a newer merge into an
// unrelated release/feature branch cannot supersede a default-branch delivery.
func (c *Client) RepositoryDefaultBranch(ctx context.Context) (string, error) {
	out, err := ghAPIWithArgsContext(ctx, fmt.Sprintf("repos/%s", c.Repo))
	if err != nil {
		return "", errors.New("read repository delivery branch failed")
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(out, &repo); err != nil {
		return "", errors.New("parse repository delivery branch failed")
	}
	branch := strings.TrimSpace(repo.DefaultBranch)
	if branch == "" || strings.ContainsAny(branch, "\r\n?&#") {
		return "", errors.New("repository delivery branch is invalid")
	}
	return branch, nil
}

func latestMergedPRGenerations(pulls []restPull, base string) ([]PRMergeInfo, error) {
	var latestAt time.Time
	var latest []PRMergeInfo
	for _, pr := range pulls {
		if strings.TrimSpace(pr.Base.Ref) != base {
			continue
		}
		if pr.MergedAt == nil {
			continue
		}
		mergedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(*pr.MergedAt))
		if err != nil {
			return nil, errors.New("merged delivery generation has invalid timestamp")
		}
		sha := strings.ToLower(strings.TrimSpace(pr.MergeCommitSHA))
		if len(sha) != 40 {
			return nil, errors.New("merged delivery generation has invalid revision")
		}
		info := PRMergeInfo{SHA: sha, MergedAt: mergedAt.UTC()}
		switch {
		case latestAt.IsZero() || info.MergedAt.After(latestAt):
			latestAt = info.MergedAt
			latest = []PRMergeInfo{info}
		case info.MergedAt.Equal(latestAt):
			latest = append(latest, info)
		}
	}
	if len(latest) == 0 {
		return nil, errors.New("repository has no merged delivery generation")
	}
	return latest, nil
}

// PRMergeCommitSHA returns GitHub's immutable merge_commit_sha for a PR that
// is already merged. It deliberately does not infer the revision from main:
// versioning commits and concurrent merges can advance main immediately after
// this PR lands, and a delivery approval must pin exactly this PR's merge.
func (c *Client) PRMergeCommitSHA(prNumber int) (string, error) {
	info, err := c.PRMergeInfo(prNumber)
	return info.SHA, err
}

// BranchHeadSHA returns the current head commit SHA of the given branch. The
// orchestrator uses it to detect when origin/main has advanced past the running
// binary so a PR merged outside the orchestrator's own merge path still
// triggers a self-deploy (#751).
func (c *Client) BranchHeadSHA(branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", fmt.Errorf("empty branch")
	}
	out, err := ghAPI(fmt.Sprintf("repos/%s/commits/%s", c.Repo, url.PathEscape(branch)))
	if err != nil {
		return "", err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(out, &commit); err != nil {
		return "", fmt.Errorf("parse commit for branch %s: %w", branch, err)
	}
	sha := strings.TrimSpace(commit.SHA)
	if sha == "" {
		return "", fmt.Errorf("empty head sha for branch %s", branch)
	}
	return sha, nil
}

func (c *Client) checkRunsForSHA(sha string) ([]greptileCheckRun, error) {
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", c.Repo, sha), "--paginate")
	if err != nil {
		return nil, err
	}
	checks, err := parseCheckRuns(out)
	if err != nil {
		return nil, fmt.Errorf("parse check runs for %s: %w", sha, err)
	}
	return checks, nil
}

func (c *Client) combinedStatusForSHA(sha string) (combinedStatusResponse, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/commits/%s/status", c.Repo, sha))
	if err != nil {
		return combinedStatusResponse{}, err
	}
	status, err := parseCombinedStatus(out)
	if err != nil {
		return combinedStatusResponse{}, fmt.Errorf("parse combined status for %s: %w", sha, err)
	}
	return status, nil
}

// CreatePR opens a pull request and returns its number.
func (c *Client) CreatePR(title, body, base, head string) (int, error) {
	args := []string{
		"pr", "create",
		"--repo", c.Repo,
		"--title", title,
		"--body", body,
		"--base", base,
		"--head", head,
	}
	out, err := ghCommand(args...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("gh pr create: %w\n%s", err, out)
	}

	output := strings.TrimSpace(string(out))
	match := regexp.MustCompile(`/pull/([0-9]+)`).FindStringSubmatch(output)
	if len(match) != 2 {
		return 0, fmt.Errorf("unexpected gh pr create output: %s", output)
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("parse PR number from %q: %w", output, err)
	}
	return n, nil
}

// UpdatePRBody replaces a pull request body.
func (c *Client) UpdatePRBody(prNumber int, body string) error {
	out, err := ghCommand(
		"pr", "edit", strconv.Itoa(prNumber),
		"--repo", c.Repo,
		"--body", body,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr edit %d --body: %w\n%s", prNumber, err, out)
	}
	return nil
}

// IsPRMerged returns true if the PR has been merged.
func (c *Client) IsPRMerged(prNumber int) (bool, error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return false, fmt.Errorf("get pull %d: %w", prNumber, err)
	}
	return strings.EqualFold(pr.State, "closed") && pr.MergedAt != nil, nil
}

// HasMergedPRForIssue returns true if a merged PR EXPLICITLY CLOSED the
// given issue (per GitHub closing-keyword convention: `closes/fixes/
// resolves #N`). #520: a bare `#N` mention in commit body / title does
// NOT count — that is a reference, not a closure. Matches GitHub's own
// "Linked pull requests" semantics.
func (c *Client) HasMergedPRForIssue(issueNumber int) (bool, error) {
	number, err := c.MergedPRNumberForIssue(issueNumber)
	return number > 0, err
}

// MergedPRNumberForIssue returns the newest merged PR that explicitly closed
// the issue, or 0. Delivery recovery needs the concrete PR number so it can
// fetch that PR's immutable merge_commit_sha; a boolean is not sufficient.
func (c *Client) MergedPRNumberForIssue(issueNumber int) (int, error) {
	prs, err := c.listClosedPRs()
	if err != nil {
		return 0, err
	}
	for _, pr := range prs {
		if pr.MergedAt != "" && prClosesIssue(pr, issueNumber) {
			return pr.Number, nil
		}
	}
	return 0, nil
}

// MergedPRNumberForBranch returns the number of a merged PR that used the
// given branch as its head, or 0 if none exists. After a squash-merge the
// branch tip is not an ancestor of main, so ancestry cannot prove the content
// landed — but a merged PR with this head branch can. The reconcile
// pushed-branch path consults this so a branch that outlived its
// squash-merged PR (e.g. an operator merge without branch deletion) settles
// the session as code_landed instead of spawning a duplicate junk PR (#800).
func (c *Client) MergedPRNumberForBranch(branch string) (int, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return 0, nil
	}
	prs, err := c.listClosedPRs()
	if err != nil {
		return 0, err
	}
	for _, pr := range prs {
		// listClosedPRs only returns closed PRs, but require the closed state
		// on the parsed record too (as IsPRMerged does) so the merged verdict
		// never depends on the list source.
		if strings.EqualFold(pr.State, "closed") && pr.MergedAt != "" && pr.HeadRefName == branch {
			return pr.Number, nil
		}
	}
	return 0, nil
}

// PRCheckRollup is a bounded, non-secret identity for the actual current PR
// head and its CI/check rollup. Fingerprint hashes public check-run IDs, names,
// and closed status/conclusion fields only; it never carries descriptions,
// URLs, output, annotations, paths, or credentials (#887/#940).
type PRCheckRollup struct {
	HeadSHA          string
	Verdict          string
	Fingerprint      string
	Complete         bool
	PendingCheckRuns bool
	Signals          []PRCheckSignal
}

// PRCheckSignal is a non-secret check-run or commit-status identity used by the
// control plane to recognize configured gates without reading log prose.
type PRCheckSignal struct {
	Source     string
	Name       string
	Status     string
	Conclusion string
}

// PRCheckRollup returns the current head, normalized aggregate verdict, and a
// stable digest of individual check/status transitions and public check-run
// identities. A manual rerun on the same head therefore advances once instead
// of looking identical to the exhausted run it replaced. When one of GitHub's
// two CI sources is unavailable the legacy aggregate verdict still degrades to
// the source that did answer, but Complete=false and Fingerprint is absent so a
// partial poll cannot fabricate material progress.
func (c *Client) PRCheckRollup(prNumber int) (PRCheckRollup, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return PRCheckRollup{Verdict: "unknown"}, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	checks, checksErr := c.checkRunsForSHA(sha)
	combined, statusErr := c.combinedStatusForSHA(sha)
	if checksErr != nil && statusErr != nil {
		return PRCheckRollup{HeadSHA: sha, Verdict: "unknown"}, fmt.Errorf("get checks for PR %d: check-runs: %v; statuses: %v", prNumber, checksErr, statusErr)
	}
	rollup := PRCheckRollup{
		HeadSHA:          sha,
		Verdict:          ciStatusFromREST(checks, combined),
		Complete:         checksErr == nil && statusErr == nil,
		PendingCheckRuns: hasPendingCheckRuns(checks),
	}
	if rollup.Complete {
		rollup.Fingerprint = ciCheckRollupFingerprint(checks, combined)
		rollup.Signals = ciCheckRollupSignals(checks, combined)
	}
	return rollup, nil
}

func hasPendingCheckRuns(checks []greptileCheckRun) bool {
	for _, check := range checks {
		status := strings.ToLower(strings.TrimSpace(check.Status))
		conclusion := strings.ToLower(strings.TrimSpace(check.Conclusion))
		if status == "queued" || status == "in_progress" || status == "waiting" || status == "requested" ||
			(status != "completed" && conclusion == "") {
			return true
		}
	}
	return false
}

// PRCIStatus returns "success", "failure", "pending", or "unknown".
func (c *Client) PRCIStatus(prNumber int) (string, error) {
	rollup, err := c.PRCheckRollup(prNumber)
	return rollup.Verdict, err
}

func ciCheckRollupFingerprint(checks []greptileCheckRun, combined combinedStatusResponse) string {
	checks = latestLogicalCheckRuns(checks)
	parts := make([]string, 0, len(checks)+len(combined.Statuses)+1)
	parts = append(parts, "combined="+strings.ToLower(strings.TrimSpace(combined.State)))
	for _, check := range checks {
		parts = append(parts, fmt.Sprintf("check:%d:%s:%s:%s",
			check.ID, strings.TrimSpace(check.Name), strings.ToLower(strings.TrimSpace(check.Status)), strings.ToLower(strings.TrimSpace(check.Conclusion))))
	}
	for _, status := range combined.Statuses {
		parts = append(parts, fmt.Sprintf("status:%s:%s",
			strings.TrimSpace(status.Context), strings.ToLower(strings.TrimSpace(status.State))))
	}
	sort.Strings(parts)
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func ciCheckRollupSignals(checks []greptileCheckRun, combined combinedStatusResponse) []PRCheckSignal {
	checks = latestLogicalCheckRuns(checks)
	signals := make([]PRCheckSignal, 0, len(checks)+len(combined.Statuses))
	for _, check := range checks {
		signals = append(signals, PRCheckSignal{
			Source:     "check_run",
			Name:       strings.TrimSpace(check.Name),
			Status:     strings.ToLower(strings.TrimSpace(check.Status)),
			Conclusion: strings.ToLower(strings.TrimSpace(check.Conclusion)),
		})
	}
	for _, status := range combined.Statuses {
		signals = append(signals, PRCheckSignal{
			Source:     "commit_status",
			Name:       strings.TrimSpace(status.Context),
			Status:     strings.ToLower(strings.TrimSpace(status.State)),
			Conclusion: strings.ToLower(strings.TrimSpace(status.State)),
		})
	}
	sort.SliceStable(signals, func(i, j int) bool {
		if signals[i].Source != signals[j].Source {
			return signals[i].Source < signals[j].Source
		}
		return signals[i].Name < signals[j].Name
	})
	return signals
}

// PRMergeable returns the mergeable state: "MERGEABLE", "CONFLICTING", "UNKNOWN"
func (c *Client) PRMergeable(prNumber int) (string, error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return "", fmt.Errorf("get pull %d: %w", prNumber, err)
	}
	return mergeableFromRESTPull(pr), nil
}

// PRMergeStatus returns both the normalized mergeable verdict
// ("MERGEABLE" / "CONFLICTING" / "UNKNOWN") AND the raw GitHub
// mergeable_state ("clean", "behind", "blocked", "dirty", "unstable",
// "unknown", "draft", "has_hooks") for a single PR, fetched from the
// REST single-PR endpoint (reused by #543/#544 for a fresh mergeable
// signal). The executor needs the raw state to distinguish a green PR
// that has merely fallen BEHIND main (recoverable via update-branch)
// from one with a real conflict (#547).
//
// mergeStateStatus is lower-cased and trimmed; "" means GitHub has not
// computed it yet (caller should treat that as "don't know, proceed").
func (c *Client) PRMergeStatus(prNumber int) (mergeable string, mergeStateStatus string, err error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return "", "", fmt.Errorf("get pull %d: %w", prNumber, err)
	}
	return mergeableFromRESTPull(pr), strings.ToLower(strings.TrimSpace(pr.MergeableState)), nil
}

// PRGreptileApproved checks whether Greptile has approved the PR.
//
// Primary path: reads GitHub Check Runs for the PR's head SHA.
//   - Looks for a check whose name contains "greptile" (case-insensitive).
//   - conclusion == "success" or "neutral" is the authoritative Greptile
//     decision and approves the current head. Greptile uses that successful
//     check for its "ok to merge" / 4-of-5-or-better verdict; inline findings
//     remain review detail and must not contradict the completed gate.
//   - check found, other conclusion → approved=false, pending=false
//   - check not found → falls through to comment-based fallback
//
// Fallback path: reads PR comments for legacy Greptile comment-mode setups.
//   - "safe to merge" or confidence 4/5 / 5/5 → approved=true
//   - comment found but not approving → approved=false, pending=false
//   - no greptile signal at all → pending=true
func (c *Client) PRGreptileApproved(prNumber int) (approved bool, pending bool, err error) {
	verdict, err := c.prGreptileReviewStreamVerdict(prNumber)
	if err != nil {
		return false, false, err
	}
	return verdict.Passed, verdict.Pending, nil
}

const (
	reviewVerdictPending        = "pending"
	reviewVerdictPassed         = "passed"
	reviewVerdictOKToMerge      = "ok_to_merge"
	reviewVerdictRepairRequired = "repair_required"
)

type greptileReviewSignal struct {
	Passed   bool
	Pending  bool
	Observed bool
	Score    int
	ScoreMax int
	Verdict  string
}

var (
	greptileConfidenceScorePattern = regexp.MustCompile(`(?i)\bconfidence(\s+score)?\s*[:=-]?\s*([0-9]+)\s*/\s*([0-9]+)\b`)
	greptilePositiveMergePattern   = regexp.MustCompile(`(?i)\b(ok|okay|safe)\s+to\s+merge\b`)
	greptileNegativeMergePattern   = regexp.MustCompile(`(?i)\b(no|not|never|isn't|isnt|wasn't|wasnt|can't|cant|cannot|don't|dont|doesn't|doesnt)\s+([[:alnum:]'-]+\s+){0,4}(ok|okay|safe)\s+to\s+merge\b|\bunsafe\s+to\s+merge\b`)
)

func (c *Client) prGreptileReviewStreamVerdict(prNumber int) (ReviewStreamVerdict, error) {
	// checkLookupFailed marks a degraded read: absence of a signal below then
	// means "unknown", not "the reviewer is silent".
	var checkLookupFailed bool

	// --- 1. Get head SHA of the PR ---
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return ReviewStreamVerdict{}, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}

	// --- 2. Get check runs for the head SHA ---
	checkRuns, err := c.checkRunsForSHA(sha)
	if err != nil {
		// Non-fatal: fall through to comment fallback
		checkLookupFailed = true
		goto commentFallback
	}

	{
		found, signal := greptileCheckReviewSignal(checkRuns)
		if found {
			verdict := reviewStreamVerdictFromGreptileSignal(signal)
			if !verdict.Passed && !verdict.Pending {
				if _, findings, hasFindings, findingsErr := c.PRHighSeverityReviewOnHead(prNumber); findingsErr == nil && hasFindings {
					verdict.Findings = findings
				}
			}
			return verdict, nil
		}
		// No greptile check run found → fall through to comment fallback
	}

commentFallback:
	// --- 3. Fallback: check PR comments (legacy Greptile comment-mode) ---
	commentsOut, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", c.Repo, prNumber), "--paginate")
	if err != nil {
		return ReviewStreamVerdict{}, fmt.Errorf("list issue comments for PR %d: %w", prNumber, err)
	}

	comments, err := parseIssueComments(commentsOut)
	if err != nil {
		return ReviewStreamVerdict{}, fmt.Errorf("parse pr %d comments: %w", prNumber, err)
	}

	foundGreptile, signal, comment := greptileCommentReviewSignal(comments)
	if !foundGreptile {
		// No check run AND no comment: the reviewer never showed up for this
		// head. Observed stays false so callers can distinguish this from a
		// review in progress and apply a missing-review policy instead of
		// waiting forever — unless the check-runs read itself failed, in which
		// case the silence is unproven and LookupFailed says so.
		return ReviewStreamVerdict{Name: "greptile", Pending: true, Verdict: reviewVerdictPending, LookupFailed: checkLookupFailed}, nil
	}
	verdict := reviewStreamVerdictFromGreptileSignal(signal)
	verdict.LookupFailed = checkLookupFailed
	// Issue comments carry no head SHA, so a comment written for an EARLIER
	// head would otherwise count as proof the reviewer spoke about this one —
	// and a silent reviewer on the current head would look alive forever. Only
	// a comment newer than the head commit is head-scoped evidence. The
	// legacy pass/fail semantics of the comment fallback are deliberately left
	// as they are; this narrows only the Observed signal.
	if scoped, lookupFailed := c.commentScopedToHead(comment, sha); !scoped {
		verdict.Observed = false
		if lookupFailed {
			// Could not prove the comment's age: absence of head-scoped
			// evidence here is unknown, not silence.
			verdict.LookupFailed = true
		}
	}
	if !verdict.Passed && !verdict.Pending && isActionableReviewSummary(comment.Body) {
		verdict.Findings = append(verdict.Findings, ReviewComment{Body: comment.Body, User: comment.User.Login})
	}
	if !verdict.Passed && !verdict.Pending {
		if _, findings, hasFindings, findingsErr := c.PRHighSeverityReviewOnHead(prNumber); findingsErr == nil && hasFindings {
			verdict.Findings = append(verdict.Findings, findings...)
		}
	}
	return verdict, nil
}

// greptileCommentDecision is the legacy comment-mode fallback. Only comments
// authored by Greptile are verdicts: operator prompts such as "@greptile
// review" or human status summaries must not turn a missing current-head check
// into a false rejection. GitHub returns issue comments oldest-first, so the
// latest bot verdict wins.
func greptileCommentDecision(comments []issueComment) (found bool, approved bool) {
	found, signal, _ := greptileCommentReviewSignal(comments)
	return found, signal.Passed
}

func greptileCommentReviewSignal(comments []issueComment) (found bool, signal greptileReviewSignal, comment issueComment) {
	for i := len(comments) - 1; i >= 0; i-- {
		comment = comments[i]
		if !isGreptileLogin(comment.User.Login) {
			continue
		}
		return true, greptileSignalFromText(comment.Body), comment
	}
	return false, greptileReviewSignal{}, issueComment{}
}

// commentScopedToHead reports whether an issue comment can be attributed to
// the given head commit, i.e. it was written after that commit existed.
//
// lookupFailed distinguishes the two reasons a comment is not scoped: it is
// genuinely older than the head (proof it says nothing about this head), or
// the commit timestamp could not be read (nothing is proven at all). Collapsing
// both into "not scoped" would let a rate-limited timestamp read look exactly
// like a silent reviewer — and a PR the reviewer REJECTED could then merge
// through the missing-review path.
func (c *Client) commentScopedToHead(comment issueComment, sha string) (scoped bool, lookupFailed bool) {
	if strings.TrimSpace(sha) == "" {
		return false, true
	}
	if comment.CreatedAt.IsZero() {
		// The comment carries no timestamp: unattributable, but the read itself
		// worked, so this is genuine "not scoped", not a degraded read.
		return false, false
	}
	headAt, err := c.commitCommittedAt(sha)
	if err != nil || headAt.IsZero() {
		return false, true
	}
	return !comment.CreatedAt.Before(headAt), false
}

// commitCommittedAt returns the committer timestamp of a commit. Only the
// review comment-fallback path calls it, which runs when no review check run
// exists at all.
func (c *Client) commitCommittedAt(sha string) (time.Time, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/commits/%s", c.Repo, sha))
	if err != nil {
		return time.Time{}, err
	}
	var payload struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return time.Time{}, err
	}
	return payload.Commit.Committer.Date, nil
}

func greptileCheckDecision(checkRuns []greptileCheckRun) (found bool, approved bool, pending bool) {
	found, signal := greptileCheckReviewSignal(checkRuns)
	return found, signal.Passed, signal.Pending
}

func greptileCheckReviewSignal(checkRuns []greptileCheckRun) (found bool, signal greptileReviewSignal) {
	for _, cr := range checkRuns {
		if !strings.Contains(strings.ToLower(cr.Name), "greptile") {
			continue
		}
		if cr.Status == "in_progress" || cr.Status == "queued" || cr.Status == "waiting" || cr.Conclusion == "" {
			return true, greptileReviewSignal{Pending: true, Verdict: reviewVerdictPending}
		}
		text := strings.Join([]string{cr.Output.Title, cr.Output.Summary, cr.Output.Text}, "\n")
		signal = greptileSignalFromText(text)
		if signal.Observed {
			return true, signal
		}
		if cr.Conclusion == "success" || cr.Conclusion == "neutral" {
			return true, greptileReviewSignal{Passed: true, Verdict: reviewVerdictPassed}
		}
		return true, greptileReviewSignal{Verdict: reviewVerdictRepairRequired}
	}
	return false, greptileReviewSignal{}
}

func greptileSignalFromText(text string) greptileReviewSignal {
	normalized := normalizedReviewText(text)
	signal := greptileReviewSignal{}
	for _, match := range greptileConfidenceScorePattern.FindAllStringSubmatch(normalized, -1) {
		if len(match) != 4 {
			continue
		}
		score, scoreErr := strconv.Atoi(match[2])
		maxScore, maxErr := strconv.Atoi(match[3])
		if scoreErr != nil || maxErr != nil || maxScore != 5 || score < 0 || score > maxScore {
			continue
		}
		signal.Score = score
		signal.ScoreMax = maxScore
		signal.Observed = true
	}

	negativeRanges := greptileNegativeMergePattern.FindAllStringIndex(normalized, -1)
	explicitObserved := false
	explicitPassed := false
	lastExplicitStart := -1
	for _, negativeRange := range negativeRanges {
		if len(negativeRange) == 2 && negativeRange[0] > lastExplicitStart {
			explicitObserved = true
			explicitPassed = false
			lastExplicitStart = negativeRange[0]
		}
	}
	for _, positiveRange := range greptilePositiveMergePattern.FindAllStringIndex(normalized, -1) {
		if !rangeContainedByAny(positiveRange, negativeRanges) && positiveRange[0] > lastExplicitStart {
			explicitObserved = true
			explicitPassed = true
			lastExplicitStart = positiveRange[0]
		}
	}
	repairRequired := strings.Contains(normalized, "repair required") || strings.Contains(normalized, "changes requested")
	if explicitObserved || repairRequired {
		signal.Observed = true
	}
	switch {
	case explicitObserved && explicitPassed:
		signal.Passed = true
		signal.Verdict = reviewVerdictOKToMerge
	case explicitObserved || repairRequired:
		signal.Verdict = reviewVerdictRepairRequired
	case signal.ScoreMax > 0:
		signal.Passed = signal.Score >= 4
		if signal.Passed {
			signal.Verdict = reviewVerdictPassed
		} else {
			signal.Verdict = reviewVerdictRepairRequired
		}
	default:
		signal.Verdict = reviewVerdictRepairRequired
	}
	return signal
}

func rangeContainedByAny(candidate []int, containers [][]int) bool {
	if len(candidate) != 2 {
		return false
	}
	for _, container := range containers {
		if len(container) == 2 && candidate[0] >= container[0] && candidate[1] <= container[1] {
			return true
		}
	}
	return false
}

// reviewStreamVerdictFromGreptileSignal builds a verdict from a signal the
// reviewer actually produced, so Observed is always true here. The only
// unobserved verdict is the explicit no-signal return in
// prGreptileReviewStreamVerdict.
func reviewStreamVerdictFromGreptileSignal(signal greptileReviewSignal) ReviewStreamVerdict {
	return ReviewStreamVerdict{
		Name:     "greptile",
		Passed:   signal.Passed,
		Pending:  signal.Pending,
		Observed: true,
		Score:    signal.Score,
		ScoreMax: signal.ScoreMax,
		Verdict:  signal.Verdict,
	}
}

func isGreptileLogin(login string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(login)), "greptile")
}

// isReviewBotLogin is the GLOBAL reviewer set for the gate-independent
// feedback consumers (CollectReviewFeedback and the issue-comment summary
// path), which run on every project regardless of the configured review
// streams. The llm-review bot logins deliberately do NOT belong here: the
// glue posts under the fleet bot account ("okbot"), and folding it into the
// global set made every fleet-bot comment with a file:line look like review
// feedback on greptile-gated projects too, triggering retry/close churn.
// llm-review attribution is scoped to the stream-verdict paths
// (reviewStreamSpec.UserContains) and — when the llm streams are configured —
// to PRBlockingReviewFindingsOnHead, PRHasCriticalReviewOnHead, and the
// feedback collectors (via isCollectableReviewFeedbackLogin).
func isReviewBotLogin(login string) bool {
	lower := strings.ToLower(strings.TrimSpace(login))
	return strings.Contains(lower, "greptile") || strings.Contains(lower, "codex")
}

// isLLMReviewBotLogin reports whether the login belongs to the bot that posts
// llm-review findings (#1148). The glue runner posts under the fleet bot
// account ("okbot" by default); any login containing "llm-review" also counts
// so a dedicated reviewer account works without a code change. Only consulted
// on paths that know the llm streams are configured — never globally (see
// isReviewBotLogin).
func isLLMReviewBotLogin(login string) bool {
	lower := strings.ToLower(strings.TrimSpace(login))
	return containsAny(lower, llmReviewBotLogins)
}

// llmReviewBotLogins are the login substrings attributed to the llm-review
// glue runner (#1148).
var llmReviewBotLogins = []string{"okbot", "llm-review"}

// llmReviewStreamsConfigured reports whether at least one llm-review model
// stream is part of the configured review streams (the "llm-review" pair
// alias normalizes to both model streams).
func llmReviewStreamsConfigured(streams []string) bool {
	return streamEnabled(streams, "llm-review-opus") ||
		streamEnabled(streams, "llm-review-terra") ||
		streamEnabled(streams, "llm-review-cursor")
}

// isCollectableReviewFeedbackLogin decides whose comments the feedback
// collectors (CollectReviewFeedback / CollectPRReviewFeedback) treat as
// review feedback. The global reviewer set (greptile/codex) is collected on
// every project; the llm-review bot ("okbot") is collected ONLY when the
// project's configured streams include an llm-review stream. Without the
// stream scoping one of two failure modes wins: folding okbot into the
// global set turns every fleet-bot comment with a file:line into review
// feedback on greptile rows (#1148 round 1, P1-3 churn), while dropping it
// entirely severs the repair circuit on llm-review rows — feedback comes
// back empty, AutoRetryReviewFeedback never fires, sessions never reach
// retry_exhausted, and the PR hangs forever (#1148 round 2, P1).
func isCollectableReviewFeedbackLogin(login string, streams []string) bool {
	if isReviewBotLogin(login) {
		return true
	}
	return llmReviewStreamsConfigured(streams) && isLLMReviewBotLogin(login)
}

func hasGreptileInlineCommentOnHead(comments []greptileReviewComment, sha string) bool {
	for _, comment := range comments {
		if !isGreptileLogin(comment.User.Login) {
			continue
		}
		if !reviewCommentTargetsHead(comment, sha) {
			continue
		}
		// Only block on P0 or P1 severity — P2/P3 are non-blocking
		if isHighSeverity(comment.Body) {
			return true
		}
	}
	return false
}

func reviewCommentTargetsHead(comment greptileReviewComment, sha string) bool {
	head := strings.TrimSpace(sha)
	if head == "" {
		return true
	}
	original := strings.TrimSpace(comment.OriginalCommitID)
	if original != "" {
		return original == head
	}
	commit := strings.TrimSpace(comment.CommitID)
	if commit == "" {
		return true
	}
	return commit == head
}

// isHighSeverity checks if a review comment is P0 or P1 severity.
// P2/P3 comments are informational and should not block merge.
// Matches both the Greptile badge markup (img alt / badge URLs) and the
// plain-text markers the llm-review glue emits (#1148): "[P0]" / "[P1]"
// and "severity: P0" / "severity: P1".
func isHighSeverity(body string) bool {
	return hasSeverityMarker(body, "p0") || hasSeverityMarker(body, "p1")
}

// isCriticalSeverity reports whether a review comment is P0 (critical) only.
// P1/P2/P3 are non-critical for the #565 convergence-merge escape.
func isCriticalSeverity(body string) bool {
	return hasSeverityMarker(body, "p0")
}

// hasSeverityMarker reports whether the comment body carries the given
// severity level ("p0"/"p1"/...) in any of the recognized encodings:
// Greptile badge markup (alt="P0", shields "badge/P0", ".../P0"), or the
// plain-text forms "[P0]" and "severity: P0" that a simple glue script can
// emit without any HTML (#1148).
func hasSeverityMarker(body, level string) bool {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "alt=\""+level+"\"") {
		return true
	}
	if strings.Contains(lower, "/"+level) {
		return true
	}
	if strings.Contains(lower, "badge/"+level) {
		return true
	}
	if strings.Contains(lower, "["+level+"]") {
		return true
	}
	if strings.Contains(lower, "severity: "+level) || strings.Contains(lower, "severity:"+level) {
		return true
	}
	return false
}

// hasCriticalReviewCommentOnHead reports whether a review finding on the
// current head must not be bypassed by the #565 convergence-merge escape.
// A Greptile P0 blocks UNCONDITIONALLY — regardless of the configured
// streams — preserving the pre-#1148 behavior: the Greptile app keeps
// reviewing a repo it is installed on even after the project row migrates to
// another gate, and a config migration must not silently downgrade a P0 from
// hard-block to advisory (#1148 round 2, P2). On top of that, when the
// llm-review streams are configured, a P0/P1 from the llm-review bot blocks
// too (P0/P1 is that gate's entire blocking contract; the glue never posts
// anything blocking below P1).
func hasCriticalReviewCommentOnHead(comments []greptileReviewComment, sha string, streams []string) bool {
	llmEnabled := llmReviewStreamsConfigured(streams)
	for _, comment := range comments {
		if !reviewCommentTargetsHead(comment, sha) {
			continue
		}
		login := comment.User.Login
		if isGreptileLogin(login) && isCriticalSeverity(comment.Body) {
			return true
		}
		if llmEnabled && isLLMReviewBotLogin(login) && isHighSeverity(comment.Body) {
			return true
		}
	}
	return false
}

// PRHasCriticalReviewOnHead reports whether the PR has a merge-blocking
// critical review comment on its current head SHA. Used by the orchestrator
// #565 convergence-merge escape: a retry-exhausted green PR with only
// non-critical findings may merge, but a critical finding on head
// hard-blocks. A Greptile P0 blocks regardless of the configured streams;
// the llm-review bot's P0/P1 blocking findings count only when those
// streams are configured (see hasCriticalReviewCommentOnHead).
func (c *Client) PRHasCriticalReviewOnHead(prNumber int, streams []string) (bool, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return false, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	comments, err := c.greptileReviewComments(prNumber)
	if err != nil {
		return false, fmt.Errorf("review comments for PR %d: %w", prNumber, err)
	}
	return hasCriticalReviewCommentOnHead(comments, sha, streams), nil
}

// PRHighSeverityReviewOnHead returns the head SHA and the list of P0/P1
// Greptile inline comments still on that head. Used by the #565
// auto-review-repair pipeline: the supervisor scopes the repair worker's
// prompt to exactly these comments (path / line / body) so the worker is
// not asked to re-implement the whole issue. Returns hasFindings=false
// when no high-severity comment remains on head (the convergence-merge
// path takes over) and a non-nil error only when the upstream lookups
// fail — never use error as a "no findings" signal.
func (c *Client) PRHighSeverityReviewOnHead(prNumber int) (sha string, findings []ReviewComment, hasFindings bool, err error) {
	sha, err = c.pullHeadSHA(prNumber)
	if err != nil {
		return "", nil, false, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	comments, err := c.greptileReviewComments(prNumber)
	if err != nil {
		return sha, nil, false, fmt.Errorf("greptile review comments for PR %d: %w", prNumber, err)
	}
	for _, cm := range comments {
		if !isGreptileLogin(cm.User.Login) {
			continue
		}
		if !reviewCommentTargetsHead(cm, sha) {
			continue
		}
		if !isHighSeverity(cm.Body) {
			continue
		}
		findings = append(findings, ReviewComment{
			Path: cm.Path,
			Line: cm.Line,
			Body: cm.Body,
			User: cm.User.Login,
		})
	}
	return sha, findings, len(findings) > 0, nil
}

func (c *Client) greptileReviewComments(prNumber int) ([]greptileReviewComment, error) {
	// Routed through the shared wrapper (#797) so this per-cycle read gets the
	// rate-limit backoff, the conditional-request cache, and usage accounting.
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100", c.Repo, prNumber), "--paginate")
	if err != nil {
		return nil, err
	}
	var comments []greptileReviewComment
	if err := json.Unmarshal(out, &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

// ClosePR closes a PR without merging and leaves a comment explaining why.
func (c *Client) ClosePR(prNumber int, comment string) error {
	if comment != "" {
		out, err := ghCommand("pr", "comment",
			fmt.Sprint(prNumber),
			"--repo", c.Repo,
			"--body", comment).CombinedOutput()
		if err != nil {
			return fmt.Errorf("gh pr comment %d: %w\n%s", prNumber, err, out)
		}
	}
	out, err := ghCommand("pr", "close",
		fmt.Sprint(prNumber),
		"--repo", c.Repo).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr close %d: %w\n%s", prNumber, err, out)
	}
	return nil
}

// PRChecksOutput returns a REST-derived check overview for a PR, useful for
// capturing CI failure details to pass to retry workers.
func (c *Client) PRChecksOutput(prNumber int) (string, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return "", fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	checks, checksErr := c.checkRunsForSHA(sha)
	combined, statusErr := c.combinedStatusForSHA(sha)
	if checksErr != nil && statusErr != nil {
		return "", fmt.Errorf("get checks for PR %d: check-runs: %v; statuses: %v", prNumber, checksErr, statusErr)
	}
	return formatChecksOverview(checks, combined), nil
}

// FailingCheck is one non-passing check-run on a PR head, carrying the check's
// name, its GitHub conclusion, and a best-effort excerpt of the failing log
// (the annotation / `##[error]` lines). Excerpt is empty when no log detail is
// fetchable, so a caller degrades gracefully to name + conclusion (#857).
type FailingCheck struct {
	Name       string
	Conclusion string
	Excerpt    string
}

// PRFailingChecks returns the check-runs on a PR's head SHA whose conclusion is
// a failure, each enriched with a best-effort excerpt of its failing log. It
// exists so a retry can surface a red lint check the worker's previous push
// introduced, instead of only the review feedback (or bare checks overview)
// that triggered the retry (#857). The excerpt is drawn from the check-run's
// own output fields, falling back to its failure annotations; either fetch
// failing leaves the excerpt empty rather than erroring, so the caller still
// names the check. A nil slice means no check is failing.
func (c *Client) PRFailingChecks(prNumber int) ([]FailingCheck, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return nil, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	checks, err := c.checkRunsForSHA(sha)
	if err != nil {
		return nil, fmt.Errorf("get check-runs for PR %d: %w", prNumber, err)
	}
	var failing []FailingCheck
	for _, ck := range checks {
		if !isFailingConclusion(ck.Conclusion) {
			continue
		}
		excerpt := checkRunOutputErrorLines(ck)
		if excerpt == "" || isGenericCheckExcerpt(excerpt) {
			// The output body was empty, or carried only generic runner
			// boilerplate ("Process completed with exit code 1") that names no
			// actionable error. Plain GitHub Actions jobs (e.g. agent-lint) emit
			// their real `::error::` detail as failure annotations, so fetch
			// those and prefer them. A fetch error, or no failure-level
			// annotation, leaves whatever the output gave us — the generic line
			// still names the failure and the call never fails (#857 review:
			// a generic excerpt must not hide the annotations fallback).
			if anns, aerr := c.checkRunAnnotations(ck.ID); aerr != nil {
				log.Printf("[github] warn: could not read annotations for check-run %q (id %d): %v", ck.Name, ck.ID, aerr)
			} else if formatted := formatFailureAnnotations(anns); formatted != "" {
				excerpt = formatted
			}
		}
		failing = append(failing, FailingCheck{
			Name:       ck.Name,
			Conclusion: ck.Conclusion,
			Excerpt:    excerpt,
		})
	}
	return failing, nil
}

// checkAnnotation is one entry from a check-run's annotations endpoint. GitHub
// records each `::error::` / `::warning::` workflow command a job emits as an
// annotation carrying the level and message.
type checkAnnotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	AnnotationLevel string `json:"annotation_level"`
	Message         string `json:"message"`
	Title           string `json:"title"`
}

func (c *Client) checkRunAnnotations(id int64) ([]checkAnnotation, error) {
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/check-runs/%d/annotations?per_page=100", c.Repo, id), "--paginate")
	if err != nil {
		return nil, err
	}
	anns, err := parseCheckAnnotations(out)
	if err != nil {
		return nil, fmt.Errorf("parse annotations for check-run %d: %w", id, err)
	}
	return anns, nil
}

// parseCheckAnnotations decodes the body of the annotations `--paginate` call.
// The annotations endpoint is a plain JSON array, so a check-run with more than
// 100 annotations yields back-to-back `[...][...]` documents that a single
// json.Unmarshal rejects; merge the pages first (same shape handled by
// mergePaginatedJSONArrays for the reconcile lists) so >100 annotations still
// parse and the failing-log excerpt survives (#857).
func parseCheckAnnotations(out []byte) ([]checkAnnotation, error) {
	merged, err := mergePaginatedJSONArrays(out)
	if err != nil {
		return nil, err
	}
	var anns []checkAnnotation
	if err := json.Unmarshal(merged, &anns); err != nil {
		return nil, err
	}
	return anns, nil
}

// failingConclusions are the check-run conclusions ciStatusFromREST treats as a
// hard failure; PRFailingChecks selects the same set so the excerpt path and the
// aggregate CI verdict never disagree on what "failing" means.
var failingConclusions = map[string]struct{}{
	"failure":         {},
	"timed_out":       {},
	"cancelled":       {},
	"action_required": {},
	"startup_failure": {},
	"stale":           {},
}

func isFailingConclusion(conclusion string) bool {
	_, ok := failingConclusions[strings.ToLower(strings.TrimSpace(conclusion))]
	return ok
}

// checkErrorLineRe matches an annotation / error line as emitted into a job log:
// a GitHub workflow command (`::error::…`, `::error file=…::…`), the Azure-style
// `##[error]…`, or a generic `Error:` prefix.
var checkErrorLineRe = regexp.MustCompile(`(?i)^(?:::error\b[^:]*::|##\[error\]|error:)`)

// checkErrorPrefixRe strips the recognized annotation prefix from a matched line
// so the surfaced text reads as the message alone.
var checkErrorPrefixRe = regexp.MustCompile(`(?i)^(?:::error\b[^:]*::|##\[error\]|error:\s*)`)

// genericCheckFailureLineRe matches the runner boilerplate a failed GitHub
// Actions step emits — "Error: Process completed with exit code 1" or "Error:
// The process '/usr/bin/bash' failed with exit code 1" — after checkErrorLineRe
// strips its `error:` prefix. Such a line names no actionable error.
var genericCheckFailureLineRe = regexp.MustCompile(`(?i)^(?:the process\b.*failed|process completed) with exit code \d+\.?$`)

// isGenericCheckExcerpt reports whether an output-derived excerpt consists only
// of generic runner boilerplate ("Process completed with exit code 1") that
// carries no actionable detail. Such an excerpt is non-empty yet useless to a
// retrying worker, so PRFailingChecks treats it like an empty excerpt and falls
// through to the check-run's failure annotations, where the real `::error::`
// lines (e.g. agent-lint) actually live (#857 review). An excerpt with any
// non-boilerplate line is kept as-is.
func isGenericCheckExcerpt(excerpt string) bool {
	var sawLine bool
	for _, raw := range strings.Split(excerpt, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !genericCheckFailureLineRe.MatchString(line) {
			return false
		}
		sawLine = true
	}
	return sawLine
}

// checkRunOutputErrorLines distills the error lines from a check-run's own
// output body (summary + text). It keeps only lines that look like an error
// annotation; if none match it falls back to the concise output summary, and to
// "" when the check reported no output at all (the graceful-degradation case).
func checkRunOutputErrorLines(ck greptileCheckRun) string {
	body := strings.TrimSpace(ck.Output.Text)
	if body == "" {
		body = strings.TrimSpace(ck.Output.Summary)
	} else if s := strings.TrimSpace(ck.Output.Summary); s != "" {
		body = s + "\n" + body
	}
	if body == "" {
		return ""
	}
	var lines []string
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if checkErrorLineRe.MatchString(line) {
			lines = append(lines, strings.TrimSpace(checkErrorPrefixRe.ReplaceAllString(line, "")))
		}
	}
	if len(lines) == 0 {
		// No annotation-shaped line — fall back to the human summary, which is
		// short and safe to surface, over the (possibly large) full text.
		return strings.TrimSpace(ck.Output.Summary)
	}
	return strings.Join(lines, "\n")
}

// formatFailureAnnotations renders the failure-level annotations of a check-run
// as one message per line, prefixing the file:line when GitHub attached one.
// Non-failure levels (warning/notice) are dropped so only blocking detail
// reaches the retry prompt.
func formatFailureAnnotations(anns []checkAnnotation) string {
	var lines []string
	for _, a := range anns {
		if !strings.EqualFold(strings.TrimSpace(a.AnnotationLevel), "failure") {
			continue
		}
		msg := strings.TrimSpace(a.Message)
		if msg == "" {
			msg = strings.TrimSpace(a.Title)
		}
		if msg == "" {
			continue
		}
		if p := strings.TrimSpace(a.Path); p != "" && p != ".github" {
			if a.StartLine > 0 {
				msg = fmt.Sprintf("%s:%d: %s", p, a.StartLine, msg)
			} else {
				msg = fmt.Sprintf("%s: %s", p, msg)
			}
		}
		lines = append(lines, msg)
	}
	return strings.Join(lines, "\n")
}

// MergePRAtHead squash-merges a PR only when its current head still matches
// expectedHeadSHA. GitHub enforces the comparison atomically with the merge,
// closing the force-push race between a gate/approval read and execution.
func (c *Client) MergePRAtHead(prNumber int, expectedHeadSHA string) error {
	expectedHeadSHA = strings.TrimSpace(expectedHeadSHA)
	if expectedHeadSHA == "" {
		return fmt.Errorf("merge PR %d: expected head SHA is required", prNumber)
	}
	out, err := ghCommand("pr", "merge",
		fmt.Sprint(prNumber),
		"--repo", c.Repo,
		"--squash",
		"--delete-branch",
		"--match-head-commit", expectedHeadSHA).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr merge %d at head %s: %w\n%s", prNumber, expectedHeadSHA, err, out)
	}
	return nil
}

// MarkPRReady marks a draft pull request as ready for review (wraps
// `gh pr ready`). `gh pr merge` on a draft fails with "still a draft", so
// the orchestrator readies a green draft PR before merging it (#697).
func (c *Client) MarkPRReady(prNumber int) error {
	out, err := ghCommand("pr", "ready",
		fmt.Sprint(prNumber),
		"--repo", c.Repo).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr ready %d: %w\n%s", prNumber, err, out)
	}
	return nil
}

// UpdateBranch merges the base branch into the PR's head branch so a
// green-but-BEHIND PR becomes up to date with main, satisfying the
// "branches must be up to date before merging" branch-protection rule.
// It is the non-bypass alternative to `gh pr merge --admin`: the updated
// head re-runs required checks and only merges once they pass.
//
// Updating the branch changes the PR head SHA, so any approval minted
// against the old head becomes stale — callers must NOT merge in the
// same pass; they should let the next supervisor cycle re-validate and
// re-mint against the new state (#547).
func (c *Client) UpdateBranch(prNumber int) error {
	out, err := ghCommand("pr", "update-branch",
		fmt.Sprint(prNumber),
		"--repo", c.Repo).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr update-branch %d: %w\n%s", prNumber, err, out)
	}
	return nil
}

// CloseIssue closes a GitHub issue and leaves a comment explaining why
func (c *Client) CloseIssue(number int, comment string) error {
	if comment != "" {
		out, err := ghCommand("issue", "comment",
			fmt.Sprint(number),
			"--repo", c.Repo,
			"--body", comment).CombinedOutput()
		if err != nil {
			return fmt.Errorf("gh issue comment %d: %w\n%s", number, err, out)
		}
	}
	out, err := ghCommand("issue", "close",
		fmt.Sprint(number),
		"--repo", c.Repo).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue close %d: %w\n%s", number, err, out)
	}
	return nil
}

// AddIssueLabel adds a label to an issue.
func (c *Client) AddIssueLabel(issueNumber int, label string) error {
	out, err := ghCommand("issue", "edit",
		strconv.Itoa(issueNumber),
		"--repo", c.Repo,
		"--add-label", label,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit --add-label: %w\n%s", err, out)
	}
	return nil
}

// EnsureLabel creates a repository label when it is missing. --force makes the
// operation idempotent under concurrent intake cycles and keeps the label's
// owned metadata deterministic when it already exists.
func (c *Client) EnsureLabel(name, color, description string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("ensure label: empty name")
	}
	args := []string{"label", "create", name, "--repo", c.Repo, "--force"}
	if color = strings.TrimPrefix(strings.TrimSpace(color), "#"); color != "" {
		args = append(args, "--color", color)
	}
	if description = strings.TrimSpace(description); description != "" {
		args = append(args, "--description", description)
	}
	out, err := ghCommand(args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh label create %q: %w\n%s", name, err, out)
	}
	return nil
}

// RemoveIssueLabel removes a label from an issue.
func (c *Client) RemoveIssueLabel(issueNumber int, label string) error {
	out, err := ghCommand("issue", "edit",
		strconv.Itoa(issueNumber),
		"--repo", c.Repo,
		"--remove-label", label,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit --remove-label: %w\n%s", err, out)
	}
	return nil
}

// CommentIssue leaves a comment on an issue.
func (c *Client) CommentIssue(issueNumber int, body string) error {
	out, err := ghCommand("issue", "comment",
		strconv.Itoa(issueNumber),
		"--repo", c.Repo,
		"--body", body,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue comment: %w\n%s", err, out)
	}
	return nil
}

// ListIssueComments returns the comments on an issue in chronological order,
// carrying each comment's ID, author login, and creation time. Used by the
// spec-groom mention trigger (#851) to poll for `@maestro groom` commands
// without a webhook dependency. GitHub treats PRs as issues, so this also
// works for PR numbers, but the supervisor only calls it for issues.
func (c *Client) ListIssueComments(issueNumber int) ([]IssueComment, error) {
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", c.Repo, issueNumber), "--paginate")
	if err != nil {
		return nil, fmt.Errorf("list issue comments for #%d: %w", issueNumber, err)
	}
	var raw []struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse issue #%d comments: %w", issueNumber, err)
	}
	comments := make([]IssueComment, 0, len(raw))
	for _, r := range raw {
		comments = append(comments, IssueComment{
			ID:        r.ID,
			Body:      r.Body,
			Author:    r.User.Login,
			CreatedAt: r.CreatedAt,
		})
	}
	return comments, nil
}

// CommentPR leaves a comment on a pull request.
func (c *Client) CommentPR(prNumber int, body string) error {
	out, err := ghCommand("pr", "comment",
		strconv.Itoa(prNumber),
		"--repo", c.Repo,
		"--body", body,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr comment: %w\n%s", err, out)
	}
	return nil
}

// PRLabels returns the labels on a PR.
func (c *Client) PRLabels(prNumber int) ([]string, error) {
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/issues/%d/labels?per_page=100", c.Repo, prNumber), "--paginate")
	if err != nil {
		return nil, fmt.Errorf("list PR %d labels: %w", prNumber, err)
	}
	names, err := parsePRLabels(out)
	if err != nil {
		return nil, err
	}
	return names, nil
}

// PRCommits returns commit messages for a PR.
func (c *Client) PRCommits(prNumber int) ([]string, error) {
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/pulls/%d/commits?per_page=100", c.Repo, prNumber), "--paginate")
	if err != nil {
		return nil, fmt.Errorf("list PR %d commits: %w", prNumber, err)
	}
	msgs, err := parsePRCommits(out)
	if err != nil {
		return nil, err
	}
	return msgs, nil
}

// CreateRelease creates a GitHub release for the given tag.
func (c *Client) CreateRelease(tag, title string) error {
	out, err := ghCommand("release", "create",
		tag,
		"--repo", c.Repo,
		"--title", title,
		"--generate-notes").CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh release create %s: %w\n%s", tag, err, out)
	}
	return nil
}

// HasOpenPRForIssue returns true if there is at least one open PR that
// references the given issue number (e.g. "closes #N") in its body or title.
func (c *Client) HasOpenPRForIssue(issueNumber int) (bool, error) {
	prs, err := c.ListOpenPRs()
	if err != nil {
		return false, err
	}
	for _, pr := range prs {
		if prReferencesIssue(pr, issueNumber) {
			return true, nil
		}
	}
	return false, nil
}

// dependencyInlinePattern matches `Depends on: #147` or
// `Depends on: #148, #149` (case-insensitive, tolerant of leading/trailing
// whitespace and trailing punctuation). Used by FindDependencies alongside a
// scan of a structured `## Dependencies` section to extract every issue the
// blocked wave member is waiting on.
//
// Kept narrow on purpose: handoff issue templates write "Depends on:" and
// occasionally "Depends:". We don't pretend to understand free-form English.
var dependencyInlinePattern = regexp.MustCompile(`(?im)^\s*depends(?:\s+on)?\s*[:\-]\s*([^\r\n]+)$`)

// dependencyIssueNumber matches a `#147` reference inside a parsed dependency
// line or section. Reused for both inline and structured forms.
var dependencyIssueNumber = regexp.MustCompile(`#(\d+)`)

var dependencyNegatingQualifierPattern = regexp.MustCompile(`(?i)\b(independently\s+mergeable|independent\s+mergeable|but\s+is\s+independent|but\s+independent|not\s+blocked|not\s+a\s+blocker)\b`)

// FindDependencies scans an issue body for dependency references in two
// supported shapes (see issue #442):
//
//   - inline: `Depends on: #147` or `Depends on: #148, #149`
//   - structured section: a `## Dependencies` (or `### Dependencies`) heading
//     followed by lines containing `#NNN` issue references.
//
// Returns deduplicated issue numbers in the order they first appear. Returns
// nil for the zero body. The function is intentionally tolerant of common
// shapes (`Depends:` without `on`, mixed case, trailing punctuation) but does
// not invent dependencies — only explicit `#N` references count.
func FindDependencies(body string) []int {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	seen := make(map[int]struct{})
	var deps []int
	add := func(n int) {
		if n <= 0 {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		deps = append(deps, n)
	}

	// Inline `Depends on:` lines.
	for _, match := range dependencyInlinePattern.FindAllStringSubmatch(body, -1) {
		if len(match) < 2 {
			continue
		}
		for _, ref := range dependencyIssueNumber.FindAllStringSubmatch(match[1], -1) {
			if len(ref) < 2 {
				continue
			}
			n, err := strconv.Atoi(ref[1])
			if err != nil {
				continue
			}
			add(n)
		}
	}

	// Structured `## Dependencies` / `### Dependencies` section. A section ends
	// at the next markdown heading at the same-or-shallower level, or EOF.
	for _, section := range extractDependenciesSections(body) {
		for _, ref := range dependencyIssueNumber.FindAllStringSubmatch(section, -1) {
			if len(ref) < 2 {
				continue
			}
			n, err := strconv.Atoi(ref[1])
			if err != nil {
				continue
			}
			add(n)
		}
	}

	return deps
}

// extractDependenciesSections returns the markdown body of every
// "Dependencies" section in the given body. A section spans from a heading
// line whose text equals "Dependencies" (case-insensitive, ignoring trailing
// punctuation) until the next heading of equal-or-shallower depth, or end of
// file.
func extractDependenciesSections(body string) []string {
	lines := strings.Split(body, "\n")
	var sections []string
	for i := 0; i < len(lines); i++ {
		level, heading, ok := headingLevelAndText(lines[i])
		if !ok || !strings.EqualFold(heading, "dependencies") {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			otherLevel, _, otherOK := headingLevelAndText(lines[j])
			if otherOK && otherLevel <= level {
				end = j
				break
			}
		}
		sections = append(sections, strings.Join(lines[i+1:end], "\n"))
		i = end - 1
	}
	return sections
}

// headingLevelAndText returns the heading depth (1 for `#`, 2 for `##`, ...)
// and trimmed text when line is a markdown ATX heading. Trailing colons and
// whitespace are removed so "## Dependencies:" matches "Dependencies".
func headingLevelAndText(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level >= len(trimmed) || (trimmed[level] != ' ' && trimmed[level] != '\t') {
		return 0, "", false
	}
	text := strings.TrimSpace(trimmed[level:])
	text = strings.TrimRight(text, " \t:")
	return level, text, true
}

// childInlinePattern matches `Children: #147, #148` (case-insensitive). Used
// by FindChildIssues alongside a scan of structured child sections.
var childInlinePattern = regexp.MustCompile(`(?im)^\s*child(?:ren)?(?:\s+issues?)?\s*[:\-]\s*([^\r\n]+)$`)

// childSectionHeadings is the set of markdown headings that FindChildIssues
// treats as a structured list of child issue references. The supervisor
// epic-completion aggregate (sup-162) reads any `#N` token inside one of
// these sections — including checked / unchecked task-list items — as a
// child of the epic.
var childSectionHeadings = []string{
	"children",
	"child issues",
	"child issue",
	"subtasks",
	"sub-tasks",
	"sub tasks",
	"issue wave",
	"wave",
	"slices",
	"epic checklist",
}

// FindChildIssues scans an issue body for child issue references used by
// the epic-completion aggregate. It recognises two shapes:
//
//   - inline: `Children: #147, #148` (also `Child issues:` / `Child:`)
//   - structured section: a markdown heading whose text matches one of
//     childSectionHeadings (case-insensitive) followed by any lines that
//     contain `#NNN` issue references. Task list items (`- [ ] #147`) and
//     plain bullets are both recognised.
//
// Returns deduplicated issue numbers in the order they first appear. The
// epic issue's own number is filtered out so a `Refs #<self>` line in
// the body never inflates progress. Returns nil for the zero body.
func FindChildIssues(body string) []int {
	return findChildIssues(body, 0)
}

// FindChildIssuesExcluding behaves like FindChildIssues but skips the given
// issue number even when it appears in the body. Callers use this to filter
// out the epic's own number when it is known.
func FindChildIssuesExcluding(body string, selfNumber int) []int {
	return findChildIssues(body, selfNumber)
}

func findChildIssues(body string, selfNumber int) []int {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	seen := make(map[int]struct{})
	var children []int
	add := func(n int) {
		if n <= 0 || (selfNumber > 0 && n == selfNumber) {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		children = append(children, n)
	}

	for _, match := range childInlinePattern.FindAllStringSubmatch(body, -1) {
		if len(match) < 2 {
			continue
		}
		for _, ref := range dependencyIssueNumber.FindAllStringSubmatch(match[1], -1) {
			if len(ref) < 2 {
				continue
			}
			n, err := strconv.Atoi(ref[1])
			if err != nil {
				continue
			}
			add(n)
		}
	}

	for _, section := range extractChildSections(body) {
		for _, ref := range dependencyIssueNumber.FindAllStringSubmatch(section, -1) {
			if len(ref) < 2 {
				continue
			}
			n, err := strconv.Atoi(ref[1])
			if err != nil {
				continue
			}
			add(n)
		}
	}

	return children
}

// extractChildSections returns the markdown body of every section in the
// given body whose heading text matches childSectionHeadings. A section
// spans from its heading line until the next heading of equal-or-shallower
// depth, or end of file.
func extractChildSections(body string) []string {
	lines := strings.Split(body, "\n")
	var sections []string
	for i := 0; i < len(lines); i++ {
		level, heading, ok := headingLevelAndText(lines[i])
		if !ok || !isChildSectionHeading(heading) {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			otherLevel, _, otherOK := headingLevelAndText(lines[j])
			if otherOK && otherLevel <= level {
				end = j
				break
			}
		}
		sections = append(sections, strings.Join(lines[i+1:end], "\n"))
		i = end - 1
	}
	return sections
}

func isChildSectionHeading(heading string) bool {
	heading = strings.ToLower(strings.TrimSpace(heading))
	for _, name := range childSectionHeadings {
		if heading == name {
			return true
		}
	}
	return false
}

// FindBlockers scans an issue body for blocker references matching the given
// regex patterns. Each pattern must contain a capture group for the issue number.
// Returns deduplicated issue numbers referenced as blockers.
func FindBlockers(body string, patterns []string) []int {
	seen := make(map[int]struct{})
	var blockers []int
	for _, pat := range patterns {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			if dependencyNegatingQualifierPattern.MatchString(line) {
				continue
			}
			for _, match := range re.FindAllStringSubmatch(line, -1) {
				if len(match) < 2 {
					continue
				}
				n, err := strconv.Atoi(match[1])
				if err != nil || n <= 0 {
					continue
				}
				if _, ok := seen[n]; !ok {
					seen[n] = struct{}{}
					blockers = append(blockers, n)
				}
			}
		}
	}
	return blockers
}

// CreateIssue creates a new GitHub issue and returns its number.
func (c *Client) CreateIssue(title, body string, labels []string) (int, error) {
	args := []string{
		"issue", "create",
		"--repo", c.Repo,
		"--title", title,
		"--body", body,
	}
	for _, l := range labels {
		args = append(args, "--label", l)
	}

	out, err := ghCommand(args...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("gh issue create: %w\n%s", err, out)
	}

	// gh issue create prints the URL; extract issue number from last path segment
	url := strings.TrimSpace(string(out))
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return 0, fmt.Errorf("unexpected gh issue create output: %s", url)
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0, fmt.Errorf("parse issue number from %q: %w", url, err)
	}
	return n, nil
}

// EditIssueBody updates the body of a GitHub issue.
func (c *Client) EditIssueBody(number int, body string) error {
	out, err := ghCommand("issue", "edit",
		strconv.Itoa(number),
		"--repo", c.Repo,
		"--body", body,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit %d --body: %w\n%s", number, err, out)
	}
	return nil
}

// FindOpenPRForIssue returns the first open PR that references the given issue number.
// Returns pr number, branch name, and whether one was found.
func (c *Client) FindOpenPRForIssue(issueNumber int) (prNumber int, branch string, found bool, err error) {
	prs, err := c.ListOpenPRs()
	if err != nil {
		return 0, "", false, err
	}
	for _, pr := range prs {
		if prReferencesIssue(pr, issueNumber) {
			return pr.Number, pr.HeadRefName, true, nil
		}
	}
	return 0, "", false, nil
}

// ReviewComment is an exported review comment (from Greptile, Codex, or any reviewer).
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
	User string `json:"user"`
}

type ReviewStreamVerdict struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Pending bool   `json:"pending"`
	// Observed reports whether the reviewer produced ANY signal for this head
	// — a check run or a comment, in any state including "queued". Without it
	// Pending conflates two very different situations: "the reviewer is
	// working on it" and "the reviewer never showed up". The second happens
	// whenever the review service is silent (live 2026-07: Greptile paused
	// reviews after its free credits ran out and posted nothing at all), and
	// an unobserved pending gate would otherwise hold every PR forever with
	// no timeout and no alert.
	Observed bool `json:"observed"`
	// LookupFailed reports that a read the verdict depends on errored (e.g.
	// the check-runs API), so "no signal" here means "unknown", not "the
	// reviewer is silent". Callers must not conclude absence from it: a GitHub
	// outage would otherwise look exactly like a reviewer that never showed up.
	LookupFailed bool            `json:"lookup_failed,omitempty"`
	Score        int             `json:"score,omitempty"`
	ScoreMax     int             `json:"score_max,omitempty"`
	Verdict      string          `json:"verdict,omitempty"`
	Findings     []ReviewComment `json:"findings,omitempty"`
}

type ReviewGateVerdict struct {
	Passed  bool `json:"passed"`
	Pending bool `json:"pending"`
	// Observed is false when NO configured stream produced any signal, i.e.
	// the whole review gate is silent rather than working.
	Observed bool `json:"observed"`
	// LookupFailed is true when any stream's read was degraded, so an absent
	// signal cannot be read as proof the reviewer is silent.
	LookupFailed bool                  `json:"lookup_failed,omitempty"`
	Streams      []ReviewStreamVerdict `json:"streams"`
}

func (v ReviewGateVerdict) BlockingFindings() []ReviewComment {
	var findings []ReviewComment
	for _, stream := range v.Streams {
		findings = append(findings, stream.Findings...)
	}
	return findings
}

func (v ReviewGateVerdict) Summary() string {
	if len(v.Streams) == 0 {
		return "review gate disabled"
	}
	var parts []string
	for _, stream := range v.Streams {
		status := "pass"
		switch {
		case stream.Pending:
			status = "pending"
		case !stream.Passed:
			status = "findings"
		}
		if stream.ScoreMax > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d/%d %s", stream.Name, stream.Score, stream.ScoreMax, status))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%s", stream.Name, status))
		}
	}
	return strings.Join(parts, ", ")
}

var reviewLocationPattern = regexp.MustCompile(`(?mi)(^|\s)([A-Za-z0-9_./-]+\.[A-Za-z0-9]+:\d+|file:\s*\S+)`)

// CollectReviewFeedback collects actionable inline review comments on a PR
// from the global reviewer set (Greptile/Codex) and — when the project's
// configured review streams include an llm-review stream — from the
// llm-review bot (see isCollectableReviewFeedbackLogin for why the bot is
// stream-scoped).
func (c *Client) CollectReviewFeedback(prNumber int, streams []string) ([]ReviewComment, error) {
	// Get HEAD SHA — only return comments on the latest commit. Errors are
	// tolerated (empty SHA matches all comments, as before); the shared
	// wrapper (#797) gives the read backoff + conditional caching.
	headSHA, _ := c.pullHeadSHA(prNumber)

	comments, err := c.greptileReviewComments(prNumber)
	if err != nil {
		return nil, err
	}
	var result []ReviewComment
	for _, cm := range comments {
		login := cm.User.Login
		if !isCollectableReviewFeedbackLogin(login, streams) {
			continue
		}
		// Skip comments that were originally left on older commits — they may already be fixed.
		if !reviewCommentTargetsHead(cm, headSHA) {
			continue
		}
		comment := ReviewComment{
			Path: cm.Path,
			Line: cm.Line,
			Body: cm.Body,
			User: login,
		}
		if !isActionableReviewComment(comment) {
			continue
		}
		result = append(result, comment)
	}
	return result, nil
}

func (c *Client) PRReviewGateVerdict(prNumber int, streams []string) (ReviewGateVerdict, error) {
	normalized := normalizeReviewStreams(streams)
	verdict := ReviewGateVerdict{Passed: true}
	if len(normalized) == 0 {
		return verdict, nil
	}
	for _, stream := range normalized {
		var sv ReviewStreamVerdict
		var err error
		switch stream {
		case "greptile":
			sv, err = c.greptileReviewStreamVerdict(prNumber)
		case "simplicity":
			sv, err = c.namedReviewStreamVerdict(prNumber, reviewStreamSpec{
				Name:          "simplicity",
				CheckContains: []string{"simplicity", "over-engineering", "overengineering"},
				UserContains:  []string{"simplicity", "maestro-simplicity", "over-engineering", "overengineering"},
			})
		case "llm-review-opus", "llm-review-terra", "llm-review-cursor":
			sv, err = c.namedReviewStreamVerdict(prNumber, llmReviewStreamSpec(stream))
		default:
			continue
		}
		if err != nil {
			return ReviewGateVerdict{}, err
		}
		verdict.Streams = append(verdict.Streams, sv)
		if sv.Observed {
			// One reviewer speaking is enough for the gate to count as live:
			// the missing-review policy only applies when the gate as a whole
			// is silent.
			verdict.Observed = true
		}
		if sv.LookupFailed {
			// One degraded read makes the whole gate's silence unproven.
			verdict.LookupFailed = true
		}
		if sv.Pending {
			verdict.Pending = true
			verdict.Passed = false
		}
		if !sv.Passed {
			verdict.Passed = false
		}
	}
	return verdict, nil
}

func (c *Client) PRBlockingReviewFindingsOnHead(prNumber int, streams []string) (sha string, findings []ReviewComment, hasFindings bool, err error) {
	sha, err = c.pullHeadSHA(prNumber)
	if err != nil {
		return "", nil, false, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	comments, err := c.greptileReviewComments(prNumber)
	if err != nil {
		return sha, nil, false, fmt.Errorf("review comments for PR %d: %w", prNumber, err)
	}
	for _, cm := range comments {
		if !reviewCommentTargetsHead(cm, sha) {
			continue
		}
		login := cm.User.Login
		body := cm.Body
		if streamEnabled(streams, "greptile") && isGreptileLogin(login) && isHighSeverity(body) {
			findings = append(findings, ReviewComment{Path: cm.Path, Line: cm.Line, Body: body, User: login})
			continue
		}
		if streamEnabled(streams, "simplicity") && isSimplicityReviewerLogin(login) && isActionableReviewComment(ReviewComment{Path: cm.Path, Line: cm.Line, Body: body, User: login}) {
			findings = append(findings, ReviewComment{Path: cm.Path, Line: cm.Line, Body: body, User: login})
			continue
		}
		// llm-review (#1148): only P0/P1 block, whichever of the two model
		// streams posted the finding — the repair scope is the union.
		if llmReviewStreamsConfigured(streams) && isLLMReviewBotLogin(login) && isHighSeverity(body) {
			findings = append(findings, ReviewComment{Path: cm.Path, Line: cm.Line, Body: body, User: login})
		}
	}
	return sha, findings, len(findings) > 0, nil
}

type reviewStreamSpec struct {
	Name          string
	CheckContains []string
	UserContains  []string
	// BodyContains, when non-empty, additionally requires an inline comment
	// body to contain one of these markers before it is attributed to this
	// stream. The llm-review streams share one bot login, so the per-model
	// marker embedded in each comment is what tells the two streams apart.
	BodyContains []string
	// HighSeverityOnly makes only P0/P1 findings block this stream. P2/P3
	// comments stay advisory (#1148 severity contract). Streams without it
	// keep the historical behavior: any actionable comment blocks.
	HighSeverityOnly bool
	// AllowCommitStatus lets the stream's external verdict arrive as a plain
	// commit status (POST /statuses/{sha}) in addition to a check run. Check
	// runs require a GitHub App; the llm-review glue posts statuses with a
	// normal token (#1148).
	AllowCommitStatus bool
}

// llmReviewStreamSpec builds the spec for one of the two llm-review streams
// (#1148): the external verdict is the commit status / check run named after
// the stream, and inline findings are the bot's comments carrying the
// stream's marker. Only P0/P1 findings block; P2/P3 stay advisory.
func llmReviewStreamSpec(name string) reviewStreamSpec {
	return reviewStreamSpec{
		Name:              name,
		CheckContains:     []string{name},
		UserContains:      llmReviewBotLogins,
		BodyContains:      []string{name},
		HighSeverityOnly:  true,
		AllowCommitStatus: true,
	}
}

func (c *Client) greptileReviewStreamVerdict(prNumber int) (ReviewStreamVerdict, error) {
	return c.prGreptileReviewStreamVerdict(prNumber)
}

func (c *Client) namedReviewStreamVerdict(prNumber int, spec reviewStreamSpec) (ReviewStreamVerdict, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return ReviewStreamVerdict{}, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	checks, checkErr := c.checkRunsForSHA(sha)
	var checkFound, checkPassed, checkPending bool
	if checkErr == nil {
		checkFound, checkPassed, checkPending = namedCheckDecision(checks, spec.CheckContains)
	}
	// The llm-review glue posts plain commit statuses (a normal token cannot
	// create check runs — those need a GitHub App). When no check run spoke
	// for this stream, look at the combined status for the same needles.
	var statusErr error
	if spec.AllowCommitStatus && !checkFound {
		var combined combinedStatusResponse
		combined, statusErr = c.combinedStatusForSHA(sha)
		if statusErr == nil {
			checkFound, checkPassed, checkPending = namedStatusDecision(combined, spec.CheckContains)
		}
	}
	findings, commentsErr := c.reviewFindingsForStream(prNumber, sha, spec)
	if commentsErr != nil && checkErr != nil {
		return ReviewStreamVerdict{}, commentsErr
	}
	sv := ReviewStreamVerdict{Name: spec.Name, Passed: false, Pending: false, Findings: findings}
	// A check run in any state, or an inline finding, means the reviewer spoke
	// for this head. The default branch below is the silent case — but only a
	// successful read can prove silence.
	sv.Observed = checkFound || checkPending || len(findings) > 0
	sv.LookupFailed = checkErr != nil || commentsErr != nil || statusErr != nil
	switch {
	case checkPending:
		sv.Pending = true
	case len(findings) > 0:
		// Any actionable inline finding from this reviewer blocks, even if
		// the external check has already settled successfully.
	case checkFound:
		sv.Passed = checkPassed
		if !checkPassed {
			sv.Findings = []ReviewComment{{
				Body: fmt.Sprintf("%s review check did not pass", spec.Name),
				User: spec.Name,
			}}
		}
	default:
		sv.Pending = true
	}
	return sv, nil
}

func namedCheckDecision(checks []greptileCheckRun, needles []string) (found bool, passed bool, pending bool) {
	for _, cr := range checks {
		name := strings.ToLower(cr.Name)
		if !containsAny(name, needles) {
			continue
		}
		found = true
		if cr.Conclusion == "success" || cr.Conclusion == "neutral" {
			return true, true, false
		}
		if cr.Status == "in_progress" || cr.Status == "queued" || cr.Status == "waiting" || cr.Conclusion == "" {
			return true, false, true
		}
		return true, false, false
	}
	return false, false, false
}

// namedStatusDecision is namedCheckDecision for plain commit statuses: the
// combined status already reports the latest status per context, so the first
// matching context is the stream's verdict.
func namedStatusDecision(combined combinedStatusResponse, needles []string) (found bool, passed bool, pending bool) {
	for _, st := range combined.Statuses {
		if !containsAny(strings.ToLower(st.Context), needles) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(st.State)) {
		case "success":
			return true, true, false
		case "pending":
			return true, false, true
		default: // "failure", "error"
			return true, false, false
		}
	}
	return false, false, false
}

func (c *Client) reviewFindingsForStream(prNumber int, sha string, spec reviewStreamSpec) ([]ReviewComment, error) {
	comments, err := c.greptileReviewComments(prNumber)
	if err != nil {
		return nil, err
	}
	return filterStreamFindings(comments, sha, spec), nil
}

// filterStreamFindings selects the inline comments on the given head that this
// stream attributes to itself and considers blocking. Pure so the attribution
// and severity rules are testable without the API reads.
func filterStreamFindings(comments []greptileReviewComment, sha string, spec reviewStreamSpec) []ReviewComment {
	var findings []ReviewComment
	for _, cm := range comments {
		if !reviewCommentTargetsHead(cm, sha) {
			continue
		}
		if !containsAny(strings.ToLower(cm.User.Login), spec.UserContains) {
			continue
		}
		if len(spec.BodyContains) > 0 && !containsAny(strings.ToLower(cm.Body), spec.BodyContains) {
			continue
		}
		if spec.HighSeverityOnly && !isHighSeverity(cm.Body) {
			continue
		}
		comment := ReviewComment{Path: cm.Path, Line: cm.Line, Body: cm.Body, User: cm.User.Login}
		if !isActionableReviewComment(comment) {
			continue
		}
		findings = append(findings, comment)
	}
	return findings
}

func normalizeReviewStreams(streams []string) []string {
	if len(streams) == 0 {
		return []string{"greptile"}
	}
	out := make([]string, 0, len(streams))
	seen := map[string]struct{}{}
	add := func(name string) {
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, raw := range streams {
		name := strings.ToLower(strings.TrimSpace(raw))
		switch name {
		case "greptile", "simplicity", "llm-review-opus", "llm-review-terra", "llm-review-cursor":
			add(name)
		case "llm-review":
			// The pair alias (#1148): "llm-review" means both model lenses.
			// llm-review-cursor is opt-in only, never expanded by the alias.
			add("llm-review-opus")
			add("llm-review-terra")
		default:
			continue
		}
	}
	return out
}

func streamEnabled(streams []string, name string) bool {
	for _, stream := range normalizeReviewStreams(streams) {
		if stream == name {
			return true
		}
	}
	return false
}

func isSimplicityReviewerLogin(login string) bool {
	lower := strings.ToLower(strings.TrimSpace(login))
	return containsAny(lower, []string{"simplicity", "maestro-simplicity", "over-engineering", "overengineering"})
}

func containsAny(s string, needles []string) bool {
	for _, needle := range needles {
		if needle = strings.ToLower(strings.TrimSpace(needle)); needle != "" && strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func isActionableReviewComment(comment ReviewComment) bool {
	body := strings.TrimSpace(comment.Body)
	if body == "" || isNonActionableReviewText(body) {
		return false
	}
	if strings.TrimSpace(comment.Path) != "" || comment.Line > 0 {
		return true
	}
	return isActionableReviewSummary(body)
}

func isActionableReviewSummary(body string) bool {
	body = strings.TrimSpace(body)
	if body == "" || isNonActionableReviewText(body) {
		return false
	}
	return hasActionableReviewMarker(body)
}

func isNonActionableReviewText(body string) bool {
	lower := normalizedReviewText(body)
	if lower == "" {
		return true
	}
	if strings.Contains(lower, "not safe to merge") || strings.Contains(lower, "unsafe to merge") {
		return false
	}

	nonActionable := []string{
		"no actionable comments",
		"no actionable feedback",
		"no actionable issues",
		"no blocking issues",
		"no bugs found",
		"no changes requested",
		"no findings",
		"no issues found",
		"no issues were found",
		"no review comments",
		"nothing to fix",
		"review complete with no findings",
		"review passed",
		"safe to merge",
		"looks good to me",
		"looks good",
		"lgtm",
		"found 0 issues",
		"0 issues found",
	}
	for _, phrase := range nonActionable {
		if strings.Contains(lower, phrase) {
			return true
		}
	}

	if strings.Contains(lower, "codex") && strings.Contains(lower, "reviewed") &&
		(strings.Contains(lower, "left comments") || strings.Contains(lower, "review comments")) &&
		!hasActionableReviewMarker(body) {
		return true
	}

	return false
}

func hasActionableReviewMarker(body string) bool {
	if reviewLocationPattern.MatchString(body) {
		return true
	}
	lower := normalizedReviewText(body)
	markers := []string{
		"not safe to merge",
		"unsafe to merge",
		"changes requested",
		"must fix",
		"please fix",
		"needs fix",
		"action required",
		"blocking",
		"regression",
		"bug",
		"crash",
		"panic",
		"nil pointer",
		"data race",
		"security",
		"vulnerability",
		"incorrect",
		"broken",
		"failing",
		"leak",
		"deadlock",
		"p0",
		"p1",
		"p2",
		"p3",
		"severity:",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func normalizedReviewText(body string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(body)), " "))
}

// FormatReviewFeedback formats review comments into a text block for worker prompts.
func FormatReviewFeedback(comments []ReviewComment) string {
	if len(comments) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n## Review Feedback (fix these issues)\n\n")
	sb.WriteString("The following review comments were left on your PR. Fix each one, commit, and push to the same branch.\n\n")
	for i, c := range comments {
		sb.WriteString(fmt.Sprintf("### Comment %d", i+1))
		if c.User != "" {
			sb.WriteString(fmt.Sprintf(" (from %s)", c.User))
		}
		sb.WriteString("\n")
		if c.Path != "" {
			sb.WriteString(fmt.Sprintf("File: %s", c.Path))
			if c.Line > 0 {
				sb.WriteString(fmt.Sprintf(", Line: %d", c.Line))
			}
			sb.WriteString("\n")
		}
		sb.WriteString(c.Body)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// CIFailureSummary gets the CI check run failure summary for a PR.
func (c *Client) CIFailureSummary(prNumber int) (string, error) {
	// 1. Get check overview
	overview, err := c.PRChecksOutput(prNumber)
	if err != nil {
		overview = err.Error()
	}

	// 2. Find failed job IDs and fetch their logs
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil || sha == "" {
		return overview, nil
	}

	checks, err := c.checkRunsForSHA(sha)
	if err != nil {
		return overview, nil
	}
	var failed []greptileCheckRun
	for _, check := range checks {
		switch strings.ToLower(strings.TrimSpace(check.Conclusion)) {
		case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
			failed = append(failed, check)
		}
	}
	if len(failed) == 0 {
		return overview, nil
	}

	var result strings.Builder
	result.WriteString("CI Check Overview:\n")
	result.WriteString(overview)
	result.WriteString("\n\n")

	for _, check := range failed {
		result.WriteString(fmt.Sprintf("=== Failed check: %s ===\n", check.Name))
		if check.Output.Summary != "" {
			result.WriteString(check.Output.Summary)
			result.WriteString("\n")
		}
		if check.Output.Text != "" {
			result.WriteString(check.Output.Text)
			result.WriteString("\n")
		}
		if check.DetailsURL != "" {
			result.WriteString("Details: ")
			result.WriteString(check.DetailsURL)
			result.WriteString("\n")
		} else if check.HTMLURL != "" {
			result.WriteString("Details: ")
			result.WriteString(check.HTMLURL)
			result.WriteString("\n")
		}
		result.WriteString("\n")
	}

	s := result.String()
	if len(s) > 8000 {
		s = s[:8000] + "\n... (truncated)"
	}
	return s, nil
}

// CollectPRReviewFeedback collects actionable review feedback from a PR —
// inline review comments and issue-level summary comments — for the global
// reviewer set plus, when the configured streams include an llm-review
// stream, the llm-review bot. Returns a formatted string ready to inject
// into a worker prompt, or empty string if no actionable review feedback
// exists.
func (c *Client) CollectPRReviewFeedback(prNumber int, streams []string) (string, error) {
	var sections []string

	// 1. Fetch issue-level comments (Greptile summary with confidence score),
	// via the shared wrapper (#797) for backoff + conditional caching.
	issueCommentsOut, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", c.Repo, prNumber), "--paginate")
	if err == nil {
		var comments []struct {
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if json.Unmarshal(issueCommentsOut, &comments) == nil {
			for _, cm := range comments {
				if isCollectableReviewFeedbackLogin(cm.User.Login, streams) && isActionableReviewSummary(cm.Body) {
					sections = append(sections, cm.Body)
				}
			}
		}
	}

	// 2. Fetch inline review comments
	inlineComments, err := c.CollectReviewFeedback(prNumber, streams)
	if err == nil && len(inlineComments) > 0 {
		sections = append(sections, FormatReviewFeedback(inlineComments))
	}

	if len(sections) == 0 {
		return "", nil
	}

	return strings.Join(sections, "\n\n"), nil
}

// HasLabel returns true if any of the issue's labels match
func HasLabel(issue Issue, labels []string) bool {
	for _, l := range issue.Labels {
		for _, excl := range labels {
			if strings.EqualFold(l.Name, excl) {
				return true
			}
		}
	}
	return false
}
