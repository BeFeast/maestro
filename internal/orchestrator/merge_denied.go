package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// Merge-denied forge actors (#1247).
//
// On a Forgejo project whose forge credential may not merge (for example the
// containment-restricted native actor, merge_denied=true by design), the
// auto-merge loop called the merge API for every green PR on every cycle and
// got the same 405 "User not allowed to merge PR" once a minute, with no
// operator signal. Two narrow rules stop that loop:
//
//  1. When the bound native containment authorization evidence declares the
//     exact credential auto-merge acts with merge_denied=true (for this
//     repository, unexpired), the merge API is not called at all. Missing,
//     unreadable or unbound evidence changes nothing.
//  2. When the forge answers that refusal anyway, the refused head is latched
//     on the session together with a fingerprint of the credential. Auto-merge
//     does not ask again until the head moves or the credential changes; then
//     it makes exactly one fresh attempt. A PR that turns out to be merged
//     already is not latched; it converges as merged.
//
// Either hold parks the PR behind one operator-gate attention item and
// journals one line plus one notification per hold. It never changes the
// session status, the retry budget, LastNotifiedStatus, or any approval, and
// every other merge refusal keeps its existing handling.

const (
	// mergeDeniedGatePrefix names the operator gate a merge-denied PR is
	// parked behind; the suffix is the attested worker login when the
	// evidence names one.
	mergeDeniedGatePrefix = "merge-denied:"
	// mergeDeniedSummary is the operator-facing verdict of both holds.
	mergeDeniedSummary = "merge requires an operator: forge actor may not merge"

	mergeDeniedKeyEvidence = "evidence:"
	mergeDeniedKeyHead     = "head:"
)

// mergeActorDenial names the forge actor the bound containment evidence
// declares merge-denied.
type mergeActorDenial struct {
	Login string
}

// mergeCredentialToken is the forge credential auto-merge acts with on a
// Forgejo project: the same token env github.New resolves for the merge
// client. Empty on GitHub, whose gh CLI owns its own auth.
func (o *Orchestrator) mergeCredentialToken() string {
	if o == nil || o.cfg == nil || !o.cfg.Forge.IsForgejo() {
		return ""
	}
	return strings.TrimSpace(os.Getenv(o.cfg.Forge.EffectiveTokenEnv()))
}

// mergeActorFingerprint is a short, non-secret identifier of the credential
// auto-merge acts with. The forge-refusal latch is keyed on it so rebinding
// the project to another credential releases the latch. Its own domain prefix
// keeps it distinct from the containment pin digest of the same token.
func (o *Orchestrator) mergeActorFingerprint() string {
	token := o.mergeCredentialToken()
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("maestro-merge-actor:v1\x00" + token))
	return hex.EncodeToString(sum[:8])
}

// boundMergeActorDenial reports whether the project's bound native
// containment authorization evidence declares the credential auto-merge acts
// with merge-denied: an unexpired forgejo-authorization bound to its profile,
// for this repository, whose credential digest is the digest of this exact
// token, with merge_denied=true. Anything less — no ai_execution manifest, no
// token, an unreadable or drifted manifest, unverifiable profiles, evidence
// for another credential or repository — is not a denial, and auto-merge
// behaves exactly as it did before.
func (o *Orchestrator) boundMergeActorDenial() (mergeActorDenial, bool) {
	if o == nil || o.cfg == nil || !o.cfg.Forge.IsForgejo() || strings.TrimSpace(o.cfg.AIExecution.ManifestPath) == "" {
		return mergeActorDenial{}, false
	}
	token := o.mergeCredentialToken()
	if token == "" {
		return mergeActorDenial{}, false
	}
	read := o.nativeForgejoAuthorizationsFn
	if read == nil {
		read = aiexecution.NativeForgejoAuthorizations
	}
	report, err := read(o.cfg.AIExecution)
	if err != nil {
		o.noteMergeActorEvidence(fmt.Sprintf("bound containment authorization unavailable (%v)", err))
		return mergeActorDenial{}, false
	}
	credential := aiexecution.NativeForgejoCredentialSHA256(token)
	repo := strings.TrimSpace(o.cfg.Repo)
	for _, authorization := range report.Authorizations {
		if authorization.MergeDenied &&
			authorization.CredentialSHA256 == credential &&
			strings.EqualFold(strings.TrimSpace(authorization.Repository), repo) {
			o.mergeActorEvidenceNote = ""
			return mergeActorDenial{Login: authorization.WorkerLogin}, true
		}
	}
	if len(report.Unverified) > 0 {
		o.noteMergeActorEvidence(fmt.Sprintf("containment profile(s) %s could not be verified", strings.Join(report.Unverified, ", ")))
	} else {
		o.mergeActorEvidenceNote = ""
	}
	return mergeActorDenial{}, false
}

// noteMergeActorEvidence journals an unavailable-evidence condition once per
// distinct message instead of once per cycle. It is informational only.
func (o *Orchestrator) noteMergeActorEvidence(note string) {
	if o.mergeActorEvidenceNote == note {
		return
	}
	o.mergeActorEvidenceNote = note
	log.Printf("[orch] merge actor: %s — auto-merge proceeds unchanged (#1247)", note)
}

// gateMergeDeniedCandidates drops the ready candidates whose merge must not
// reach the forge this cycle: every candidate while the bound evidence
// declares the merge credential merge-denied, and a candidate whose current
// head the forge already refused for this credential. A latch whose head or
// credential changed is released for exactly one fresh attempt. Every other
// candidate passes through untouched.
func (o *Orchestrator) gateMergeDeniedCandidates(ready []mergeCandidate) []mergeCandidate {
	if len(ready) == 0 {
		return ready
	}
	denial, denied := o.boundMergeActorDenial()
	actor := o.mergeActorFingerprint()
	kept := ready[:0]
	for _, candidate := range ready {
		sess := candidate.sess
		if sess == nil {
			kept = append(kept, candidate)
			continue
		}
		if denied {
			o.applyMergeDeniedHold(sess, candidate.pr, denial.Login, mergeDeniedKeyEvidence+actor,
				"the bound containment authorization declares the configured forge credential merge-denied",
				"Auto-merge does not call the forge merge API while that evidence stands.")
			continue
		}
		if latched := strings.TrimSpace(sess.MergeDeniedHeadSHA); latched != "" {
			current := o.mergeCandidateHead(candidate)
			switch {
			case sess.MergeDeniedActor != actor:
				log.Printf("[orch] PR #%d: merge credential changed since the forge refused head %s — releasing the merge-denied latch for one fresh attempt (#1247)",
					candidate.pr.Number, shortHeadSHA(latched))
			case current == "" || strings.EqualFold(current, latched):
				// Same head (or an unresolvable one), same credential: the
				// forge's answer stands. Re-apply the hold silently.
				o.applyMergeDeniedHold(sess, candidate.pr, "", mergeDeniedHeadKey(latched, actor),
					fmt.Sprintf("the forge refused head %s for the configured credential (HTTP 405)", shortHeadSHA(latched)),
					mergeDeniedHeadRelease)
				continue
			default:
				log.Printf("[orch] PR #%d head moved from %s to %s — releasing the merge-denied latch for one fresh attempt (#1247)",
					candidate.pr.Number, shortHeadSHA(latched), shortHeadSHA(current))
			}
		}
		// No merge-denied hold applies (any more); a later one surfaces anew.
		releaseMergeDeniedHold(sess)
		kept = append(kept, candidate)
	}
	return kept
}

const mergeDeniedHeadRelease = "Auto-merge does not retry this merge until the PR head or the forge credential changes."

func mergeDeniedHeadKey(head, actor string) string {
	return mergeDeniedKeyHead + strings.TrimSpace(head) + "@" + actor
}

// mergeCandidateHead is the head a candidate's merge is bound to: the observed
// gate head when the CI rollup carried one, otherwise the PR's current head
// (empty when it cannot be read).
func (o *Orchestrator) mergeCandidateHead(candidate mergeCandidate) string {
	if head := strings.TrimSpace(candidate.headSHA); head != "" {
		return head
	}
	head, err := o.prHeadSHA(candidate.pr.Number)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(head)
}

// latchMergeDeniedHead records the forge's "User not allowed to merge PR"
// answer for head as terminal for (head, merge credential). A PR the forge
// already merged (an operator merged it concurrently) is not latched: the
// existing merged-PR reconciliation converges the session on the next cycle,
// once the PR has left the open-PR snapshot.
func (o *Orchestrator) latchMergeDeniedHead(sess *state.Session, pr github.PR, head string) {
	if sess == nil {
		return
	}
	// Read the merged state fresh: a value cached earlier in this cycle
	// predates the refusal.
	delete(o.cyclePRMerged, pr.Number)
	if merged, err := o.isPRMerged(pr.Number); err == nil && merged {
		log.Printf("[orch] PR #%d is already merged — not latching the merge refusal; converging as merged (#1247)", pr.Number)
		return
	}
	head = strings.TrimSpace(head)
	actor := o.mergeActorFingerprint()
	sess.MergeDeniedHeadSHA = head
	sess.MergeDeniedActor = actor
	o.applyMergeDeniedHold(sess, pr, "", mergeDeniedHeadKey(head, actor),
		fmt.Sprintf("the forge refused the merge at head %s for the configured credential (HTTP 405)", shortHeadSHA(head)),
		mergeDeniedHeadRelease)
}

// applyMergeDeniedHold parks the session behind the merge-denied operator gate
// (the dashboard attention item) and surfaces the hold exactly once per key:
// one journal line and one notification. The green-CI path clears every
// operator gate each cycle, so the gate is re-applied silently on later
// cycles. The session status, retry budget and LastNotifiedStatus are left
// alone.
func (o *Orchestrator) applyMergeDeniedHold(sess *state.Session, pr github.PR, login, key, reason, release string) {
	if sess == nil {
		return
	}
	actor := strings.TrimSpace(login)
	if actor == "" {
		actor = "forge-actor"
	}
	if pr.Number > 0 {
		sess.PRNumber = pr.Number
	}
	sess.OperatorGateName = mergeDeniedGatePrefix + actor
	sess.OperatorGateRequiredAction = fmt.Sprintf("Merge PR #%d with a credential that is allowed to merge, or rebind the project's forge credential to a merge-capable actor. %s",
		pr.Number, release)
	if sess.MergeDeniedSurfaced == key {
		return
	}
	sess.MergeDeniedSurfaced = key
	log.Printf("[orch] PR #%d (%s): %s — %s; auto-merge will not call the forge merge API for it (#1247)",
		pr.Number, sess.Branch, mergeDeniedSummary, reason)
	o.notifier.Sendf("⛔ maestro: PR #%d (%s): %s — %s. Merge it with an operator credential.",
		pr.Number, sess.Branch, mergeDeniedSummary, reason)
}

// releaseMergeDeniedHold drops every piece of merge-denied state from sess:
// the latch, the one-time surface key, and the merge-denied operator gate
// (other gates are left alone).
func releaseMergeDeniedHold(sess *state.Session) {
	if sess == nil {
		return
	}
	if strings.HasPrefix(strings.TrimSpace(sess.OperatorGateName), mergeDeniedGatePrefix) {
		sess.OperatorGateName = ""
		sess.OperatorGateRequiredAction = ""
	}
	sess.MergeDeniedHeadSHA = ""
	sess.MergeDeniedActor = ""
	sess.MergeDeniedSurfaced = ""
}
