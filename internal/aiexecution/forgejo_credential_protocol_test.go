package aiexecution

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Git terminates helper stdin with EOF; a synthetic blank-line-only fixture
// does not exercise the protocol used by a real push.
func TestNativeForgejoCredentialRealGitProtocol(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := `!"` + strings.ReplaceAll(executable, `"`, `\"`) + `" -test.run=^TestNativeForgejoCredentialHelperProcess$ --`
	cmd := exec.Command(git, "-c", "credential.helper=", "-c", "credential.helper="+helper, "-c", "credential.useHttpPath=true", "credential", "fill")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "MAESTRO_CREDENTIAL_HELPER_TEST=1"}
	cmd.Stdin = strings.NewReader("protocol=https\nhost=git.oklabs.uk\npath=BeFeast/hedroom.git\n\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real Git helper exchange: %v: %s", err, output)
	}
	if !bytes.Contains(output, []byte("username=oauth2\n")) || !bytes.Contains(output, []byte("password=synthetic-test-token\n")) {
		t.Fatalf("missing fixture credentials: %s", output)
	}
}

func TestNativeForgejoCredentialHelperProcess(t *testing.T) {
	if os.Getenv("MAESTRO_CREDENTIAL_HELPER_TEST") != "1" {
		return
	}
	err := NativeForgejoCredential(os.Args[len(os.Args)-1], "BeFeast/hedroom", "synthetic-test-token", os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestNativeForgejoCredentialEOFValidation(t *testing.T) {
	valid := "protocol=https\nhost=git.oklabs.uk\npath=BeFeast/hedroom.git"
	for _, ending := range []string{"", "\n", "\n\n"} {
		var out bytes.Buffer
		if err := NativeForgejoCredential("get", "BeFeast/hedroom", "synthetic-test-token", strings.NewReader(valid+ending), &out); err != nil {
			t.Fatalf("valid ending %q rejected: %v", ending, err)
		}
	}
	for _, request := range []string{"", "protocol=https\nhost=git.oklabs.uk", valid + "\nprotocol=https", strings.Replace(valid, "hedroom.git", "other.git", 1), valid + "\nextra=" + strings.Repeat("a", 8192)} {
		var out bytes.Buffer
		if err := NativeForgejoCredential("get", "BeFeast/hedroom", "synthetic-test-token", strings.NewReader(request), &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid request disclosed fixture credentials")
		}
	}
}
