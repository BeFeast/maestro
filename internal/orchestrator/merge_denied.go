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
// operator signal. Two narrow sources raise a merge-denied hold:
//
//  1. Evidence: the bound native containment authorization declares the exact
//     credential auto-merge acts with merge_denied=true (for this repository,
//     unexpired). The merge API is not called while that evidence stands.
//  2. Forge: the forge answered that refusal anyway. A PR that turns out to be
//     merged already is not held; it converges as merged.
//
// Either source latches the PR head together with a non-secret fingerprint of
// the credential. A latch is released for exactly one fresh attempt when the
// credential changes, or when the head moves while the evidence does not
// (any longer) declare the denial. An evidence-derived latch is additionally
// released when the bound evidence explicitly says merge_denied=false for the
// same credential; a forge-derived latch is not, because the forge itself
// refused that head. Evidence that becomes unreadable, unverifiable or silent
// about the credential releases nothing, so evidence that flaps between
// "denied" and "unavailable" cannot re-open the merge loop.
//
// A hold parks the PR behind one operator-gate attention item. It is surfaced
// (one journal line and one notification) once per PR, head and credential,
// whichever source raised it. It never changes the session status, the retry
// budget, LastNotifiedStatus, or any approval, and every other merge refusal
// keeps its existing handling. Missing or unbound evidence on a PR without a
// latch changes nothing.

const (
	// mergeDeniedGatePrefix names the operator gate a merge-denied PR is
	// parked behind; the suffix is the attested worker login when the
	// evidence names one.
	mergeDeniedGatePrefix = "merge-denied:"
	// mergeDeniedSummary is the operator-facing verdict of every hold.
	mergeDeniedSummary = "merge requires an operator: forge actor may not merge"

	// mergeDeniedSourceEvidence and mergeDeniedSourceForge record what raised
	// a latch (state.Session.MergeDeniedSource).
	mergeDeniedSourceEvidence = "evidence"
	mergeDeniedSourceForge    = "forge"

	mergeDeniedEvidenceRelease = "Auto-merge does not call the forge merge API for this PR until the bound evidence says merge_denied=false for this credential, the forge credential changes, or a new head arrives while no evidence declares the denial."
	mergeDeniedForgeRelease    = "Auto-merge does not retry this merge until the PR head or the forge credential changes."
)

// mergeActorEvidence is what the bound containment authorization evidence
// says about the credential auto-merge acts with.
type mergeActorEvidence int

const (
	// mergeEvidenceUnknown: no bound evidence speaks for this credential —
	// none configured, unreadable, unverifiable, or about another credential
	// or repository.
	mergeEvidenceUnknown mergeActorEvidence = iota
	// mergeEvidenceDenied: bound evidence declares merge_denied=true.
	mergeEvidenceDenied
	// mergeEvidenceAllowed: bound evidence explicitly declares
	// merge_denied=false and no profile went unverified.
	mergeEvidenceAllowed
)

// mergeActorVerdict is the evidence verdict plus the attested worker login.
type mergeActorVerdict struct {
	evidence mergeActorEvidence
	login    string
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
// auto-merge acts with. Latches and the surfaced key are bound to it, so
// rebinding the project to another credential releases a latch and surfaces a
// new hold anew. Its own domain prefix keeps it distinct from the containment
// pin digest of the same token.
func (o *Orchestrator) mergeActorFingerprint() string {
	token := o.mergeCredentialToken()
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("maestro-merge-actor:v1\x00" + token))
	return hex.EncodeToString(sum[:8])
}

// boundMergeActorEvidence reads what the project's bound native containment
// authorization evidence says about the credential auto-merge acts with. Only
// an unexpired forgejo-authorization bound to its profile, for this
// repository, whose credential digest is the digest of this exact token
// counts. merge_denied=true on any such authorization is a denial;
// merge_denied=false is an explicit allowance only when no other profile went
// unverified (an unverified profile could carry the denial). Anything else —
// no ai_execution manifest, no token, an unreadable or drifted manifest,
// evidence for another credential or repository — is unknown.
func (o *Orchestrator) boundMergeActorEvidence() mergeActorVerdict {
	if o == nil || o.cfg == nil || !o.cfg.Forge.IsForgejo() || strings.TrimSpace(o.cfg.AIExecution.ManifestPath) == "" {
		return mergeActorVerdict{}
	}
	token := o.mergeCredentialToken()
	if token == "" {
		return mergeActorVerdict{}
	}
	read := o.nativeForgejoAuthorizationsFn
	if read == nil {
		read = aiexecution.NativeForgejoAuthorizations
	}
	report, err := read(o.cfg.AIExecution)
	if err != nil {
		o.noteMergeActorEvidence(fmt.Sprintf("bound containment authorization unavailable (%v)", err))
		return mergeActorVerdict{}
	}
	credential := aiexecution.NativeForgejoCredentialSHA256(token)
	repo := strings.TrimSpace(o.cfg.Repo)
	var allowed *aiexecution.NativeForgejoAuthorization
	for i := range report.Authorizations {
		authorization := &report.Authorizations[i]
		if authorization.CredentialSHA256 != credential || !strings.EqualFold(strings.TrimSpace(authorization.Repository), repo) {
			continue
		}
		if authorization.MergeDenied {
			o.mergeActorEvidenceNote = ""
			return mergeActorVerdict{evidence: mergeEvidenceDenied, login: authorization.WorkerLogin}
		}
		if allowed == nil {
			allowed = authorization
		}
	}
	if len(report.Unverified) > 0 {
		o.noteMergeActorEvidence(fmt.Sprintf("containment profile(s) %s could not be verified", strings.Join(report.Unverified, ", ")))
		return mergeActorVerdict{}
	}
	o.mergeActorEvidenceNote = ""
	if allowed != nil {
		return mergeActorVerdict{evidence: mergeEvidenceAllowed, login: allowed.WorkerLogin}
	}
	return mergeActorVerdict{}
}

// noteMergeActorEvidence journals an unavailable-evidence condition once per
// distinct message instead of once per cycle. It is informational only.
func (o *Orchestrator) noteMergeActorEvidence(note string) {
	if o.mergeActorEvidenceNote == note {
		return
	}
	o.mergeActorEvidenceNote = note
	log.Printf("[orch] merge actor: %s — no merge-denied hold is raised or released on it (#1247)", note)
}

// gateMergeDeniedCandidates drops the ready candidates whose merge must not
// reach the forge this cycle and passes every other candidate through
// untouched.
func (o *Orchestrator) gateMergeDeniedCandidates(ready []mergeCandidate) []mergeCandidate {
	if len(ready) == 0 {
		return ready
	}
	verdict := o.boundMergeActorEvidence()
	actor := o.mergeActorFingerprint()
	kept := ready[:0]
	for _, candidate := range ready {
		if candidate.sess != nil && o.holdMergeDeniedCandidate(candidate, verdict, actor) {
			continue
		}
		kept = append(kept, candidate)
	}
	return kept
}

// holdMergeDeniedCandidate reports whether candidate stays held this cycle,
// updating its latch: the evidence denial latches the current head; an
// existing latch is re-applied, or released for one fresh attempt when its
// credential changed, its evidence now explicitly allows the merge, or the
// head moved.
func (o *Orchestrator) holdMergeDeniedCandidate(candidate mergeCandidate, verdict mergeActorVerdict, actor string) bool {
	sess, pr := candidate.sess, candidate.pr
	latched := strings.TrimSpace(sess.MergeDeniedHeadSHA)
	hasLatch := latched != "" || sess.MergeDeniedSource != ""

	if verdict.evidence == mergeEvidenceDenied {
		head := o.mergeCandidateHead(candidate)
		sameActor := hasLatch && sess.MergeDeniedActor == actor
		if head == "" && sameActor {
			head = latched
		}
		// A forge refusal already recorded for this head and credential is
		// the stronger record: keep it, so a later merge_denied=false in the
		// evidence does not release a head the forge itself refused.
		forgeRefusedHead := sameActor && sess.MergeDeniedSource == mergeDeniedSourceForge && latched != "" && strings.EqualFold(head, latched)
		if !forgeRefusedHead {
			sess.MergeDeniedHeadSHA = head
			sess.MergeDeniedActor = actor
			sess.MergeDeniedSource = mergeDeniedSourceEvidence
			sess.MergeDeniedLogin = strings.TrimSpace(verdict.login)
		}
		o.applyMergeDeniedHold(sess, pr, head)
		return true
	}

	if !hasLatch {
		// No merge-denied hold applies. The surfaced key stays, so a renewed
		// hold for the same PR, head and credential is not surfaced again.
		clearMergeDeniedLatch(sess)
		return false
	}

	current := o.mergeCandidateHead(candidate)
	switch {
	case sess.MergeDeniedActor != actor:
		log.Printf("[orch] PR #%d: merge credential changed since the merge-denied hold at head %s — releasing it for one fresh attempt (#1247)",
			pr.Number, shortHeadSHA(latched))
	case sess.MergeDeniedSource == mergeDeniedSourceEvidence && verdict.evidence == mergeEvidenceAllowed:
		log.Printf("[orch] PR #%d: the bound containment authorization now declares the configured forge credential merge_denied=false — releasing the evidence-derived merge-denied hold for one fresh attempt (#1247)",
			pr.Number)
	case current != "" && latched != "" && !strings.EqualFold(current, latched):
		log.Printf("[orch] PR #%d head moved from %s to %s — releasing the merge-denied hold for one fresh attempt (#1247)",
			pr.Number, shortHeadSHA(latched), shortHeadSHA(current))
	default:
		// Same head (or an unresolvable one), same credential, and nothing
		// that releases this source: the hold stands. Re-apply it silently.
		head := latched
		if head == "" {
			head = current
			sess.MergeDeniedHeadSHA = current
		}
		o.applyMergeDeniedHold(sess, pr, head)
		return true
	}
	clearMergeDeniedLatch(sess)
	return false
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
	sess.MergeDeniedHeadSHA = head
	sess.MergeDeniedActor = o.mergeActorFingerprint()
	sess.MergeDeniedSource = mergeDeniedSourceForge
	sess.MergeDeniedLogin = ""
	o.applyMergeDeniedHold(sess, pr, head)
}

// mergeDeniedSurfaceKey identifies one surfaced hold: the PR, the credential
// fingerprint and the head, independent of what raised the hold.
func mergeDeniedSurfaceKey(prNumber int, actor, head string) string {
	return fmt.Sprintf("pr:%d@%s#%s", prNumber, actor, strings.ToLower(strings.TrimSpace(head)))
}

// mergeDeniedAlreadySurfaced reports whether surfaced already covers the hold
// for (prNumber, actor, head). An unknown head on either side counts as the
// same head: an unreadable head is no evidence that it moved.
func mergeDeniedAlreadySurfaced(surfaced string, prNumber int, actor, head string) bool {
	prefix := mergeDeniedSurfaceKey(prNumber, actor, "")
	if !strings.HasPrefix(surfaced, prefix) {
		return false
	}
	surfacedHead := strings.TrimPrefix(surfaced, prefix)
	head = strings.TrimSpace(head)
	return surfacedHead == "" || head == "" || strings.EqualFold(surfacedHead, head)
}

// applyMergeDeniedHold parks the session behind the merge-denied operator gate
// (the dashboard attention item) and surfaces the hold once per PR, head and
// credential, whichever source raised it: one journal line and one
// notification. The green-CI path clears every operator gate each cycle, so
// the gate is re-applied silently on later cycles. The session status, retry
// budget and LastNotifiedStatus are left alone.
func (o *Orchestrator) applyMergeDeniedHold(sess *state.Session, pr github.PR, head string) {
	if sess == nil {
		return
	}
	login := strings.TrimSpace(sess.MergeDeniedLogin)
	if login == "" {
		login = "forge-actor"
	}
	reason := fmt.Sprintf("the forge refused the merge at head %s for the configured credential (HTTP 405)", shortHeadSHA(head))
	release := mergeDeniedForgeRelease
	if sess.MergeDeniedSource == mergeDeniedSourceEvidence {
		reason = "the bound containment authorization declares the configured forge credential merge-denied"
		release = mergeDeniedEvidenceRelease
	}
	if pr.Number > 0 {
		sess.PRNumber = pr.Number
	}
	sess.OperatorGateName = mergeDeniedGatePrefix + login
	sess.OperatorGateRequiredAction = fmt.Sprintf("Merge PR #%d with a credential that is allowed to merge, or rebind the project's forge credential to a merge-capable actor. %s",
		pr.Number, release)
	actor := sess.MergeDeniedActor
	if mergeDeniedAlreadySurfaced(sess.MergeDeniedSurfaced, pr.Number, actor, head) {
		if strings.TrimSpace(head) != "" {
			sess.MergeDeniedSurfaced = mergeDeniedSurfaceKey(pr.Number, actor, head)
		}
		return
	}
	sess.MergeDeniedSurfaced = mergeDeniedSurfaceKey(pr.Number, actor, head)
	log.Printf("[orch] PR #%d (%s): %s — %s; auto-merge will not call the forge merge API for it (#1247)",
		pr.Number, sess.Branch, mergeDeniedSummary, reason)
	o.notifier.Sendf("⛔ maestro: PR #%d (%s): %s — %s. Merge it with an operator credential.",
		pr.Number, sess.Branch, mergeDeniedSummary, reason)
}

// clearMergeDeniedLatch drops the latch and the merge-denied operator gate
// (other gates are left alone). The surfaced key is kept: a renewed hold for
// the same PR, head and credential is not surfaced again.
func clearMergeDeniedLatch(sess *state.Session) {
	if sess == nil {
		return
	}
	if strings.HasPrefix(strings.TrimSpace(sess.OperatorGateName), mergeDeniedGatePrefix) {
		sess.OperatorGateName = ""
		sess.OperatorGateRequiredAction = ""
	}
	sess.MergeDeniedHeadSHA = ""
	sess.MergeDeniedActor = ""
	sess.MergeDeniedSource = ""
	sess.MergeDeniedLogin = ""
}

// releaseMergeDeniedHold drops every piece of merge-denied state from sess,
// including the surfaced key. It runs once the PR merged or left the open
// set, when no hold can apply to it any more.
func releaseMergeDeniedHold(sess *state.Session) {
	if sess == nil {
		return
	}
	clearMergeDeniedLatch(sess)
	sess.MergeDeniedSurfaced = ""
}
