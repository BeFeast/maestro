package aiexecution

import "strings"

// The root-owned profile selects one project credential. It is never selected
// from the daemon-global provider environment or mounted in the child.
func readNativeForgejoCredential(p NativeContainmentProfile) (string, error) {
	b, err := readNativeEvidence(p.ForgejoCredential.Path, p.UID, 16<<10)
	if err != nil || !validDigest(p.ForgejoCredential.SHA256) || digest(b) != p.ForgejoCredential.SHA256 {
		return "", Held("containment_forgejo_credential_unverified")
	}
	var credential struct {
		Version    int    `json:"version"`
		Repository string `json:"repository"`
		Token      string `json:"token"`
	}
	if DecodeStrict(b, &credential) != nil || credential.Version != 1 || credential.Repository != p.ForgejoRepository || credential.Token == "" || strings.ContainsAny(credential.Token, " \t\r\n\x00") || !validDigest(p.ForgejoTokenSHA256) || NativeForgejoCredentialSHA256(credential.Token) != p.ForgejoTokenSHA256 {
		return "", Held("containment_forgejo_credential_unverified")
	}
	return credential.Token, nil
}
