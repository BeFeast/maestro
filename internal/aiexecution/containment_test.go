package aiexecution

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNativeEnvelopePreservesExactPromptSuffix(t *testing.T) {
	e := nativeLaunchEnvelope{Version: 1, NativeSessionID: uuid.NewString()}
	frame, err := encodeNativeEnvelope(e)
	if err != nil {
		t.Fatal(err)
	}
	prompt := []byte("prompt\x00\nwith {} and newlines\n")
	in := bytes.NewReader(append(frame, prompt...))
	got, err := readNativeEnvelope(in)
	if err != nil || got.NativeSessionID != e.NativeSessionID {
		t.Fatal(got, err)
	}
	remaining, _ := io.ReadAll(in)
	if !bytes.Equal(remaining, prompt) {
		t.Fatalf("prompt changed %q", remaining)
	}
}

func TestNativeWorkerClassProfileSupportsSuccessiveSlotsWithoutAuxFallback(t *testing.T) {
	worker, exact := FileProof{Path: "/profiles/worker", SHA256: strings.Repeat("a", 64)}, FileProof{Path: "/profiles/exact", SHA256: strings.Repeat("b", 64)}
	m := Manifest{Containment: map[string]FileProof{"worker": worker, "sup-1": exact}}
	for _, slot := range []string{"sup-2", "sup-10000"} {
		pin, err := selectContainmentProfile(m, slot, "implementer")
		if err != nil || pin != worker {
			t.Fatal(slot, pin, err)
		}
	}
	if pin, err := selectContainmentProfile(m, "sup-1", "implementer"); err != nil || pin != exact {
		t.Fatal(pin, err)
	}
	for _, role := range []string{"supervisor", "reviewer", "planner", "router", "repair", "summary", ""} {
		if _, err := selectContainmentProfile(m, role, role); err == nil {
			t.Fatal("auxiliary used worker profile", role)
		}
	}
	for _, role := range []string{"router", "summary", "supervisor", "reviewer"} {
		if _, err := selectContainmentProfile(m, "sup-2", role); err == nil {
			t.Fatal("unsupported role used worker fallback", role)
		}
	}
	delete(m.Containment, "worker")
	if _, err := selectContainmentProfile(m, "sup-2", "implementer"); err == nil {
		t.Fatal("implicit profile fallback")
	}
}

func TestNativeToolsExcludeAgentsAndAuxiliaryTools(t *testing.T) {
	id := uuid.NewString()
	base := []string{"claude", "-p", "--model", "claude-opus-5", "--session-id", id}
	args, err := containedNativeArguments(base, id, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--tools Bash,Read,Edit,Write,Glob,Grep") || strings.Contains(joined, "Agent") {
		t.Fatal(joined)
	}
	for _, role := range []string{"supervisor", "reviewer"} {
		args, err := containedNativeArguments(base, id, role)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(args, "|"), "--tools||--max-turns|1") {
			t.Fatal(args)
		}
	}
	for _, extra := range [][]string{{"--tools", "Agent"}, {"--settings", "/host"}, {"--mcp-config", "unsafe.json"}, {"--add-dir", "/home"}, {"--model", "other"}, {"--resume", "other"}} {
		if _, err := containedNativeArguments(append(append([]string(nil), base...), extra...), id, "implementer"); err == nil {
			t.Fatal("accepted override", extra)
		}
	}
}

func TestNativeAuxiliaryPermissionsDenyPromptAndBypassOverrides(t *testing.T) {
	id := uuid.NewString()
	base := []string{"claude", "-p", "--model", "claude-opus-5", "--session-id", id}
	for _, role := range []string{"supervisor", "reviewer"} {
		for _, extra := range [][]string{nil, {"--permission-mode", "dontAsk", "--permission-prompts", "none"}, {"--permission-mode=dontAsk", "--permission-prompts=none"}} {
			args, err := containedNativeArguments(append(append([]string(nil), base...), extra...), id, role)
			if err != nil || !strings.HasSuffix(strings.Join(args, "|"), "--tools||--max-turns|1|--permission-mode|dontAsk|--permission-prompts|none") {
				t.Fatalf("role=%s args=%v err=%v", role, args, err)
			}
		}
		for _, extra := range [][]string{
			{"--permission-mode", "auto"}, {"--permission-mode", "bypassPermissions"},
			{"--permission-mode=acceptEdits"}, {"--permission-mode", "manual"},
			{"--permission-mode", "plan"}, {"--permission-mode", ""},
			{"--permission-prompts", "host"}, {"--permission-prompts=other"},
			{"--permission-mode"}, {"--permission-prompts"},
			{"--permission-mode", "dontAsk", "--permission-mode=auto"},
			{"--permission-prompts", "none", "--permission-prompts=host"},
			{"--dangerously-skip-permissions"}, {"--tools", "Read"}, {"--tools", "Agent"},
		} {
			if _, err := containedNativeArguments(append(append([]string(nil), base...), extra...), id, role); err == nil {
				t.Fatalf("role=%s accepted override %v", role, extra)
			}
		}
	}
	for _, extra := range [][]string{{"--permission-mode", "dontAsk"}, {"--permission-prompts", "none"}} {
		if _, err := containedNativeArguments(append(append([]string(nil), base...), extra...), id, "implementer"); err == nil {
			t.Fatal("auxiliary-only permission option accepted on worker", extra)
		}
	}
}

func TestNativeEnvironmentDoesNotInheritProviderOrHostCredentials(t *testing.T) {
	p := NativeContainmentProfile{GatewayURL: "http://192.0.2.1:8317", ForgejoRepository: "BeFeast/maestro", ForgejoTokenSHA256: digest([]byte("maestro-native-forgejo:v1\x00scoped"))}
	base := []string{"ANTHROPIC_AUTH_TOKEN=managed", "ANTHROPIC_BASE_URL=" + p.GatewayURL, "ANTHROPIC_API_KEY=unmanaged", "CLAUDE_CODE_OAUTH_TOKEN=unmanaged", "HTTP_PROXY=http://proxy", "SSH_AUTH_SOCK=/run/ssh", "GH_TOKEN=broad", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=bad", "FORGEJO_TOKEN=scoped", "MAESTRO_FORGEJO_REPOSITORY_TOKEN=broad"}
	for _, role := range []string{"implementer", "supervisor", "reviewer"} {
		env, err := containedNativeEnvironment(p, base, role)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(env, "\n")
		if !strings.Contains(joined, "CLAUDE_CODE_MAX_OUTPUT_TOKENS=32000") {
			t.Fatal("native request output ceiling missing", role)
		}
		for _, denied := range []string{"unmanaged", "broad", "HTTP_PROXY", "SSH_AUTH_SOCK", "core.fsmonitor=bad"} {
			if strings.Contains(joined, denied) {
				t.Fatal("environment escaped", denied)
			}
		}
		if role != "implementer" && strings.Contains(joined, "scoped") {
			t.Fatal("aux got repository credentials")
		}
	}
}

func TestNativeForgejoCredentialOnlyAnswersExactRepository(t *testing.T) {
	valid := "protocol=https\nhost=git.oklabs.uk\npath=BeFeast/maestro.git\n\n"
	var out bytes.Buffer
	if err := NativeForgejoCredential("get", "BeFeast/maestro", "secret", strings.NewReader(valid), &out); err != nil || !strings.Contains(out.String(), "password=secret") {
		t.Fatal(out.String(), err)
	}
	for _, request := range []string{strings.ReplaceAll(valid, "https", "http"), strings.ReplaceAll(valid, "git.oklabs.uk", "git.oklabs.uk:443"), strings.ReplaceAll(valid, "maestro.git", "other.git"), strings.ReplaceAll(valid, "maestro.git", "../maestro.git"), "protocol=https\nprotocol=http\nhost=git.oklabs.uk\npath=BeFeast/maestro.git\n"} {
		out.Reset()
		if err := NativeForgejoCredential("get", "BeFeast/maestro", "secret", strings.NewReader(request), &out); err == nil || out.Len() != 0 {
			t.Fatal("leaked credential", request, out.String(), err)
		}
	}
}
