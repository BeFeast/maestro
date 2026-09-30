//go:build !linux

package aiexecution

import (
	"context"
	"os/exec"
)

func NativeGitRegistered(string) bool { return false }
func RegisterNativeGit(string) error  { return Held("native_git_platform_unsupported") }
func NativeGitCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", args...)
}
func NativeGitCommand(args ...string) *exec.Cmd {
	return NativeGitCommandContext(context.Background(), args...)
}
