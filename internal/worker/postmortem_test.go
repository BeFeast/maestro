package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExtractPostmortem_FailedCommands(t *testing.T) {
	log := strings.Join([]string{
		"running go test ./...",
		"--- FAIL: TestFoo (0.01s)",
		"    foo_test.go:42: expected 3, got 4",
		"FAIL\tgithub.com/befeast/maestro/internal/foo\t0.02s",
		"internal/bar/bar.go:10:2: undefined: doStuff",
		"exit status 1",
		"some unrelated chatter that is fine",
	}, "\n")

	path := writeTempLog(t, log)
	out := ExtractPostmortem(path, PostmortemTailLines)

	if out == "" {
		t.Fatal("expected a non-empty post-mortem")
	}
	for _, want := range []string{
		"--- FAIL: TestFoo",
		"undefined: doStuff",
		"exit status 1",
		"Errors / failed commands observed",
		"Last actions before the attempt ended",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("post-mortem missing %q:\n%s", want, out)
		}
	}
	// A benign line must not be pulled into the failures section, but it is the
	// last line so it appears under "last actions".
	if strings.Count(out, "unrelated chatter") != 1 {
		t.Errorf("benign line should appear once (as a last action), got:\n%s", out)
	}
}

func TestExtractPostmortem_EditedFiles(t *testing.T) {
	log := strings.Join([]string{
		`{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/internal/a.go"}}`,
		`Wrote internal/b.go`,
		`+++ b/internal/c.go`,
		`diff --git a/internal/d.go b/internal/d.go`,
		"done",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)
	for _, want := range []string{
		"Files the previous attempt touched",
		"/repo/internal/a.go",
		"internal/b.go",
		"internal/c.go",
		"internal/d.go",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("post-mortem missing edited file %q:\n%s", want, out)
		}
	}
}

func TestExtractPostmortem_EmptyLogNoSection(t *testing.T) {
	// Empty file.
	if got := ExtractPostmortem(writeTempLog(t, ""), PostmortemTailLines); got != "" {
		t.Errorf("empty log should yield no post-mortem, got %q", got)
	}
	// Whitespace-only file.
	if got := ExtractPostmortem(writeTempLog(t, "   \n\t\n  "), PostmortemTailLines); got != "" {
		t.Errorf("whitespace-only log should yield no post-mortem, got %q", got)
	}
	// Missing file.
	if got := ExtractPostmortem(filepath.Join(t.TempDir(), "does-not-exist.log"), PostmortemTailLines); got != "" {
		t.Errorf("missing log should yield no post-mortem, got %q", got)
	}
	// Empty path.
	if got := ExtractPostmortem("", PostmortemTailLines); got != "" {
		t.Errorf("empty path should yield no post-mortem, got %q", got)
	}
}

func TestExtractPostmortem_RedactsSecrets(t *testing.T) {
	// Fabricated credentials are assembled via concatenation so secret
	// scanners (agent-lint) don't match them in the diff.
	fakeBearer := "sk-" + "abcdefghijklmnopqrstuvwxyz012345"
	fakeGHToken := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	log := strings.Join([]string{
		"error: request failed",
		"Authorization: Bearer " + fakeBearer,
		"exporting GITHUB_TOKEN=" + fakeGHToken,
		"API_KEY=supersecretvalue123",
		"last line",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, secret := range []string{
		fakeBearer,
		fakeGHToken,
		"supersecretvalue123",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("post-mortem leaked secret %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "[REDACTED") && !strings.Contains(out, "=[REDACTED]") {
		t.Errorf("expected a redaction marker in output:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsMultilineSecrets covers PEM/multiline secret
// bodies: the per-line patterns only mask the assignment line, so the base64
// body and footer lines of a `KEY="-----BEGIN … KEY-----` value survived into
// the prompt and persisted file (#835 review).
func TestExtractPostmortem_RedactsMultilineSecrets(t *testing.T) {
	// Fabricated PEM body and headers assembled at runtime so secret
	// scanners (agent-lint private-key rule) don't match them in the diff.
	body1 := "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB"
	body2 := "AAAAMwAAAAtzc2gtZWQyNTUxOQAAACDddeadbeefdeadbeefdeadbe"
	dashes := strings.Repeat("-", 5)
	beginOpenSSH := dashes + "BEGIN OPENSSH PRIVATE " + "KEY" + dashes
	endOpenSSH := dashes + "END OPENSSH PRIVATE " + "KEY" + dashes
	log := strings.Join([]string{
		"error: ssh auth failed",
		`SSH_PRIVATE_KEY="` + beginOpenSSH,
		body1,
		body2,
		endOpenSSH + `"`,
		"last line",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, secret := range []string{body1, body2, "BEGIN OPENSSH PRIVATE " + "KEY", "END OPENSSH PRIVATE " + "KEY"} {
		if strings.Contains(out, secret) {
			t.Errorf("post-mortem leaked multiline secret content %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected a redaction marker in output:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsBarePEMBlock covers a private key dumped to the
// log outside any KEY=… assignment (e.g. a `cat key.pem` echo).
func TestExtractPostmortem_RedactsBarePEMBlock(t *testing.T) {
	// Headers assembled at runtime to avoid secret-scanner diff matches.
	body := "MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Q"
	dashes := strings.Repeat("-", 5)
	log := strings.Join([]string{
		"error: reading deploy key",
		dashes + "BEGIN RSA PRIVATE " + "KEY" + dashes,
		body,
		dashes + "END RSA PRIVATE " + "KEY" + dashes,
		"build failed",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	if strings.Contains(out, body) {
		t.Errorf("post-mortem leaked bare PEM body %q:\n%s", body, out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected PEM block redaction marker in output:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsTailClippedPEM covers the case where the scanned
// tail starts inside a PEM dump: the -----BEGIN----- banner is beyond the tail
// window, so the complete-block regex cannot see it, but the base64 body and the
// -----END----- footer remain in the last-actions section. The clipped-PEM
// scrubber must still redact the orphaned key material (#835 review comment 1).
func TestExtractPostmortem_RedactsTailClippedPEM(t *testing.T) {
	// A private key so large its BEGIN banner falls outside the last-400-line
	// tail; only later body lines and the END footer survive into the window.
	body := "MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Q"
	dashes := strings.Repeat("-", 5)
	var lines []string
	lines = append(lines, "some early setup output")
	lines = append(lines, dashes+"BEGIN RSA PRIVATE "+"KEY"+dashes) // clipped out of the tail
	for i := 0; i < PostmortemTailLines+50; i++ {
		lines = append(lines, body)
	}
	lines = append(lines, dashes+"END RSA PRIVATE "+"KEY"+dashes)
	lines = append(lines, "build failed")

	out := ExtractPostmortem(writeTempLog(t, strings.Join(lines, "\n")), PostmortemTailLines)

	if strings.Contains(out, body) {
		t.Errorf("tail-clipped PEM body leaked into post-mortem:\n%s", out)
	}
	if strings.Contains(out, "END RSA PRIVATE "+"KEY") {
		t.Errorf("orphaned PEM footer leaked into post-mortem:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected clipped-PEM redaction marker in output:\n%s", out)
	}
	// A benign non-key line near the end must survive.
	if !strings.Contains(out, "build failed") {
		t.Errorf("benign trailing line should be preserved:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsShortInlinePEMTail covers a tail-clipped private
// key fragment embedded in a larger command line (`$ echo <fragment>`). The
// whole-line clipped-PEM pass cannot match an embedded run, and the fragment is
// under the 44-byte full-width width, so redactInlinePEMBody must catch it by its
// base64 '=' padding — the tail signature the >=44-only branch missed (#835
// review — short inline PEM tail leak).
func TestExtractPostmortem_RedactsShortInlinePEMTail(t *testing.T) {
	frag := "Kj34GkxFhD90vcNLYLInFE="
	log := strings.Join([]string{
		"running deploy",
		"$ echo " + frag,
		"exit status 1",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	if strings.Contains(out, frag) {
		t.Errorf("short inline PEM tail leaked into post-mortem:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected PEM redaction marker in output:\n%s", out)
	}
	// The command prose around the fragment must survive.
	if !strings.Contains(out, "$ echo") {
		t.Errorf("command prefix should be preserved:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsShortPaddedInlinePEMTail covers the finding's
// sharpest inline case (#835 review — short inline PEM tail leak): a tail-clipped
// key fragment embedded in a larger command line whose remaining base64 run is
// only 8–15 characters. The >=16-char inline floor skipped the run before
// inlineRunIsKeyMaterial could classify it, so `$ echo AbCdEfGhIjK=` reached both
// the persisted post-mortem and the retry prompt with the raw fragment intact. The
// run carries '=' padding — the unambiguous key-material signal — so the short
// padded alternative in pemInlineBodyRe now hands it to the classifier.
func TestExtractPostmortem_RedactsShortPaddedInlinePEMTail(t *testing.T) {
	frag := "AbCdEfGhIjK=" // 11 base64 chars + '=' padding: under the 16-char floor
	log := strings.Join([]string{
		"running deploy",
		"$ echo " + frag,
		"exit status 1",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	if strings.Contains(out, frag) {
		t.Errorf("short padded inline PEM tail leaked into post-mortem:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected PEM redaction marker in output:\n%s", out)
	}
	// The command prose around the fragment must survive.
	if !strings.Contains(out, "$ echo") {
		t.Errorf("command prefix should be preserved:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsUnpaddedInlinePEMTail covers the residual of the
// same finding (#835 review — "Unpadded Inline PEM Tail Leaks"): a tail-clipped
// key fragment embedded in a command line WITHOUT '=' padding (`$ echo
// Kj34GkxFhD90vcNLYLInFE`) fell under both the 44-byte full-width width and the
// padded-tail branch, so the inline matcher skipped it and the whole-line PEM
// scrubber could not classify it — leaking the fragment into the prompt and the
// persisted post-mortem. A digit-free file path in the same window must still
// survive so the fix does not over-redact ordinary post-mortem content.
func TestExtractPostmortem_RedactsUnpaddedInlinePEMTail(t *testing.T) {
	frag := "Kj34GkxFhD90vcNLYLInFE" // no '=' padding
	path := "internal/worker/postmortem.go"
	log := strings.Join([]string{
		"running deploy",
		"$ echo " + frag,
		"$ vim " + path,
		"exit status 1",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	if strings.Contains(out, frag) {
		t.Errorf("unpadded inline PEM tail leaked into post-mortem:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected PEM redaction marker in output:\n%s", out)
	}
	// The command prose and a digit-free path must survive.
	if !strings.Contains(out, "$ echo") {
		t.Errorf("command prefix should be preserved:\n%s", out)
	}
	if !strings.Contains(out, path) {
		t.Errorf("digit-free path should be preserved (not over-redacted):\n%s", out)
	}
}

// TestExtractPostmortem_RedactsShortAndAllCapsInlinePEMTail closes the residual of
// the same finding (#835 review — "Unpadded Inline PEM Tail Escapes"). Two inline
// fragments still slipped through: a short UNPADDED run below the old >=16 candidate
// floor (`$ echo XyZ12aBc`, never matched by the regex at all) and a 16–43 char
// ALL-UPPERCASE run (`$ echo ABCDEFGHIJKLMNOPQRSTUVWXYZAB`, matched but left weak by
// the classifier's digit requirement). Both reach both sinks as raw key tails. The
// >=8 inline floor now hands the short run to the classifier (whose upper+digit mix
// redacts it) and the all-uppercase branch redacts the long bare-caps blob, while a
// digit-free camelCase identifier and a lowercase hash in the same window survive.
func TestExtractPostmortem_RedactsShortAndAllCapsInlinePEMTail(t *testing.T) {
	shortTail := "XyZ12aBc"                   // 8 chars, unpadded, upper+lower+digit
	allCaps := "ABCDEFGHIJKLMNOPQRSTUVWXYZAB" // 28 chars, all uppercase, no digit
	ident := "handleUserRequest"              // camelCase identifier: case, no digit
	hash := "deadbeefdeadbeef"                // lowercase hex: a plausible digest
	log := strings.Join([]string{
		"running deploy",
		"$ echo " + shortTail,
		"$ echo " + allCaps,
		"$ grep " + ident,
		"$ git show " + hash,
		"exit status 1",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, leak := range []string{shortTail, allCaps} {
		if strings.Contains(out, leak) {
			t.Errorf("inline PEM tail %q leaked into post-mortem:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected PEM redaction marker in output:\n%s", out)
	}
	// Ordinary post-mortem content must survive: a digit-free identifier and a
	// lowercase hash are not over-redacted, and the command prose is kept.
	for _, keep := range []string{ident, hash, "$ echo", "exit status 1"} {
		if !strings.Contains(out, keep) {
			t.Errorf("benign content %q should survive inline redaction:\n%s", keep, out)
		}
	}
}

// TestExtractPostmortem_RedactsSingleClippedPEMBodyLine covers the finding's
// sharpest case: the tail starts inside a PEM dump and only ONE full-width base64
// body line survives into the last-actions window, with both -----BEGIN----- and
// -----END----- banners clipped away. strongCount is 1, so the old >=2 threshold
// left the line unredacted and it leaked into the prompt and persisted file (#835
// review comment 1).
func TestExtractPostmortem_RedactsSingleClippedPEMBodyLine(t *testing.T) {
	body := "MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Q"
	log := strings.Join([]string{
		"$ cat ~/.ssh/id_rsa",
		body, // lone survivor: both PEM banners fell outside the scanned tail
		"$ go build ./...",
		"build failed",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	if strings.Contains(out, body) {
		t.Errorf("single clipped PEM body line leaked into post-mortem:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected clipped-PEM redaction marker in output:\n%s", out)
	}
	if !strings.Contains(out, "build failed") {
		t.Errorf("benign trailing line should be preserved:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsShortClippedPEMTail covers the finding's sharpest
// remaining case (#835 review comment 1 — "Short PEM Tail Leaks"): the scanned
// tail starts inside a PEM dump so the -----BEGIN----- banner, every full-width
// body line, and the -----END----- footer are all clipped away, leaving ONLY the
// short final base64 body line. That fragment is below the 44-char strong width,
// so the old adjacency rule classified it weak and — with no adjacent marker or
// full-width line — left it unredacted, leaking a key fragment into the prompt
// and persisted file.
func TestExtractPostmortem_RedactsShortClippedPEMTail(t *testing.T) {
	// A short final PEM body line: below the strong width and carrying base64
	// characters no hex hash can (uppercase, '=' padding), so it is the tail of a
	// clipped key rather than a git SHA.
	tail := "Kj34GkxFhD90vcNLYLInFE="
	log := strings.Join([]string{
		"$ cat ~/.ssh/id_rsa",
		tail, // lone survivor: BEGIN, body, and END all fell outside the scanned tail
		"$ go build ./...",
		"build failed",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	if strings.Contains(out, tail) {
		t.Errorf("short clipped PEM tail leaked into post-mortem:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected clipped-PEM redaction marker in output:\n%s", out)
	}
	if !strings.Contains(out, "build failed") {
		t.Errorf("benign trailing line should be preserved:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsUppercaseHexShortPEMTail closes the residual of the
// same finding (#835 review comment 1 — "Short PEM Tail Leaks"): after realistic
// (base64) short tails were already anchored, a fragment that is PURE HEX but
// carries uppercase A–F — which a PEM/DER base64 body routinely does — was still
// accepted as a hex hash by the earlier `[0-9a-fA-F]` rule, classified weak, and
// left unredacted. Only a conventional LOWERCASE git SHA keeps the carve-out now.
func TestExtractPostmortem_RedactsUppercaseHexShortPEMTail(t *testing.T) {
	// The lone survivor of a clipped key dump: below the strong width and pure hex,
	// but uppercase — so a base64 body tail, not a lowercase git SHA.
	tail := "DEADBEEFCAFE1234DEADBEEF"
	// A benign lowercase git SHA on its own line must still survive (the carve-out).
	sha := "0123456789abcdef0123456789abcdef01234567"
	log := strings.Join([]string{
		"$ cat ~/.ssh/id_rsa",
		tail, // BEGIN, full-width body, and END all fell outside the scanned tail
		"$ git rev-parse HEAD",
		sha,
		"$ go build ./...",
		"build failed",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	if strings.Contains(out, tail) {
		t.Errorf("uppercase-hex short PEM tail leaked into post-mortem:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected clipped-PEM redaction marker in output:\n%s", out)
	}
	if !strings.Contains(out, sha) {
		t.Errorf("lone lowercase git SHA should be preserved (not over-redacted):\n%s", out)
	}
	if !strings.Contains(out, "build failed") {
		t.Errorf("benign trailing line should be preserved:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsInlinePEMBody covers the finding's inline case: a
// clipped full-width base64 body appears inside a larger log line — command
// output prefixed with `$ echo ` or a `cat id_rsa: ` label — so the anchored
// whole-line classifier in redactClippedPEM never sees it as PEM material and it
// would otherwise reach the prompt and persisted file with the key body intact
// (#835 review).
func TestExtractPostmortem_RedactsInlinePEMBody(t *testing.T) {
	body := "MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Q"
	log := strings.Join([]string{
		"$ echo " + body,
		"cat id_rsa: " + body,
		"build failed",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	if strings.Contains(out, body) {
		t.Errorf("inline PEM body leaked into post-mortem:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected clipped-PEM redaction marker in output:\n%s", out)
	}
	// The command / label prefix around the redacted body must survive so the
	// post-mortem still records what the attempt tried.
	for _, ctx := range []string{"$ echo", "cat id_rsa:", "build failed"} {
		if !strings.Contains(out, ctx) {
			t.Errorf("benign context %q should survive inline redaction:\n%s", ctx, out)
		}
	}
}

// TestRedactInlinePEMBody covers the inline scrubber directly: a full-width body
// embedded in a command line is masked while the prefix is kept, and a short
// base64 token (below the strong width) embedded in a line is left intact.
func TestRedactInlinePEMBody(t *testing.T) {
	body := "MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Q"
	line := "- $ printf %s " + body + " > /tmp/k"
	got := redactInlinePEMBody(line)
	if strings.Contains(got, body) {
		t.Errorf("inline body not redacted:\n%s", got)
	}
	if !strings.Contains(got, pemRedactionMarker) {
		t.Errorf("expected redaction marker:\n%s", got)
	}
	for _, ctx := range []string{"- $ printf", "> /tmp/k"} {
		if !strings.Contains(got, ctx) {
			t.Errorf("context %q should survive:\n%s", ctx, got)
		}
	}

	// A short base64 fragment embedded in a line (below the 44-char strong width)
	// with no '=' padding is ambiguous (may be a hash) and must be left alone.
	frag := "deadbeefdeadbeef"
	benign := "commit " + frag + " touched foo.go"
	if got := redactInlinePEMBody(benign); !strings.Contains(got, frag) {
		t.Errorf("short embedded fragment should not be redacted:\n%s", got)
	}

	// A short base64 tail that DOES carry '=' padding (a clipped final PEM body
	// line embedded in a command) is key material and must be redacted, even
	// though it is under the full-width width (#835 review — short inline PEM tail
	// leak). '=' padding never appears in a git SHA / hex digest, so this does not
	// reclassify the padding-free fragment above.
	tail := "Kj34GkxFhD90vcNLYLInFE="
	if got := redactInlinePEMBody("$ echo " + tail); strings.Contains(got, tail) {
		t.Errorf("padded short inline tail not redacted:\n%s", got)
	} else if !strings.Contains(got, "$ echo") || !strings.Contains(got, pemRedactionMarker) {
		t.Errorf("expected command prose kept and marker present:\n%s", got)
	}

	// A padded tail whose base64 run is under the 16-char candidate floor
	// (`AbCdEfGhIjK=`, 11 chars) is the finding's leak: the >=16 alternative
	// skipped it before inlineRunIsKeyMaterial could classify it, so the raw
	// fragment reached both sinks. The short padded alternative now hands it to the
	// classifier, whose '=' padding signal redacts it (#835 review — short inline
	// PEM tail leak).
	shortTail := "AbCdEfGhIjK="
	if got := redactInlinePEMBody("$ echo " + shortTail); strings.Contains(got, shortTail) {
		t.Errorf("short (<16-char) padded inline tail not redacted:\n%s", got)
	} else if !strings.Contains(got, "$ echo") || !strings.Contains(got, pemRedactionMarker) {
		t.Errorf("expected command prose kept and marker present:\n%s", got)
	}

	// The same short tail with the '=' padding CLIPPED OFF (`$ echo Kj34…InFE`,
	// no '=') is the finding's leak: it is under the full-width width and has no
	// padding, so the padded-only bar skipped it. It is disambiguated from a hash
	// by its digits together with base64-only characters (uppercase, no '='), so it
	// is redacted while the command prose survives (#835 review — unpadded inline
	// PEM tail leak).
	unpadded := "Kj34GkxFhD90vcNLYLInFE"
	if got := redactInlinePEMBody("$ echo " + unpadded); strings.Contains(got, unpadded) {
		t.Errorf("unpadded short inline tail not redacted:\n%s", got)
	} else if !strings.Contains(got, "$ echo") || !strings.Contains(got, pemRedactionMarker) {
		t.Errorf("expected command prose kept and marker present:\n%s", got)
	}

	// A digit-free file path embedded in a command line reaches the same >=8-char
	// candidate matcher but carries no uppercase, so it must survive intact — the
	// unpadded-tail rule must not garble the paths a post-mortem records.
	path := "internal/worker/postmortem"
	if got := redactInlinePEMBody("$ vim " + path + ".go"); !strings.Contains(got, path) {
		t.Errorf("digit-free path should survive inline redaction:\n%s", got)
	}

	// A short (<16-char) UNPADDED run carrying the encoded-byte mix (uppercase +
	// digit) is a clipped key tail the old >=16 floor never matched at all, leaking
	// it into both sinks (#835 review — unpadded inline PEM tail escapes). The >=8
	// floor now hands it to the classifier, which redacts it.
	shortUnpadded := "XyZ12aBc" // 8 chars, upper+lower+digit
	if got := redactInlinePEMBody("$ echo " + shortUnpadded); strings.Contains(got, shortUnpadded) {
		t.Errorf("short unpadded inline tail not redacted:\n%s", got)
	} else if !strings.Contains(got, "$ echo") || !strings.Contains(got, pemRedactionMarker) {
		t.Errorf("expected command prose kept and marker present:\n%s", got)
	}

	// A 16–43 char ALL-UPPERCASE run (no lowercase, no digit) was matched but left
	// weak by the classifier's digit requirement, so a bare-caps clipped key body
	// reached both sinks (#835 review — unpadded inline PEM tail escapes, all-caps
	// case). The all-uppercase branch now redacts it.
	allCaps := "ABCDEFGHIJKLMNOPQRSTUVWXYZAB" // 28 chars, all uppercase
	if got := redactInlinePEMBody("$ echo " + allCaps); strings.Contains(got, allCaps) {
		t.Errorf("all-uppercase inline blob not redacted:\n%s", got)
	} else if !strings.Contains(got, "$ echo") || !strings.Contains(got, pemRedactionMarker) {
		t.Errorf("expected command prose kept and marker present:\n%s", got)
	}

	// A mixed-case, digit-free identifier stays intact: it has lowercase (so the
	// all-caps branch skips it) and no digit (so the upper+digit branch skips it) —
	// the unpadded-tail rules must not garble the identifiers a post-mortem records.
	ident := "handleUserRequest"
	if got := redactInlinePEMBody("$ grep " + ident); !strings.Contains(got, ident) {
		t.Errorf("mixed-case identifier should survive inline redaction:\n%s", got)
	}
}

// TestRedactClippedPEM covers the scrubber directly against assembled-body
// shapes: an orphaned END + body run, a marker-free multi-line body run, and a
// lone base64 token that must NOT be redacted (it may be a hash, not a key).
func TestRedactClippedPEM(t *testing.T) {
	body1 := "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB"
	body2 := "AAAAMwAAAAtzc2gtZWQyNTUxOQAAACDddeadbeefdeadbeefdeadbe"
	dashes := strings.Repeat("-", 5)
	end := dashes + "END OPENSSH PRIVATE " + "KEY" + dashes

	// Orphaned END + body (BEGIN clipped): redact the whole run.
	orphanEnd := "### Last actions before the attempt ended\n- " + body1 + "\n- " + body2 + "\n- " + end
	got := redactClippedPEM(orphanEnd)
	for _, secret := range []string{body1, body2, "END OPENSSH PRIVATE " + "KEY"} {
		if strings.Contains(got, secret) {
			t.Errorf("orphaned-END run leaked %q:\n%s", secret, got)
		}
	}
	if !strings.Contains(got, pemRedactionMarker) {
		t.Errorf("orphaned-END run not redacted:\n%s", got)
	}

	// Marker-free body run (both banners clipped): >=2 full-width lines ⇒ redact.
	bodyOnly := "- " + body1 + "\n- " + body2
	if got := redactClippedPEM(bodyOnly); strings.Contains(got, body1) || strings.Contains(got, body2) {
		t.Errorf("marker-free body run leaked:\n%s", got)
	}

	// A SINGLE full-width body line with both banners clipped and surrounded by
	// non-base64 lines: the tail started inside a PEM dump and only one body line
	// survived. One full-width line must be enough to redact (#835 review comment
	// 1 — the old >=2 threshold leaked this line).
	single := "- $ cat id_rsa\n- " + body1 + "\n- $ go build ./...\n- build failed"
	got = redactClippedPEM(single)
	if strings.Contains(got, body1) {
		t.Errorf("single full-width body line leaked:\n%s", got)
	}
	if !strings.Contains(got, pemRedactionMarker) {
		t.Errorf("single full-width body line not redacted:\n%s", got)
	}
	for _, benign := range []string{"$ cat id_rsa", "$ go build ./...", "build failed"} {
		if !strings.Contains(got, benign) {
			t.Errorf("benign line %q should survive:\n%s", benign, got)
		}
	}

	// A lone short base64 body tail (below the strong width) with both banners and
	// full-width lines clipped away — a tail that landed on the PEM's final line.
	// Each carries a character no LOWERCASE-hex git SHA can (base64-only letters,
	// '=' padding, or uppercase A–F), so it anchors on its own and is redacted even
	// in isolation (#835 review comment 1 — short PEM tail).
	for _, shortTail := range []string{
		"Kj34GkxFhD90vcNLYLInFE=",  // padded final line
		"MIIBOgIBAAJBAKj3GkxF",     // unpadded, but uppercase base64 ⇒ not a hex hash
		"DEADBEEFCAFE1234DEADBEEF", // pure UPPERCASE hex ⇒ a base64 body tail, not a
		//                            lowercase git SHA; the old [0-9a-fA-F] rule left
		//                            it weak and leaked it.
	} {
		single := "- $ cat id_rsa\n- " + shortTail + "\n- $ go build ./...\n- build failed"
		got := redactClippedPEM(single)
		if strings.Contains(got, shortTail) {
			t.Errorf("short clipped PEM body tail %q leaked:\n%s", shortTail, got)
		}
		if !strings.Contains(got, pemRedactionMarker) {
			t.Errorf("short clipped PEM body tail %q not redacted:\n%s", shortTail, got)
		}
		for _, benign := range []string{"$ cat id_rsa", "$ go build ./...", "build failed"} {
			if !strings.Contains(got, benign) {
				t.Errorf("benign line %q should survive:\n%s", benign, got)
			}
		}
	}

	// A lone base64-looking token (e.g. a 40-char git SHA, or a single short blob)
	// with no PEM context must be left intact: a LOWERCASE-hex token is a plausible
	// hash/digest, not key material.
	sha := "0123456789abcdef0123456789abcdef01234567"
	loneBlob := "- edited internal/foo.go\n- " + sha + "\n- build failed"
	if got := redactClippedPEM(loneBlob); !strings.Contains(got, sha) {
		t.Errorf("lone base64 token should not be redacted:\n%s", got)
	}

	// Two adjacent short fragments with no full-width line and no marker must also
	// survive: without a key-material anchor they never enter a redaction run.
	frag1, frag2 := "0123456789abcdef", "fedcba9876543210"
	weakPair := "- " + frag1 + "\n- " + frag2
	if got := redactClippedPEM(weakPair); !strings.Contains(got, frag1) || !strings.Contains(got, frag2) {
		t.Errorf("marker-free short-fragment pair should not be redacted:\n%s", got)
	}
}

func TestCapPostmortem_Enforced(t *testing.T) {
	// A body far larger than the cap.
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("- this is a reasonably long line of post-mortem content\n")
	}
	big := sb.String()
	if len(big) <= PostmortemPromptCapBytes {
		t.Fatalf("test setup: body should exceed cap, got %d", len(big))
	}

	capped := CapPostmortem(big, PostmortemPromptCapBytes)
	// The marker is reserved out of the budget: content plus marker must never
	// exceed capBytes (#835 review: marker previously overshot the cap).
	if len(capped) > PostmortemPromptCapBytes {
		t.Errorf("capped output exceeds cap: %d > %d bytes", len(capped), PostmortemPromptCapBytes)
	}
	if !strings.Contains(capped, "truncated") {
		t.Errorf("expected truncation marker in capped output")
	}

	// A small body is returned unchanged.
	small := "- short\n- body"
	if got := CapPostmortem(small, PostmortemPromptCapBytes); got != small {
		t.Errorf("small body should be unchanged, got %q", got)
	}
	// capBytes <= 0 disables the cap.
	if got := CapPostmortem(big, 0); got != big {
		t.Errorf("capBytes<=0 should disable cap")
	}
}

// TestCapPostmortem_MarkerWithinCap exercises a range of caps — including ones
// smaller than the truncation marker itself — and asserts the result stays
// within budget and remains valid UTF-8 (#835 review: marker overshoot).
func TestCapPostmortem_MarkerWithinCap(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("- a reasonably long line of post-mortem content ☃\n")
	}
	big := sb.String()

	for _, capBytes := range []int{10, 40, len(postmortemTruncationMarker), 100, 512, PostmortemPromptCapBytes} {
		got := CapPostmortem(big, capBytes)
		if len(got) > capBytes {
			t.Errorf("cap=%d: output %d bytes exceeds cap", capBytes, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("cap=%d: output is not valid UTF-8: %q", capBytes, got)
		}
	}
}

// TestCapPostmortem_MultibyteBoundary ensures a cap landing mid-rune does not
// split a multibyte character or overshoot the byte budget — across both the
// marker-free small-cap branch and the main branch (cap larger than the marker,
// no newline in the budget window, where an after-slice sanitize could
// previously expand a split rune and push content+marker back over the cap).
func TestCapPostmortem_MultibyteBoundary(t *testing.T) {
	s := strings.Repeat("☃", 200) // 3 bytes each, no newlines
	m := len(postmortemTruncationMarker)
	caps := []int{1, 2, 3, 5, 20, m - 1, m, m + 1, m + 2, m + 5, m + 10, m + 100}
	for _, capBytes := range caps {
		got := CapPostmortem(s, capBytes)
		if len(got) > capBytes {
			t.Fatalf("cap=%d: output %d bytes exceeds cap", capBytes, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("cap=%d: output not valid UTF-8: %q", capBytes, got)
		}
	}
}

func TestExtractPostmortem_TailBounded(t *testing.T) {
	// A failure line far above the tail window must be dropped; a recent one kept.
	var lines []string
	lines = append(lines, "FAIL\tancient/package\t0.01s") // line 0, way above the tail
	for i := 0; i < PostmortemTailLines+50; i++ {
		lines = append(lines, "noise line filler")
	}
	lines = append(lines, "error: recent failure near the end")

	out := ExtractPostmortem(writeTempLog(t, strings.Join(lines, "\n")), PostmortemTailLines)
	if strings.Contains(out, "ancient/package") {
		t.Errorf("failure outside the tail window should be dropped:\n%s", out)
	}
	if !strings.Contains(out, "recent failure near the end") {
		t.Errorf("recent failure should be captured:\n%s", out)
	}
}

// #835 review comment 1: respawns append to the same slot.log, so the tail can
// carry an older attempt's output. The extractor isolates the region after the
// last worker-worktree banner (the per-(re)spawn marker) so the prior attempt's
// failures/files are not mislabeled as the current one — and the banner's host
// path never leaks into the post-mortem.
func TestExtractPostmortem_IsolatesCurrentAttempt(t *testing.T) {
	log := strings.Join([]string{
		"[maestro] worker worktree: /home/wt/slot-1",
		"--- FAIL: TestOld (0.00s)",
		"Wrote internal/old.go",
		"exit status 1",
		"[maestro] worker worktree: /home/wt/slot-1", // attempt 2 begins here
		"running go test ./...",
		"--- FAIL: TestNew (0.00s)",
		"Wrote internal/new.go",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, stale := range []string{"TestOld", "internal/old.go"} {
		if strings.Contains(out, stale) {
			t.Errorf("prior attempt content %q should be isolated out:\n%s", stale, out)
		}
	}
	for _, want := range []string{"TestNew", "internal/new.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("current attempt content %q missing:\n%s", want, out)
		}
	}
	// The worker-worktree banner is a host path and must not reach the prompt.
	if strings.Contains(out, "worker worktree") {
		t.Errorf("worktree banner should not appear in the post-mortem:\n%s", out)
	}
}

// #835 review comment 2: the codex stream-splitter renders file changes as
// "[codex] <kind> <path>", which the earlier patterns ignored, so the edited
// files were dropped for stream-split codex logs. Command/exit lines under the
// same tag must not be misread as files.
func TestExtractPostmortem_CodexRenderedFileChange(t *testing.T) {
	log := strings.Join([]string{
		"[codex] $ /bin/bash -lc go test ./...",
		"--- FAIL: TestFoo (0.00s)",
		"[codex] exit=1",
		"[codex] modified internal/codexedit.go",
		"[codex] add newpkg/newfile.go",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, want := range []string{"internal/codexedit.go", "newpkg/newfile.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("codex-rendered edit %q not captured:\n%s", want, out)
		}
	}
	// "[codex] $ …" (a command, not a file change) must not leak into the files
	// section — it legitimately appears under "last actions".
	if files := postmortemSection(out, "Files the previous attempt touched"); strings.Contains(files, "/bin/bash") {
		t.Errorf("codex command token misparsed as a file:\n%s", files)
	}
	// A rendered non-zero codex exit is a failure signal.
	if !strings.Contains(out, "[codex] exit=1") {
		t.Errorf("codex non-zero exit should be flagged as a failure:\n%s", out)
	}
}

// #835 review comment 2: the claude stream-splitter renders tool_use without
// its file_path input, so the edited files live only in the raw .jsonl side
// channel. The extractor reads that side channel to recover them.
func TestExtractPostmortem_ClaudeJSONLSideChannel(t *testing.T) {
	// slot.log — the rendered stream — drops file_path from tool_use.
	logContent := strings.Join([]string{
		"[claude] model: claude-opus-4",
		"Let me edit the file.",
		"[tool_use: Edit]",
		"--- FAIL: TestBar (0.00s)",
		"[tool_use: Write]",
	}, "\n")
	// slot.jsonl — the raw NDJSON side channel — keeps file_path.
	jsonlContent := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-opus-4"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/edited.go"}}]}}`,
		`{"type":"user","message":{"content":"tool result string, not an array"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"internal/written.go"}}]}}`,
	}, "\n")

	logPath := writeTempLogAndJSONL(t, logContent, jsonlContent)
	out := ExtractPostmortem(logPath, PostmortemTailLines)

	for _, want := range []string{"Files the previous attempt touched", "internal/edited.go", "internal/written.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("claude jsonl side-channel edit %q not captured:\n%s", want, out)
		}
	}
}

// The .jsonl side channel also accumulates across respawns; its per-attempt
// boundary is the backend's session-start frame (claude system/init here), so a
// prior attempt's edits must not surface for the just-failed attempt.
func TestExtractPostmortem_JSONLIsolatesCurrentAttempt(t *testing.T) {
	logContent := strings.Join([]string{
		"[maestro] worker worktree: /home/wt/slot-1",
		"[tool_use: Edit]",
	}, "\n")
	jsonlContent := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"m"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/attempt1.go"}}]}}`,
		`{"type":"system","subtype":"init","model":"m"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/attempt2.go"}}]}}`,
	}, "\n")

	logPath := writeTempLogAndJSONL(t, logContent, jsonlContent)
	out := ExtractPostmortem(logPath, PostmortemTailLines)

	if strings.Contains(out, "internal/attempt1.go") {
		t.Errorf("prior attempt's jsonl edit should be isolated out:\n%s", out)
	}
	if !strings.Contains(out, "internal/attempt2.go") {
		t.Errorf("current attempt's jsonl edit missing:\n%s", out)
	}
}

// postmortemSection returns the body of the "### <heading>" section of a
// post-mortem, up to the next "### " heading or the end of the text.
func postmortemSection(out, heading string) string {
	marker := "### " + heading
	idx := strings.Index(out, marker)
	if idx < 0 {
		return ""
	}
	rest := out[idx+len(marker):]
	if next := strings.Index(rest, "\n### "); next >= 0 {
		return rest[:next]
	}
	return rest
}

func writeTempLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp log: %v", err)
	}
	return path
}

// writeTempLogAndJSONL writes slot.log and its paired slot.jsonl side channel
// into the same directory so JSONLPathForLog resolves the pair.
func writeTempLogAndJSONL(t *testing.T, logContent, jsonlContent string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "slot.log")
	if err := os.WriteFile(logPath, []byte(logContent), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if err := os.WriteFile(JSONLPathForLog(logPath), []byte(jsonlContent), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return logPath
}
