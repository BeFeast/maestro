package admissioncontrol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func requestFixture() RegistrationRequest {
	return RegistrationRequest{Binding: Binding{GatewayScope: "fixture-gateway", NativeSessionID: "11111111-1111-4111-8111-111111111111", FleetID: "fleet", ProjectID: "project", RunID: "existing-budget-run", Role: "supervisor", ExpiresAt: time.Now().Unix() + 60}, ExpectedVersion: 2}
}

func responseFixture(r RegistrationRequest) []byte {
	b, _ := json.Marshal(map[string]any{"version": 1, "id": r.NativeSessionID, "ok": true, "result": Acknowledgement{Binding: r.Binding, RegistrationVersion: r.ExpectedVersion}})
	return b
}

func TestRegistrationResponseRejectsAmbiguity(t *testing.T) {
	r := requestFixture()
	valid := responseFixture(r)
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"duplicate", func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"ok":true`, `"ok":true,"ok":true`, 1))
		}},
		{"null_revoked", func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"revoked":false`, `"revoked":null`, 1))
		}},
		{"wrong_id", func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"id":"`+r.NativeSessionID+`"`, `"id":"wrong"`, 1))
		}},
		{"wrong_binding", func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"role":"supervisor"`, `"role":"worker"`, 1))
		}},
		{"future_version", func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"registration_version":2`, `"registration_version":3`, 1))
		}},
		{"extra_field", func(b []byte) []byte { return append([]byte(`{"extra":1,`), b[1:]...) }},
		{"trailing_json", func(b []byte) []byte { return append(b, []byte(`{}`)...) }},
		{"numeric_bool", func(b []byte) []byte { return []byte(strings.Replace(string(b), `"ok":true`, `"ok":1`, 1)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeResponse(tc.change(append([]byte(nil), valid...)), r)
			var hold *Hold
			if !errors.As(err, &hold) || hold.Code != "invalid_response" {
				t.Fatalf("err=%v", err)
			}
		})
	}
	ack, err := decodeResponse(valid, r)
	if err != nil || ack.Binding != r.Binding {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
	_, err = decodeResponse([]byte(strings.Replace(string(valid), `"revoked":false`, `"revoked":true`, 1)), r)
	var hold *Hold
	if !errors.As(err, &hold) || hold.Code != "registration_revoked" {
		t.Fatal(err)
	}
	old := r
	old.ExpiresAt = time.Now().Unix() - 1
	_, err = decodeResponse(responseFixture(old), old)
	if !errors.As(err, &hold) || hold.Code != "registration_expired" {
		t.Fatal(err)
	}
	_, err = decodeResponse([]byte(`{"version":1,"id":null,"ok":false,"hold":{"code":"caller_forbidden"}}`), r)
	if !errors.As(err, &hold) || hold.Code != "caller_forbidden" {
		t.Fatal(err)
	}
}

func TestRegistrationUnixPeerFramingAndNoRetry(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux peer authentication")
	}
	for _, mode := range []string{"success", "wrong_peer", "lost_reply", "oversize", "truncated", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			// Linux sun_path is short; the test name can make t.TempDir too long.
			dir, err := os.MkdirTemp("", "ar-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "control.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			request := requestFixture()
			seen := make(chan []byte, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				var prefix [4]byte
				if _, err = io.ReadFull(conn, prefix[:]); err != nil {
					seen <- nil
					return
				}
				length := binary.BigEndian.Uint32(prefix[:])
				if length > MaxFrame {
					seen <- nil
					return
				}
				body := make([]byte, length)
				if _, err = io.ReadFull(conn, body); err != nil {
					seen <- nil
					return
				}
				seen <- body
				if mode == "lost_reply" {
					return
				}
				if mode == "timeout" {
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				response := responseFixture(request)
				if mode == "oversize" {
					binary.BigEndian.PutUint32(prefix[:], MaxFrame+1)
					_, _ = conn.Write(prefix[:])
					return
				}
				binary.BigEndian.PutUint32(prefix[:], uint32(len(response)))
				_, _ = conn.Write(prefix[:])
				if mode == "truncated" {
					response = response[:len(response)/2]
				}
				_, _ = conn.Write(response)
			}()
			uid := uint32(os.Geteuid())
			if mode == "wrong_peer" {
				uid++
			}
			client := Client{SocketPath: path, AuthorityUID: uid, Timeout: 100 * time.Millisecond}
			ack, err := client.Register(request)
			if mode == "success" {
				if err != nil || ack.Binding != request.Binding {
					t.Fatalf("ack=%+v err=%v", ack, err)
				}
			} else if err == nil {
				t.Fatal("malformed/unknown reply accepted")
			}
			var body []byte
			select {
			case body = <-seen:
			case <-time.After(time.Second):
				t.Fatal("fixture did not finish")
			}
			if mode == "wrong_peer" {
				if len(body) != 0 {
					t.Fatal("sent request to unauthenticated peer")
				}
				return
			}
			var sent map[string]json.RawMessage
			if json.Unmarshal(body, &sent) != nil || len(sent) != 4 || string(sent["op"]) != `"register"` {
				t.Fatalf("wire=%s", body)
			}
			var args RegistrationRequest
			if json.Unmarshal(sent["args"], &args) != nil || args != request {
				t.Fatalf("args=%+v", args)
			}
			_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
			if conn, err := listener.Accept(); err == nil {
				conn.Close()
				t.Fatal("automatic retry")
			}
		})
	}
}
