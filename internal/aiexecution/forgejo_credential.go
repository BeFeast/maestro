package aiexecution

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// NativeForgejoCredential is a read-only Git credential helper. The server's
// repository-scoped token and branch policy must be proved at provisioning;
// a client-side path restriction is not that ACL.
func NativeForgejoCredential(operation, repo, token string, input io.Reader, output io.Writer) error {
	if operation != "get" {
		return nil
	}
	if repo == "" || token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return Held("containment_forgejo_credential_unavailable")
	}
	fields := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(input, 8193))
	scanner.Buffer(make([]byte, 1024), 8193)
	terminated, bytesRead := false, 0
	for scanner.Scan() {
		line := scanner.Text()
		bytesRead += len(line) + 1
		if bytesRead > 8192 {
			return Held("containment_git_credential_request_invalid")
		}
		if line == "" {
			terminated = true
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || len(line) > 4096 {
			return Held("containment_git_credential_request_invalid")
		}
		if _, exists := fields[k]; exists {
			return Held("containment_git_credential_request_invalid")
		}
		fields[k] = v
	}
	if scanner.Err() != nil || !terminated {
		return Held("containment_git_credential_request_invalid")
	}
	if fields["protocol"] != "https" || fields["host"] != "git.oklabs.uk" || fields["path"] != repo+".git" || fields["username"] != "" && fields["username"] != "oauth2" {
		return Held("containment_git_credential_destination_denied")
	}
	_, err := fmt.Fprintf(output, "username=oauth2\npassword=%s\n\n", token)
	return err
}
