package approver

// Delivery executor (#872): the execution stage of the approval-gated
// post-merge delivery. Given an APPROVED deploy_project approval, it:
//
//  1. takes the durable approved→executing claim in the SQLite store BEFORE any
//     side effect, so a daemon and a CLI approve contend on one transition and
//     exactly one runs the delivery (and a restart observing executing never
//     replays it);
//  2. pins the exact merged revision — it verifies LocalPath's remote identity,
//     fetches from the canonical approved repository, and materializes the
//     payload's MergedSHA without inheriting mutable checkout behavior;
//  3. runs the project's delivery command once, with the configured timeout;
//  4. runs the configured live/deployment verifier;
//  5. records only structured terminal metadata (stage, exit code, timeout,
//     revision, timestamps) — never command/output/error text.
//
// The delivery command and verifier are read from config at execute time, NOT
// from the approval payload — so a secret inlined into a command never enters
// the durable approval record.

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// CommandRunner runs a shell command in dir under ctx and returns combined
// stdout+stderr plus any error. The default runner uses `bash -c`. Tests inject
// a fake to exercise success/failure/timeout without touching a real service.
type CommandRunner interface {
	Run(ctx context.Context, dir, command string) (output string, err error)
}

// CommandRunnerFunc adapts a plain function to CommandRunner.
type CommandRunnerFunc func(ctx context.Context, dir, command string) (string, error)

func (f CommandRunnerFunc) Run(ctx context.Context, dir, command string) (string, error) {
	return f(ctx, dir, command)
}

// DeliveryFreshnessChecker proves that the approved merge is still the newest
// authoritative GitHub merge generation immediately before execution. It is
// called twice: once before the durable claim and once immediately after it,
// before any checkout or delivery side effect. Production must always provide
// one; tests may inject a deterministic function.
type DeliveryFreshnessChecker interface {
	CheckDeliveryFreshness(context.Context, *state.DeliveryPayload) error
}

// DeliveryFreshnessFunc adapts a function to DeliveryFreshnessChecker.
type DeliveryFreshnessFunc func(context.Context, *state.DeliveryPayload) error

func (f DeliveryFreshnessFunc) CheckDeliveryFreshness(ctx context.Context, payload *state.DeliveryPayload) error {
	return f(ctx, payload)
}

// LatestMergedGenerationReader is the authoritative GitHub read needed by the
// execution-time freshness fence. Implementations return every generation tied
// at the latest merged_at second; silently picking one by PR number would turn
// GitHub's second-resolution timestamp into an unsafe ordering oracle.
type LatestMergedGenerationReader interface {
	LatestMergedPRGenerations(context.Context) ([]github.PRMergeInfo, error)
}

// NewGitHubDeliveryFreshnessChecker builds the production freshness fence.
// Different revisions with the same merged_at second are ordered only by an
// isolated remote ancestry proof. A read/topology error or incomparable pair
// fails closed without returning provider text that might contain credentials.
func NewGitHubDeliveryFreshnessChecker(reader LatestMergedGenerationReader, repo, sourceDir string) DeliveryFreshnessChecker {
	return DeliveryFreshnessFunc(func(ctx context.Context, approved *state.DeliveryPayload) error {
		if reader == nil || approved == nil {
			return ErrDeliveryFreshnessUnverified
		}
		latest, err := reader.LatestMergedPRGenerations(ctx)
		if err != nil || len(latest) == 0 {
			return ErrDeliveryFreshnessUnverified
		}
		return checkLatestDeliveryGenerations(ctx, approved, latest, func(ctx context.Context, ancestor, descendant string) (bool, error) {
			return RevisionContains(ctx, repo, sourceDir, ancestor, descendant)
		})
	})
}

type revisionContainsFunc func(context.Context, string, string) (bool, error)

func checkLatestDeliveryGenerations(ctx context.Context, approved *state.DeliveryPayload, latest []github.PRMergeInfo, contains revisionContainsFunc) error {
	approvedSHA := strings.ToLower(strings.TrimSpace(approved.MergedSHA))
	if !validFullRevision(approvedSHA) || approved.MergedAt.IsZero() || contains == nil {
		return ErrDeliveryFreshnessUnverified
	}
	approvedAt := approved.MergedAt.UTC()
	for _, generation := range latest {
		sha := strings.ToLower(strings.TrimSpace(generation.SHA))
		if !validFullRevision(sha) || generation.MergedAt.IsZero() {
			return ErrDeliveryFreshnessUnverified
		}
		mergedAt := generation.MergedAt.UTC()
		if sha == approvedSHA {
			// Exact immutable identity wins over timestamp formatting drift. Keep
			// checking other same-latest generations before declaring freshness.
			continue
		}
		if mergedAt.After(approvedAt) {
			return ErrDeliverySuperseded
		}
		if mergedAt.Before(approvedAt) {
			// An authoritative "latest" set cannot predate an approved merge it
			// omitted. Treat the inconsistent read as unavailable, not fresh.
			return ErrDeliveryFreshnessUnverified
		}

		// GitHub REST timestamps have second precision. For a tie, prove order
		// from the canonical remote and ignore mutable local refs/config.
		forward, err := contains(ctx, approvedSHA, sha)
		if err != nil {
			return ErrDeliveryFreshnessUnverified
		}
		if forward {
			return ErrDeliverySuperseded
		}
		reverse, err := contains(ctx, sha, approvedSHA)
		if err != nil {
			return ErrDeliveryFreshnessUnverified
		}
		if !reverse {
			return ErrDeliveryGenerationAmbiguous
		}
	}
	return nil
}

// PreparedCheckout is an isolated, immutable execution surface for one
// approved merge revision. Cleanup removes the standalone temporary git
// repository. Its path is intentionally runtime-only and never persisted.
type PreparedCheckout struct {
	Dir      string
	Revision string
	Cleanup  func() error
}

// CheckoutPreparer fetches the approved revision and materializes a clean,
// detached checkout without changing the authoritative LocalPath worktree.
type CheckoutPreparer interface {
	Prepare(ctx context.Context, sourceDir, approvedRevision string) (*PreparedCheckout, error)
}

// CheckoutPreparerFunc adapts a plain function to CheckoutPreparer.
type CheckoutPreparerFunc func(context.Context, string, string) (*PreparedCheckout, error)

func (f CheckoutPreparerFunc) Prepare(ctx context.Context, sourceDir, approvedRevision string) (*PreparedCheckout, error) {
	return f(ctx, sourceDir, approvedRevision)
}

// bashRunner is the default CommandRunner: `bash -c <command>` in dir, output
// bounded by ctx timeout. A context deadline surfaces as a distinct error so
// the caller can label a timeout.
type bashRunner struct{ limit int }

// entrypointRunner is the approval-gated runner. The approved config contract
// is one argument-free repository-relative executable, so invoking it directly
// avoids `bash -c`, shell expansion, and inherited shell startup hooks. The
// entrypoint itself may use a committed shebang; interpreter/loader injection
// variables are removed while ordinary target credentials remain available.
type entrypointRunner struct{ limit int }

type boundedCapture struct {
	mu      sync.Mutex
	data    []byte
	limit   int
	dropped int
}

func (b *boundedCapture) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	want := len(p)
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		b.data = append(b.data, p[:remaining]...)
	}
	b.dropped += want - remaining
	return want, nil
}

func (b *boundedCapture) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := string(b.data)
	if b.dropped > 0 {
		out += fmt.Sprintf("\n…[capture truncated %d bytes]", b.dropped)
	}
	return out
}

func (r bashRunner) Run(ctx context.Context, dir, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = dir
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
	limit := r.limit
	if limit <= 0 {
		limit = state.DefaultDeliveryOutputLimit
	}
	capture := &boundedCapture{limit: limit}
	cmd.Stdout = capture
	cmd.Stderr = capture
	err := cmd.Run()
	out := capture.String()
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("timed out: %w", context.DeadlineExceeded)
	}
	return out, err
}

func (r entrypointRunner) Run(ctx context.Context, dir, command string) (string, error) {
	entrypoint, err := secureDeliveryEntrypoint(dir, command)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, entrypoint)
	cmd.Dir = dir
	cmd.Env = sanitizedDeliveryEnv(os.Environ())
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
	limit := r.limit
	if limit <= 0 {
		limit = state.DefaultDeliveryOutputLimit
	}
	capture := &boundedCapture{limit: limit}
	cmd.Stdout = capture
	cmd.Stderr = capture
	err = cmd.Run()
	out := capture.String()
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("timed out: %w", context.DeadlineExceeded)
	}
	return out, err
}

// sanitizedDeliveryEnv preserves normal deployment variables (including
// target credentials) but removes process-loader, shell-startup, directory,
// and exported-function controls that can execute code before the committed
// entrypoint. Git controls are also removed because scripts must not inherit a
// mutable object/config view when inspecting the exact-SHA checkout.
func sanitizedDeliveryEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "LD_") ||
			strings.HasPrefix(key, "DYLD_") || strings.HasPrefix(key, "BASH_FUNC_") {
			continue
		}
		switch key {
		case "PATH", "BASH_ENV", "ENV", "CDPATH", "SHELLOPTS", "BASHOPTS", "PROMPT_COMMAND", "ZDOTDIR",
			"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONINSPECT",
			"PERL5OPT", "PERL5LIB", "PERLLIB", "PERL_LOCAL_LIB_ROOT",
			"RUBYOPT", "RUBYLIB", "GEM_HOME", "GEM_PATH",
			"NODE_OPTIONS", "NODE_PATH",
			"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS":
			continue
		}
		out = append(out, entry)
	}
	// `/usr/bin/env` shebangs resolve only from a controlled system path; a
	// mutable operator PATH must not substitute an interpreter before the
	// committed entrypoint starts.
	return append(out, "PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/usr/local/sbin")
}

// RunBoundedShell exposes the same process-group-safe, memory-bounded shell
// runner for the explicit automatic/legacy delivery path. Callers must still
// sanitize output before logging or persistence.
func RunBoundedShell(ctx context.Context, dir, command string, outputLimit int) (string, error) {
	return (bashRunner{limit: outputLimit}).Run(ctx, dir, command)
}

// gitIsolatedPreparer is the production checkout fence. LocalPath is used only
// for a hook-free, include-free read of remote.origin.url so the configured
// checkout can be bound to the approved owner/repository. The approved object
// is fetched from a canonical GitHub HTTPS URL into a brand-new repository,
// then materialized with the built-in git archive stream. In particular, no
// hook, filter, fsmonitor, credential helper, URL rewrite, or checkout setting
// from LocalPath is inherited or executed.
//
// fetchURL is a test-only seam for a local bare origin. Production always
// leaves it empty and derives https://github.com/<owner>/<repo>.git from the
// repo already bound into the approval payload/hash and executor guard.
type gitIsolatedPreparer struct {
	expectedRepo string
	fetchURL     string
}

// NewLocalFixtureCheckoutPreparer exposes the production-hardened isolated
// materializer to self-checks and tests backed by a local bare origin. Runtime
// delivery must leave Checkout nil so the canonical GitHub remote is derived
// from the approval-bound owner/repository identity.
func NewLocalFixtureCheckoutPreparer(expectedRepo, fetchURL string) CheckoutPreparer {
	return gitIsolatedPreparer{expectedRepo: expectedRepo, fetchURL: fetchURL}
}

// RevisionContains reports whether ancestor is reachable from descendant in
// the approved GitHub repository. It deliberately does not consult LocalPath's
// object database, refs, replace refs, grafts, commit graph, or Git config:
// both exact objects are fetched from the canonical remote into a brand-new
// repository under the same sanitized Git environment used for delivery
// materialization.
func RevisionContains(ctx context.Context, expectedRepo, sourceDir, ancestor, descendant string) (bool, error) {
	return revisionContainsFromRemote(ctx, gitIsolatedPreparer{expectedRepo: expectedRepo}, sourceDir, ancestor, descendant)
}

func revisionContainsFromRemote(ctx context.Context, p gitIsolatedPreparer, sourceDir, ancestor, descendant string) (bool, error) {
	ancestor = strings.ToLower(strings.TrimSpace(ancestor))
	descendant = strings.ToLower(strings.TrimSpace(descendant))
	if !validFullRevision(ancestor) || !validFullRevision(descendant) || len(ancestor) != len(descendant) {
		return false, errors.New("revision ancestry requires two full same-format git object IDs")
	}
	expectedRepo, fetchURL, err := p.expectedRemote()
	if err != nil {
		return false, err
	}
	if err := validateSourceOrigin(ctx, sourceDir, expectedRepo, fetchURL, p.fetchURL != ""); err != nil {
		return false, err
	}

	base, err := os.MkdirTemp("", "maestro-delivery-ancestry-")
	if err != nil {
		return false, errors.New("create isolated ancestry repository failed")
	}
	defer os.RemoveAll(base)
	templateDir := filepath.Join(base, "empty-template")
	if err := os.Mkdir(templateDir, 0o700); err != nil {
		return false, errors.New("create empty git template failed")
	}
	repoDir := filepath.Join(base, "repo")
	initArgs := []string{"init", "--quiet", "--template=" + templateDir}
	if len(ancestor) == 64 {
		initArgs = append(initArgs, "--object-format=sha256")
	}
	initArgs = append(initArgs, repoDir)
	if err := runIsolatedGitQuiet(ctx, "", initArgs...); err != nil {
		return false, fmt.Errorf("initialize isolated ancestry repository: %w", err)
	}
	for _, revision := range []string{ancestor, descendant} {
		if err := runIsolatedGitQuiet(ctx, repoDir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", fetchURL, revision); err != nil {
			return false, fmt.Errorf("fetch exact ancestry revision from canonical remote: %w", err)
		}
		if err := runIsolatedGitQuiet(ctx, repoDir, "cat-file", "-e", revision+"^{commit}"); err != nil {
			return false, fmt.Errorf("verify ancestry revision is a commit: %w", err)
		}
	}

	cmd := isolatedGitCommand(ctx, repoDir, "merge-base", "--is-ancestor", ancestor, descendant)
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base failed: %w", err)
}

func (p gitIsolatedPreparer) Prepare(ctx context.Context, sourceDir, approvedRevision string) (*PreparedCheckout, error) {
	pinned := strings.ToLower(strings.TrimSpace(approvedRevision))
	if !validFullRevision(pinned) {
		return nil, fmt.Errorf("approved revision is not a full hexadecimal git object ID")
	}
	expectedRepo, fetchURL, err := p.expectedRemote()
	if err != nil {
		return nil, err
	}
	if err := validateSourceOrigin(ctx, sourceDir, expectedRepo, fetchURL, p.fetchURL != ""); err != nil {
		return nil, err
	}

	base, err := os.MkdirTemp("", "maestro-delivery-checkout-")
	if err != nil {
		return nil, fmt.Errorf("create temporary checkout root: %w", err)
	}
	cleanupOnError := func(err error) (*PreparedCheckout, error) {
		_ = os.RemoveAll(base)
		return nil, err
	}

	// An explicitly empty template prevents even a machine-level template from
	// planting hooks in the new repository. System/global Git config and all
	// inherited GIT_* controls are disabled by isolatedGitCommand below.
	templateDir := filepath.Join(base, "empty-template")
	if err := os.Mkdir(templateDir, 0o700); err != nil {
		return cleanupOnError(fmt.Errorf("create empty git template: %w", err))
	}
	checkout := filepath.Join(base, "checkout")
	initArgs := []string{"init", "--quiet", "--template=" + templateDir}
	if len(pinned) == 64 {
		initArgs = append(initArgs, "--object-format=sha256")
	}
	initArgs = append(initArgs, checkout)
	if err := runIsolatedGitQuiet(ctx, "", initArgs...); err != nil {
		return cleanupOnError(fmt.Errorf("initialize isolated checkout: %w", err))
	}
	if err := runIsolatedGitQuiet(ctx, checkout, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", fetchURL, pinned); err != nil {
		return cleanupOnError(fmt.Errorf("fetch approved revision from canonical remote: %w", err))
	}
	if err := runIsolatedGitQuiet(ctx, checkout, "cat-file", "-e", pinned+"^{commit}"); err != nil {
		return cleanupOnError(fmt.Errorf("verify approved revision is a commit: %w", err))
	}
	if err := extractGitArchive(ctx, checkout, pinned, checkout); err != nil {
		return cleanupOnError(fmt.Errorf("materialize approved revision: %w", err))
	}
	// read-tree records the exact approved tree in the index without checking
	// out files (and therefore without invoking clean/smudge filters). --no-deref
	// turns HEAD into the exact detached commit without running checkout hooks.
	if err := runIsolatedGitQuiet(ctx, checkout, "read-tree", pinned); err != nil {
		return cleanupOnError(fmt.Errorf("index approved revision: %w", err))
	}
	if err := runIsolatedGitQuiet(ctx, checkout, "update-ref", "--no-deref", "HEAD", pinned); err != nil {
		return cleanupOnError(fmt.Errorf("detach approved revision: %w", err))
	}
	head, err := runIsolatedGitText(ctx, checkout, 256, "rev-parse", "HEAD")
	if err != nil {
		return cleanupOnError(fmt.Errorf("inspect isolated checkout: %w", err))
	}
	if !revisionMatches(head, pinned) {
		return cleanupOnError(errors.New("isolated checkout does not match the approved revision"))
	}

	cleanup := func() error {
		if err := os.RemoveAll(base); err != nil {
			// os.PathError embeds the ephemeral absolute path, so persist only a
			// path-free warning. The checkout path itself is never audit data.
			return errors.New("remove temporary checkout files failed")
		}
		return nil
	}

	return &PreparedCheckout{Dir: checkout, Revision: head, Cleanup: cleanup}, nil
}

func (p gitIsolatedPreparer) expectedRemote() (repo, fetchURL string, err error) {
	repo, err = canonicalGitHubRepo(p.expectedRepo)
	if err != nil {
		return "", "", err
	}
	if p.fetchURL != "" {
		return repo, p.fetchURL, nil
	}
	return repo, "https://github.com/" + repo + ".git", nil
}

func validateSourceOrigin(ctx context.Context, sourceDir, expectedRepo, fetchURL string, localFixture bool) error {
	remote, err := runIsolatedGitText(ctx, sourceDir, 4<<10, "config", "--local", "--no-includes", "--get-all", "remote.origin.url")
	if err != nil {
		return fmt.Errorf("read configured checkout origin without executing repo config: %w", err)
	}
	if strings.ContainsAny(remote, "\r\n") {
		return errors.New("configured checkout origin is ambiguous")
	}
	if localFixture {
		got, gotErr := filepath.Abs(strings.TrimSpace(remote))
		want, wantErr := filepath.Abs(fetchURL)
		if gotErr != nil || wantErr != nil || filepath.Clean(got) != filepath.Clean(want) {
			return errors.New("configured checkout origin does not match the approved remote identity")
		}
		return nil
	}
	repo, err := githubRepoFromRemote(remote)
	if err != nil || !strings.EqualFold(repo, expectedRepo) {
		return errors.New("configured checkout origin does not match the approved GitHub repository")
	}
	return nil
}

func canonicalGitHubRepo(repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !validGitHubRepoPart(parts[0]) || !validGitHubRepoPart(parts[1]) {
		return "", fmt.Errorf("repo must use the canonical owner/repository form")
	}
	return parts[0] + "/" + parts[1], nil
}

func validGitHubRepoPart(part string) bool {
	if part == "" || part == "." || part == ".." {
		return false
	}
	for _, ch := range part {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return true
}

func githubRepoFromRemote(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if strings.HasPrefix(remote, "git@github.com:") {
		return canonicalGitHubRepo(strings.TrimSuffix(strings.TrimPrefix(remote, "git@github.com:"), ".git"))
	}
	u, err := url.Parse(remote)
	if err != nil || (u.Scheme != "https" && u.Scheme != "ssh") ||
		!strings.EqualFold(u.Hostname(), "github.com") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("origin is not a canonical GitHub HTTPS/SSH remote")
	}
	if u.Scheme == "https" && (u.User != nil || (u.Port() != "" && u.Port() != "443")) {
		return "", errors.New("GitHub HTTPS origin embeds credentials or a non-standard port")
	}
	if u.Scheme == "ssh" {
		if u.User == nil || u.User.Username() != "git" || u.User.String() != "git" || (u.Port() != "" && u.Port() != "22") {
			return "", errors.New("GitHub SSH origin has an unexpected identity")
		}
	}
	repo := strings.TrimPrefix(pathpkg.Clean(u.EscapedPath()), "/")
	unescaped, err := url.PathUnescape(repo)
	if err != nil || unescaped != repo {
		return "", errors.New("GitHub origin contains escaped path data")
	}
	return canonicalGitHubRepo(strings.TrimSuffix(repo, ".git"))
}

func isolatedGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	gitArgs := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"-c", "core.attributesFile=/dev/null",
		"-c", "core.excludesFile=/dev/null",
		"-c", "credential.helper=",
		"-c", "protocol.ext.allow=never",
		"--no-replace-objects",
	}
	// Private GitHub repos remain usable without inheriting any mutable Git
	// credential helper. gh is an operator-installed trusted helper; its path,
	// not a token, is placed in the process arguments and nothing is persisted.
	if ghPath, ok := trustedSystemExecutable("gh"); ok {
		gitArgs = append(gitArgs, "-c", "credential.helper=!"+singleQuote(ghPath)+" auth git-credential")
	}
	if dir != "" {
		gitArgs = append(gitArgs, "-C", dir)
	}
	gitArgs = append(gitArgs, args...)
	gitPath, ok := trustedSystemExecutable("git")
	if !ok {
		// An impossible absolute path fails closed without consulting ambient
		// PATH. Callers map the resulting PathError to a path-free stage error.
		gitPath = "/nonexistent/maestro-trusted-git"
	}
	cmd := exec.CommandContext(ctx, gitPath, gitArgs...)
	cmd.Env = sanitizedGitEnv(os.Environ())
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

func sanitizedGitEnv(env []string) []string {
	// Materialization is infrastructure, not the deployment entrypoint. Use a
	// narrow allow-list so AWS/ADB/database/target credentials cannot reach git
	// or the gh credential helper merely because the eventual deploy needs them.
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
	return append(out,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/usr/local/sbin",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GCM_INTERACTIVE=Never",
	)
}

// trustedSystemExecutable resolves only from fixed system-owned locations and
// returns an absolute regular executable. Ambient PATH is deliberately never
// consulted for git/gh because delivery materialization occurs before the
// approved project entrypoint and therefore sits on the authorization fence.
func trustedSystemExecutable(name string) (string, bool) {
	return trustedExecutableWithinRoots(name, []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/local/bin", "/usr/local/sbin"})
}

func trustedExecutableWithinRoots(name string, allowedRoots []string) (string, bool) {
	if name == "" || strings.ContainsRune(name, filepath.Separator) {
		return "", false
	}
	for _, root := range allowedRoots {
		canonicalRoot, err := filepath.EvalSymlinks(root)
		if err != nil || !filepath.IsAbs(canonicalRoot) || !trustedRootOwnedPath(canonicalRoot, canonicalRoot) {
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
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 && trustedRootOwnedPath(resolved, canonicalRoot) {
			return resolved, true
		}
	}
	return "", false
}

// trustedRootOwnedPath verifies both the executable and every directory up to
// its allowlisted root. A fixed absolute path alone is insufficient when an
// unprivileged account can replace /usr/local/bin/gh or one of its parents.
func trustedRootOwnedPath(path, root string) bool {
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

func singleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runIsolatedGitQuiet(ctx context.Context, dir string, args ...string) error {
	cmd := isolatedGitCommand(ctx, dir, args...)
	if err := cmd.Run(); err != nil {
		// Deliberately omit git's output and command arguments: either may contain
		// a credential-bearing environment value or the ephemeral checkout path.
		return fmt.Errorf("git %s failed: %w", args[0], err)
	}
	return nil
}

func runIsolatedGitText(ctx context.Context, dir string, limit int, args ...string) (string, error) {
	capture := &boundedCapture{limit: limit}
	cmd := isolatedGitCommand(ctx, dir, args...)
	cmd.Stdout = capture
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s failed: %w", args[0], err)
	}
	if capture.dropped != 0 {
		return "", fmt.Errorf("git %s output exceeded safe limit", args[0])
	}
	return strings.TrimSpace(capture.String()), nil
}

func extractGitArchive(ctx context.Context, repoDir, revision, destination string) error {
	cmd := isolatedGitCommand(ctx, repoDir, "archive", "--format=tar", revision)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("open git archive stream failed")
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git archive failed: %w", err)
	}
	extractErr := extractTarTree(stdout, destination)
	if extractErr != nil && cmd.Cancel != nil {
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		return fmt.Errorf("git archive failed: %w", waitErr)
	}
	return nil
}

func extractTarTree(r io.Reader, destination string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.New("read git archive failed")
		}
		name, err := safeArchiveName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "" || hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if err := ensureSafeArchiveParents(destination, filepath.Dir(target)); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			info, statErr := os.Lstat(target)
			switch {
			case errors.Is(statErr, os.ErrNotExist):
				if err := os.Mkdir(target, os.FileMode(hdr.Mode)&0o777); err != nil {
					return errors.New("create archived directory failed")
				}
			case statErr != nil:
				return errors.New("inspect archived directory failed")
			case !info.IsDir():
				return errors.New("git archive directory collides with a non-directory")
			}
		case tar.TypeReg, tar.TypeRegA:
			if _, err := os.Lstat(target); err == nil {
				return errors.New("git archive contains a duplicate path")
			} else if !errors.Is(err, os.ErrNotExist) {
				return errors.New("inspect archived path failed")
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return errors.New("create archived file failed")
			}
			_, copyErr := io.CopyN(f, tr, hdr.Size)
			closeErr := f.Close()
			if copyErr != nil || closeErr != nil {
				return errors.New("write archived file failed")
			}
		case tar.TypeSymlink:
			if strings.ContainsRune(hdr.Linkname, '\x00') {
				return errors.New("git archive contains an invalid symlink")
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return errors.New("create archived symlink failed")
			}
		default:
			return fmt.Errorf("git archive contains unsupported entry type %d", hdr.Typeflag)
		}
	}
}

func safeArchiveName(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	raw := strings.TrimSuffix(name, "/")
	clean := pathpkg.Clean(raw)
	if raw == "" || pathpkg.IsAbs(raw) || clean != raw || strings.ContainsRune(raw, '\x00') {
		return "", errors.New("git archive contains an unsafe path")
	}
	for _, part := range strings.Split(raw, "/") {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("git archive contains an unsafe path")
		}
		if strings.EqualFold(part, ".git") {
			return "", errors.New("git archive attempts to replace repository metadata")
		}
	}
	return raw, nil
}

func ensureSafeArchiveParents(root, parent string) error {
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("git archive path escapes the isolated checkout")
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(current, 0o755); err != nil {
				return errors.New("create archived parent directory failed")
			}
		case err != nil:
			return errors.New("inspect archived parent directory failed")
		case !info.IsDir():
			return errors.New("git archive parent is not a directory")
		}
	}
	return nil
}

// secureDeliveryEntrypoint resolves the strict ./repo/relative execution
// contract without following symlinks. Every path component must already be a
// real directory in the pristine checkout and the final node must be a regular
// executable file. This blocks a committed symlink such as
// ./scripts/deploy.sh -> /mutable/operator/path from escaping the approved tree.
func secureDeliveryEntrypoint(root, command string) (string, error) {
	if command == "" || command != strings.TrimSpace(command) || !strings.HasPrefix(command, "./") {
		return "", ErrDeliveryEntrypointUnsafe
	}
	rel := strings.TrimPrefix(command, "./")
	if rel == "" || pathpkg.IsAbs(rel) || pathpkg.Clean(rel) != rel || strings.HasPrefix(rel, "../") {
		return "", ErrDeliveryEntrypointUnsafe
	}
	for _, ch := range rel {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '/' || ch == '_' || ch == '-' || ch == '.' {
			continue
		}
		return "", ErrDeliveryEntrypointUnsafe
	}

	root = filepath.Clean(root)
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", ErrDeliveryEntrypointUnsafe
	}
	current := root
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", ErrDeliveryEntrypointUnsafe
		}
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", ErrDeliveryEntrypointUnsafe
		}
		if i < len(parts)-1 {
			if !info.IsDir() {
				return "", ErrDeliveryEntrypointUnsafe
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", ErrDeliveryEntrypointUnsafe
		}
	}
	return current, nil
}

func validFullRevision(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	for _, ch := range revision {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

// DeliveryExecutor drives one approved deploy_project approval to its delivery.
type DeliveryExecutor struct {
	// Store is the SQLite approvals store that arbitrates the durable
	// approved→executing claim and records the terminal result. Required.
	Store *approvalstore.Store
	// StateDir scopes the claim to one project's rows in the shared DB.
	StateDir string
	// Repo binds the executor to a project; a delivery approval stamped with a
	// different repo is refused (cross-project mutation guard).
	Repo string
	// Delivery is the resolved delivery config (command / verifier / timeout)
	// read at execute time. The raw command is never taken from the approval.
	Delivery config.DeliveryConfig

	// Runner / Checkout default to direct entrypoint execution + an isolated
	// exact-revision checkout when nil.
	Runner   CommandRunner
	Checkout CheckoutPreparer
	// Actor labels audit entries (e.g. "daemon", operator name).
	Actor string
	// OutputLimit bounds runtime-only capture; output is always discarded and
	// never stored, returned in DeliveryResult, or logged. 0 uses the default.
	OutputLimit int
	// Freshness is the authoritative execution-time GitHub generation fence.
	// It is mandatory: an approval that cannot prove it is still the newest
	// merged generation never claims or runs a side effect.
	Freshness DeliveryFreshnessChecker
	// Now overrides the clock in tests. nil uses time.Now().UTC().
	Now func() time.Time
}

// DeliveryResult is the outcome of a Deliver call.
type DeliveryResult struct {
	Approval *state.Approval
	Status   state.ApprovalStatus
	Summary  string
	Err      error
	// Skipped is true when no side effect ran because the claim was lost, the
	// approval was not deliverable, or it was already in flight/terminal.
	Skipped bool
}

// ErrNotDeliverable is returned when an approval is not an executable delivery.
var ErrNotDeliverable = errors.New("approval is not an executable deploy_project delivery")

// ErrDeliveryConfigMismatch means the command/verifier/operator context no
// longer matches what was approved. It is checked before the durable claim so
// no side effect runs and the operator can mint a fresh approval.
var ErrDeliveryConfigMismatch = approvalstore.ErrDeliveryConfigMismatch

// ErrDeliveryVerifierRequired fails closed when an approval-gated delivery has
// no live/deployment verifier. A command succeeding is not proof the product
// was deployed or installed successfully.
var ErrDeliveryVerifierRequired = errors.New("approval-gated delivery requires verify_command")

var (
	ErrDeliveryFreshnessUnverified = errors.New("delivery merge freshness could not be verified")
	ErrDeliverySuperseded          = errors.New("delivery was superseded by a newer merged generation")
	ErrDeliveryGenerationAmbiguous = errors.New("delivery merge generation order is ambiguous")
	ErrDeliveryEntrypointUnsafe    = errors.New("delivery entrypoint is not a safe committed executable")
)

const deliveryFreshnessTimeout = 2 * time.Minute

var (
	ErrDeliveryPreconditionFailed = errors.New("delivery precondition failed")
	ErrDeliveryCheckoutFailed     = errors.New("delivery checkout failed")
	ErrDeliveryCommandFailed      = errors.New("delivery command failed")
	ErrDeliveryVerificationFailed = errors.New("delivery verification failed")
)

var errDeliveryCheckoutRevisionMismatch = errors.New("delivery checkout revision mismatch")

func (d *DeliveryExecutor) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d *DeliveryExecutor) runner() CommandRunner {
	if d.Runner != nil {
		return d.Runner
	}
	return entrypointRunner{limit: d.OutputLimit}
}

func (d *DeliveryExecutor) checkout() CheckoutPreparer {
	if d.Checkout != nil {
		return d.Checkout
	}
	return gitIsolatedPreparer{expectedRepo: d.Repo}
}

func (d *DeliveryExecutor) actor() string {
	if strings.TrimSpace(d.Actor) != "" {
		return d.Actor
	}
	return "delivery-executor"
}

func (d *DeliveryExecutor) checkFreshness(ctx context.Context, payload *state.DeliveryPayload) error {
	if d.Freshness == nil {
		return ErrDeliveryFreshnessUnverified
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkCtx, cancel := context.WithTimeout(ctx, deliveryFreshnessTimeout)
	defer cancel()
	if err := d.Freshness.CheckDeliveryFreshness(checkCtx, payload.Clone()); err != nil {
		switch {
		case errors.Is(err, ErrDeliverySuperseded):
			return ErrDeliverySuperseded
		case errors.Is(err, ErrDeliveryGenerationAmbiguous):
			return ErrDeliveryGenerationAmbiguous
		default:
			return ErrDeliveryFreshnessUnverified
		}
	}
	return nil
}

// Deliver executes the delivery for approval id. It is safe to call
// concurrently across processes and goroutines for the same id: the store's
// approved→executing claim admits exactly one runner; every other caller
// returns a Skipped result with no side effect.
func (d *DeliveryExecutor) Deliver(ctx context.Context, id string) DeliveryResult {
	if d.Store == nil {
		return DeliveryResult{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("delivery executor has no store"), Skipped: true}
	}

	// Inspect before claiming so a non-delivery / cross-repo approval is
	// refused without consuming the claim.
	pre, err := d.Store.Get(ctx, d.StateDir, id)
	if err != nil {
		return DeliveryResult{Status: state.ApprovalStatusExecutionFailed, Err: err, Skipped: true}
	}
	if pre.Action != state.ApprovalActionDeployProject || pre.Delivery == nil {
		return DeliveryResult{Approval: pre, Status: state.ApprovalStatusExecutionFailed, Err: ErrNotDeliverable, Skipped: true}
	}
	if guardErr := d.repoGuard(pre); guardErr != nil {
		return DeliveryResult{Approval: pre, Status: state.ApprovalStatusExecutionFailed, Err: guardErr, Skipped: true}
	}
	currentDigest := d.Delivery.ApprovalDigest()
	if freshErr := d.checkFreshness(ctx, pre.Delivery); freshErr != nil {
		if errors.Is(freshErr, ErrDeliverySuperseded) || errors.Is(freshErr, ErrDeliveryGenerationAmbiguous) {
			stale, staleErr := d.Store.MarkStale(ctx, d.StateDir, id, d.now(), "delivery generation is no longer current")
			if staleErr != nil {
				return DeliveryResult{Approval: stale, Status: statusOf(stale), Err: ErrDeliveryFreshnessUnverified, Skipped: true}
			}
			return DeliveryResult{Approval: stale, Status: state.ApprovalStatusStale, Summary: "delivery approval is not the current merge generation", Err: freshErr, Skipped: true}
		}
		return DeliveryResult{Approval: pre, Status: pre.Status, Summary: "delivery freshness unavailable; no command ran", Err: ErrDeliveryFreshnessUnverified, Skipped: true}
	}

	// Durable claim: approved → executing. Exactly one winner.
	claimed, err := d.Store.ClaimDeliveryExecuting(ctx, d.StateDir, id, currentDigest, d.now(), d.actor(), "delivery claim before side effect")
	if err != nil {
		// Lost the race, already executing, or already terminal — never a
		// side effect. A restart observing an executing/terminal row lands here.
		return DeliveryResult{
			Approval: claimed,
			Status:   statusOf(claimed),
			Summary:  "delivery not claimable; no command ran",
			Err:      err,
			Skipped:  true,
		}
	}

	payload := claimed.Delivery.Clone()
	// Close the merge-between-read-and-claim race. A newer GitHub merge after
	// the first check terminal-fails this claimed generation before checkout or
	// any project side effect. A merge after this second authoritative read is a
	// distributed event no local transaction can lock; the check is therefore
	// intentionally adjacent to the durable claim and precedes all execution.
	if freshErr := d.checkFreshness(ctx, payload); freshErr != nil {
		if errors.Is(freshErr, ErrDeliveryFreshnessUnverified) {
			return d.releaseForRetry(ctx, id, freshErr, "delivery freshness unavailable; claim released before side effect")
		}
		return d.fail(ctx, id, payload, state.DeliveryFailureStagePrecondition, false, freshErr)
	}
	payload.StartedAt = d.now()
	if strings.TrimSpace(d.Delivery.VerifyCommand) == "" {
		return d.fail(ctx, id, payload, state.DeliveryFailureStagePrecondition, false, ErrDeliveryVerifierRequired)
	}

	// Revision pin: fetch and materialize the approved merge SHA in a clean,
	// detached temporary checkout. LocalPath remains the authoritative source
	// repo but is never the command working directory.
	timeout := d.Delivery.EffectiveTimeout()
	deployCheckout, prepareTimedOut, err := d.prepareDeliveryCheckout(ctx, timeout, payload.MergedSHA)
	if err != nil {
		if deployCheckout != nil {
			payload.ExecutedRevision = deployCheckout.Revision
			cleanupPreparedCheckout(payload, deployCheckout)
		}
		if !errors.Is(err, errDeliveryCheckoutRevisionMismatch) {
			return d.releaseForRetry(ctx, id, ErrDeliveryCheckoutFailed, "delivery checkout unavailable; claim released before side effect")
		}
		return d.fail(ctx, id, payload, state.DeliveryFailureStageCheckout, prepareTimedOut, ErrDeliveryCheckoutFailed)
	}
	payload.ExecutedRevision = deployCheckout.Revision
	deployCleaned := false
	cleanupDeploy := func() {
		if !deployCleaned {
			deployCleaned = true
			cleanupPreparedCheckout(payload, deployCheckout)
		}
	}
	defer cleanupDeploy()
	command := strings.TrimSpace(d.Delivery.Command)
	if _, err := secureDeliveryEntrypoint(deployCheckout.Dir, command); err != nil {
		cleanupDeploy()
		return d.fail(ctx, id, payload, state.DeliveryFailureStagePrecondition, false, ErrDeliveryEntrypointUnsafe)
	}
	// Materialization may involve a remote fetch. Re-check after the immutable
	// entrypoint is ready and immediately before the first target side effect,
	// removing the otherwise-large merge-during-fetch window.
	if freshErr := d.checkFreshness(ctx, payload); freshErr != nil {
		cleanupDeploy()
		if errors.Is(freshErr, ErrDeliveryFreshnessUnverified) {
			return d.releaseForRetry(ctx, id, freshErr, "delivery freshness unavailable; claim released before side effect")
		}
		return d.fail(ctx, id, payload, state.DeliveryFailureStagePrecondition, false, freshErr)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	_, runErr := d.runner().Run(runCtx, deployCheckout.Dir, command)
	runTimedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) || errors.Is(runErr, context.DeadlineExceeded)
	cancel()
	if runErr != nil {
		payload.DeployExitCode = deliveryExitCode(runErr)
		cleanupDeploy()
		return d.fail(ctx, id, payload, state.DeliveryFailureStageDeploy, runTimedOut, ErrDeliveryCommandFailed)
	}
	payload.DeployExitCode = intPointer(0)
	cleanupDeploy()

	// The deploy entrypoint was allowed to mutate its checkout. Materialize a
	// second pristine exact-SHA checkout for verification, so deploy code cannot
	// replace or rewrite the verifier that decides whether it succeeded.
	verifyCheckout, verifyPrepareTimedOut, err := d.prepareDeliveryCheckout(ctx, timeout, payload.MergedSHA)
	if err != nil || filepath.Clean(verifyCheckout.Dir) == filepath.Clean(deployCheckout.Dir) {
		if verifyCheckout != nil {
			cleanupPreparedCheckout(payload, verifyCheckout)
		}
		return d.fail(ctx, id, payload, state.DeliveryFailureStageCheckout, verifyPrepareTimedOut, ErrDeliveryCheckoutFailed)
	}
	verifyCleaned := false
	cleanupVerify := func() {
		if !verifyCleaned {
			verifyCleaned = true
			cleanupPreparedCheckout(payload, verifyCheckout)
		}
	}
	defer cleanupVerify()

	verify := strings.TrimSpace(d.Delivery.VerifyCommand)
	if _, err := secureDeliveryEntrypoint(verifyCheckout.Dir, verify); err != nil {
		cleanupVerify()
		return d.fail(ctx, id, payload, state.DeliveryFailureStagePrecondition, false, ErrDeliveryEntrypointUnsafe)
	}
	vCtx, vCancel := context.WithTimeout(ctx, timeout)
	_, vErr := d.runner().Run(vCtx, verifyCheckout.Dir, verify)
	verifyTimedOut := errors.Is(vCtx.Err(), context.DeadlineExceeded) || errors.Is(vErr, context.DeadlineExceeded)
	vCancel()
	if vErr != nil {
		payload.VerifyExitCode = deliveryExitCode(vErr)
		cleanupVerify()
		return d.fail(ctx, id, payload, state.DeliveryFailureStageVerify, verifyTimedOut, ErrDeliveryVerificationFailed)
	}
	payload.VerifyExitCode = intPointer(0)

	payload.Verified = true
	payload.FinishedAt = d.now()
	cleanupVerify()
	summary := "delivery verified"
	finishCtx, finishCancel := deliveryFinishContext(ctx)
	defer finishCancel()
	final, err := d.Store.FinishDelivery(finishCtx, d.StateDir, id, true, payload, d.now(), d.actor(), summary)
	if err != nil {
		return DeliveryResult{Approval: final, Status: statusOf(final), Err: err}
	}
	return DeliveryResult{Approval: final, Status: state.ApprovalStatusExecuted, Summary: summary}
}

func (d *DeliveryExecutor) prepareDeliveryCheckout(ctx context.Context, timeout time.Duration, revision string) (*PreparedCheckout, bool, error) {
	prepareCtx, prepareCancel := context.WithTimeout(ctx, timeout)
	prepared, err := d.checkout().Prepare(prepareCtx, d.Delivery.LocalPath, revision)
	timedOut := errors.Is(prepareCtx.Err(), context.DeadlineExceeded)
	prepareCancel()
	if err != nil || prepared == nil || strings.TrimSpace(prepared.Dir) == "" {
		if prepared != nil && prepared.Cleanup != nil {
			_ = prepared.Cleanup()
		}
		return nil, timedOut, ErrDeliveryCheckoutFailed
	}
	if !revisionMatches(prepared.Revision, revision) {
		return prepared, timedOut, errDeliveryCheckoutRevisionMismatch
	}
	return prepared, timedOut, nil
}

func (d *DeliveryExecutor) releaseForRetry(ctx context.Context, id string, publicErr error, summary string) DeliveryResult {
	releaseCtx, cancel := deliveryFinishContext(ctx)
	defer cancel()
	released, err := d.Store.ReleaseDeliveryExecuting(releaseCtx, d.StateDir, id, d.now(), d.actor())
	if err != nil {
		return DeliveryResult{Approval: released, Status: statusOf(released), Err: err, Skipped: true}
	}
	return DeliveryResult{Approval: released, Status: state.ApprovalStatusApproved, Summary: summary, Err: publicErr, Skipped: true}
}

func cleanupPreparedCheckout(payload *state.DeliveryPayload, prepared *PreparedCheckout) {
	if payload == nil {
		return
	}
	if prepared == nil || prepared.Cleanup == nil {
		payload.CleanupFailed = true
		return
	}
	if err := prepared.Cleanup(); err != nil {
		payload.CleanupFailed = true
	}
}

// fail records a terminal execution_failed result for a claimed delivery.
func (d *DeliveryExecutor) fail(ctx context.Context, id string, payload *state.DeliveryPayload, stage string, timedOut bool, publicErr error) DeliveryResult {
	payload.Verified = false
	payload.FailureStage = stage
	payload.TimedOut = timedOut
	payload.FinishedAt = d.now()
	summary := "delivery execution failed"
	finishCtx, finishCancel := deliveryFinishContext(ctx)
	defer finishCancel()
	final, err := d.Store.FinishDelivery(finishCtx, d.StateDir, id, false, payload, d.now(), d.actor(), summary)
	if err != nil {
		return DeliveryResult{Approval: final, Status: statusOf(final), Err: err}
	}
	return DeliveryResult{Approval: final, Status: state.ApprovalStatusExecutionFailed, Summary: summary, Err: publicErr}
}

// The command context is expected to be canceled on timeout. Terminal state
// must nevertheless be recorded, otherwise the durable row is stranded in
// executing and can never be safely reconciled or retried.
func deliveryFinishContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
}

func (d *DeliveryExecutor) repoGuard(a *state.Approval) error {
	if a == nil || a.Delivery == nil {
		return errors.New("delivery approval repo binding mismatch")
	}
	cfgRepo, cfgErr := canonicalGitHubRepo(d.Repo)
	stampedRepo, stampedErr := canonicalGitHubRepo(a.Repo)
	payloadRepo, payloadErr := canonicalGitHubRepo(a.Delivery.Repo)
	if cfgErr != nil || stampedErr != nil || payloadErr != nil ||
		!strings.EqualFold(cfgRepo, stampedRepo) || !strings.EqualFold(cfgRepo, payloadRepo) {
		return errors.New("delivery approval repo binding mismatch")
	}
	return nil
}

func statusOf(a *state.Approval) state.ApprovalStatus {
	if a == nil {
		return state.ApprovalStatusExecutionFailed
	}
	return a.Status
}

func deliveryExitCode(err error) *int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return intPointer(exitErr.ExitCode())
	}
	return intPointer(-1)
}

func intPointer(value int) *int { return &value }

// revisionMatches requires exact full-SHA equality. Delivery approvals are
// minted from GitHub's merge_commit_sha; accepting an abbreviated prefix would
// weaken the immutable revision fence the operator approved.
func revisionMatches(head, pinned string) bool {
	head = strings.ToLower(strings.TrimSpace(head))
	pinned = strings.ToLower(strings.TrimSpace(pinned))
	return head != "" && pinned != "" && head == pinned
}

func shortRev(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
