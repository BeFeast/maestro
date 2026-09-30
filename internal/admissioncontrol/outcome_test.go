package admissioncontrol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sealedFixture(request SealRequest) NativeOutcome {
	outcome := NativeOutcome{Binding: request.Binding, RegistrationVersion: request.RegistrationVersion,
		Sealed: true, Outcome: "no_dispatch", NextGenerationAllowed: true, AttemptsDigest: strings.Repeat("a", 64)}
	outcome.SnapshotDigest, _ = outcomeDigest(outcome)
	outcome.EvidenceID = "native-outcome-v1:" + outcome.SnapshotDigest
	return outcome
}

func sealedEnvelope(request SealRequest, outcome NativeOutcome) []byte {
	body, _ := json.Marshal(map[string]any{"version": 1, "id": request.NativeSessionID, "ok": true, "result": outcome})
	return body
}

func TestNativeOutcomePythonGoldenAndOldExpiredRegistration(t *testing.T) {
	body, err := os.ReadFile("testdata/native-outcome.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Request SealRequest   `json:"request"`
		Result  NativeOutcome `json:"result"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNativeOutcome(fixture.Result, fixture.Request); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeNativeOutcome(sealedEnvelope(fixture.Request, fixture.Result), fixture.Request); err != nil {
		t.Fatal(err)
	}
	// The registration is intentionally expired; the installed version is
	// independent from any current policy. Sealing old exposure must work.
	if fixture.Request.ExpiresAt >= time.Now().Unix() {
		t.Fatal("golden must be expired")
	}
}

func TestNativeOutcomeStrictSchemaAndDerivedAuthorization(t *testing.T) {
	request := SealRequest{Binding: requestFixture().Binding, RegistrationVersion: 2}
	for _, change := range []func(*NativeOutcome){
		func(o *NativeOutcome) { o.Binding.Role = "other" },
		func(o *NativeOutcome) { o.RegistrationVersion++ },
		func(o *NativeOutcome) { o.Sealed = false },
		func(o *NativeOutcome) { o.PhysicalAttempts = 1 },
		func(o *NativeOutcome) { o.UnresolvedAttempts = -1 },
		func(o *NativeOutcome) { o.TerminalAttempts = math.MaxInt64; o.UnresolvedAttempts = 1 },
		func(o *NativeOutcome) { o.NextGenerationAllowed = false },
		func(o *NativeOutcome) { o.Outcome = "settled" },
		func(o *NativeOutcome) { code := "outcome_unknown"; o.HoldCode = &code },
		func(o *NativeOutcome) { o.AttemptsDigest = "fake" },
		func(o *NativeOutcome) { o.BoundViolations = 1 },
	} {
		outcome := sealedFixture(request)
		change(&outcome)
		outcome.SnapshotDigest, _ = outcomeDigest(outcome)
		outcome.EvidenceID = "native-outcome-v1:" + outcome.SnapshotDigest
		if _, err := decodeNativeOutcome(sealedEnvelope(request, outcome), request); err == nil {
			t.Fatalf("accepted inconsistent outcome: %+v", outcome)
		}
	}
	valid := sealedEnvelope(request, sealedFixture(request))
	for _, pair := range [][2]string{{`"ok":true`, `"ok":null`}, {`"sealed":true`, `"sealed":null`},
		{`"physical_attempts":0`, `"physical_attempts":null`}, {`"physical_attempts":0`, `"physical_attempts":true`},
		{`"sealed":true`, `"sealed":true,"sealed":true`}, {`"physical_attempts":0`, `"physical_attempts":0.0`},
		{`"outcome":"no_dispatch"`, `"outcome":"no_dispatch","extra":1`}, {`"hold_code":null,`, ``},
		{`"registration_version":2`, `"registration_version":null`}} {
		changed := strings.Replace(string(valid), pair[0], pair[1], 1)
		if changed == string(valid) {
			t.Fatal("fixture substitution missed")
		}
		if _, err := decodeNativeOutcome([]byte(changed), request); err == nil {
			t.Fatalf("accepted: %s", changed)
		}
	}
	for _, kind := range []string{"outcome_unknown", "bound_violation", "attempt_limit_exceeded"} {
		outcome := sealedFixture(request)
		outcome.Outcome = "held"
		outcome.NextGenerationAllowed = false
		outcome.HoldCode = &kind
		outcome.PhysicalAttempts = 1
		outcome.UnresolvedAttempts = 1
		if kind == "bound_violation" {
			outcome.UnresolvedAttempts = 0
			outcome.TerminalAttempts = 1
			outcome.BoundViolations = 1
		}
		if kind == "attempt_limit_exceeded" {
			outcome.PhysicalAttempts = 4097
			outcome.UnresolvedAttempts = 4097
		}
		outcome.SnapshotDigest, _ = outcomeDigest(outcome)
		outcome.EvidenceID = "native-outcome-v1:" + outcome.SnapshotDigest
		if _, err := decodeNativeOutcome(sealedEnvelope(request, outcome), request); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeOutcomeUnixExactRequestAndLostReplyNoAutomaticRetry(t *testing.T) {
	for _, lost := range []bool{false, true} {
		dir, err := os.MkdirTemp("", "seal-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		listener, err := net.Listen("unix", filepath.Join(dir, "c"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		request := SealRequest{Binding: requestFixture().Binding, RegistrationVersion: 1}
		request.ExpiresAt = 1
		done := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			var prefix [4]byte
			if _, err = io.ReadFull(conn, prefix[:]); err != nil {
				done <- err
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(prefix[:]))
			_, err = io.ReadFull(conn, body)
			if err != nil {
				done <- err
				return
			}
			var sent struct {
				Version int
				ID, Op  string
				Args    SealRequest
			}
			if err = json.Unmarshal(body, &sent); err != nil {
				done <- err
				return
			}
			if sent.Version != 1 || sent.ID != request.NativeSessionID || sent.Op != "seal_native" || sent.Args != request {
				done <- io.ErrUnexpectedEOF
				return
			}
			if !lost {
				response := sealedEnvelope(request, sealedFixture(request))
				binary.BigEndian.PutUint32(prefix[:], uint32(len(response)))
				_, err = io.Copy(conn, bytes.NewReader(append(prefix[:], response...)))
			}
			done <- err
		}()
		result, err := (Client{SocketPath: filepath.Join(dir, "c"), AuthorityUID: uint32(os.Geteuid()), Timeout: time.Second}).SealNative(request)
		if lost && err == nil || !lost && (err != nil || !result.NextGenerationAllowed) {
			t.Fatalf("lost=%v outcome=%+v err=%v", lost, result, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
		if conn, err := listener.Accept(); err == nil {
			conn.Close()
			t.Fatal("automatic replay")
		}
	}
}
