package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLatestMergedPRGenerations_PreservesSameSecondTieWithoutPRNumberOrdering(t *testing.T) {
	older := "2026-07-13T10:08:59Z"
	tied := "2026-07-13T10:09:00Z"
	pulls := []restPull{
		{Number: 101, MergedAt: &tied, MergeCommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Number: 100, MergedAt: &tied, MergeCommitSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{Number: 999, MergedAt: &older, MergeCommitSHA: "cccccccccccccccccccccccccccccccccccccccc"},
	}
	for i := range pulls {
		pulls[i].Base.Ref = "main"
	}
	featureNewer := "2026-07-13T10:10:00Z"
	feature := restPull{Number: 102, MergedAt: &featureNewer, MergeCommitSHA: "dddddddddddddddddddddddddddddddddddddddd"}
	feature.Base.Ref = "release"
	pulls = append(pulls, feature)

	got, err := latestMergedPRGenerations(pulls, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("latest generations = %d, want both same-second revisions", len(got))
	}
	wantAt := time.Date(2026, 7, 13, 10, 9, 0, 0, time.UTC)
	if !got[0].MergedAt.Equal(wantAt) || !got[1].MergedAt.Equal(wantAt) {
		t.Fatalf("latest timestamps = %v / %v, want %v", got[0].MergedAt, got[1].MergedAt, wantAt)
	}
	seen := map[string]bool{got[0].SHA: true, got[1].SHA: true}
	if !seen[pulls[0].MergeCommitSHA] || !seen[pulls[1].MergeCommitSHA] {
		t.Fatalf("same-second revisions lost: %+v", got)
	}
}

func TestGHAPIWithArgsContext_CancelledFreshnessReadReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := ghAPIWithArgsContext(ctx, "repos/owner/app")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled freshness read err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled freshness read took %s", elapsed)
	}
}

func TestGitHubFreshnessEnvCannotRedirectAuthority(t *testing.T) {
	env := githubFreshnessEnv([]string{
		"GH_TOKEN=github-token",
		"GITHUB_TOKEN=github-token-2",
		"GH_HOST=attacker.invalid",
		"GH_ENTERPRISE_TOKEN=enterprise-secret",
		"LD_PRELOAD=/tmp/hostile.so",
	})
	if !slices.Contains(env, "GH_TOKEN=github-token") || !slices.Contains(env, "GITHUB_TOKEN=github-token-2") {
		t.Fatalf("github.com auth missing from freshness env: %v", env)
	}
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"GH_HOST=", "GH_ENTERPRISE_TOKEN=", "LD_PRELOAD="} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("authority/injection env %q reached freshness process: %v", forbidden, env)
		}
	}
}

func TestTrustedGHExecutableRejectsUserOwnedAllowlistedRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, ok := trustedGHExecutableWithinRoots("gh", []string{root}); ok {
		t.Fatalf("user-owned executable trusted as %q", got)
	}
}

func TestGHAPIWithArgsContext_CancelsHungAppTokenRefreshBeforeCommand(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	_, key := testRSAKeyPEM(t, "pkcs1")
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	appTokenNow = func() time.Time { return now }
	appTokenMu.Lock()
	appTokenSrc = &appTokenSource{
		appID: 1, installationID: 2, privateKey: key,
		token: "expired", expiry: now.Add(-time.Minute),
	}
	appTokenMu.Unlock()
	started := make(chan struct{})
	appTokenHTTPPostContext = func(ctx context.Context, _, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	_, err := ghAPIWithArgsContext(ctx, "repos/owner/app")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung App refresh err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("hung App refresh ignored freshness deadline for %s", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("App token refresh seam was not exercised")
	}
}

func TestLatestMergedPRGenerations_UsesNonMainDefaultAndIgnoresOtherBranches(t *testing.T) {
	trunkAt := "2026-07-13T10:09:00Z"
	releaseAt := "2026-07-13T10:10:00Z"
	trunk := restPull{Number: 7, MergedAt: &trunkAt, MergeCommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	trunk.Base.Ref = "trunk"
	release := restPull{Number: 8, MergedAt: &releaseAt, MergeCommitSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	release.Base.Ref = "release"

	got, err := latestMergedPRGenerations([]restPull{release, trunk}, "trunk")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SHA != trunk.MergeCommitSHA {
		t.Fatalf("latest default-branch generation = %+v, want trunk merge only", got)
	}
}
