package grounding

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/ghantakiran/OTEL/copilot"
)

// Verdict is what became of one claim.
//
// FOUR OUTCOMES, NOT TWO, and the split is the useful part. "Not supported" hides
// three different failures that an operator responds to differently:
//
//	Supported     the cited evidence was fetched, and a judge read it against the
//	              claim and said it bears the claim out.
//	Unsupported   either the claim cites a trace nobody fetched (see Fabricated),
//	              or the judge read real evidence and said it does not show this.
//	Uncited       the claim rests on no trace at all. The ungrounded hypothesis
//	              ADR 0009 is actually about.
//	Unchecked     the judge could not answer. NOT a pass — see Check.
type Verdict string

const (
	Supported   Verdict = "supported"
	Unsupported Verdict = "unsupported"
	Uncited     Verdict = "uncited"
	Unchecked   Verdict = "unchecked"
)

// Judge decides whether evidence bears out a claim.
//
// AN INTERFACE BECAUSE THE ANSWER NEEDS A READER. Provenance is decidable from
// the conversation with a regexp; support is not. Whether "the payments service
// is the bottleneck" follows from a root span named `POST /checkout` lasting
// 4.2s requires reading the two together, and the only thing on this platform
// that reads is the model. Keeping that behind an interface is what stops this
// package from importing a vendor SDK — the same discipline as copilot.Model.
//
// It also keeps the expensive thing swappable. A judge is a second model call per
// claim; a deployment that wants a cheaper tier for it (#19's split), or an Eval
// Harness that wants a recorded one (#20), replaces this and nothing else.
//
// The contract has one rule: DO NOT RETURN true WHEN UNSURE. A judge that
// defaults to "supported" turns this whole package into an expensive no-op, and
// it does so silently. Return an error instead; Check fails closed on it.
type Judge interface {
	// Supports reports whether evidence bears out the claim.
	//
	// It is given ONLY the evidence the claim cites, never everything fetched —
	// see Check. path is the telemetry-path reading that accompanied that
	// evidence, or nil when none was recorded; nil means "not known", which is
	// not the same as healthy.
	Supports(ctx context.Context, c Claim, evidence []copilot.TraceRef, path *copilot.TelemetryPath) (bool, error)
}

// Assessment is one claim and what became of it.
type Assessment struct {
	// Claim is the sentence, as the model wrote it.
	Claim Claim
	// Verdict is the outcome.
	Verdict Verdict
	// Fabricated names the cited trace IDs that no tool ever returned. Non-empty
	// only on Unsupported, and it is the difference between "cited the wrong
	// trace" and "invented a trace" — the second is a much worse failure and an
	// operator should not have to guess which happened.
	Fabricated []string
	// Err is why a judge could not answer, on Unchecked. Kept so that a failing
	// checker is visible as a failure rather than as a quiet run of unsupported
	// claims.
	Err error
}

// Check assesses every claim in a summary against the evidence the conversation
// actually holds.
//
// THE ORDER OF THE TWO CHECKS IS THE DESIGN. Provenance runs first and can settle
// a claim on its own; the judge is asked only about claims whose every citation
// was really fetched. Asking a judge about a trace that does not exist invites a
// confident verdict on nothing — the model would have only the claim to go on and
// would answer from it, which is the fabrication one step removed.
//
// FAILS CLOSED. A judge error produces Unchecked, never Supported. The opposite
// choice is the dangerous one: an outage in the checker would silently promote
// every claim to verified, and the summary would read as most trustworthy at the
// moment the check stopped running.
func Check(ctx context.Context, j Judge, summary string, c *copilot.Conversation) []Assessment {
	fetched := index(c)

	var out []Assessment
	for _, claim := range Claims(summary) {
		out = append(out, assess(ctx, j, claim, fetched))
	}
	return out
}

// assess settles one claim.
func assess(ctx context.Context, j Judge, claim Claim, fetched map[string]entry) Assessment {
	if claim.Uncited() {
		// The judge is deliberately not asked. There is nothing to show it, and a
		// judge handed a claim and no evidence is being asked to rate a sentence
		// on its plausibility — which is the ungrounded answer this whole layer
		// exists to refuse.
		return Assessment{Claim: claim, Verdict: Uncited}
	}

	evidence, fabricated, path := resolve(claim, fetched)
	if len(fabricated) > 0 {
		return Assessment{Claim: claim, Verdict: Unsupported, Fabricated: fabricated}
	}

	// A judge is only reachable here, and only with evidence that was fetched.
	if j == nil {
		return Assessment{Claim: claim, Verdict: Unchecked, Err: ErrNoJudge}
	}
	supported, err := j.Supports(ctx, claim, evidence, path)
	switch {
	case err != nil:
		return Assessment{Claim: claim, Verdict: Unchecked, Err: err}
	case supported:
		return Assessment{Claim: claim, Verdict: Supported}
	default:
		return Assessment{Claim: claim, Verdict: Unsupported}
	}
}

// ErrNoJudge is recorded against a claim when Check was given no judge. It is an
// Unchecked verdict rather than a panic or a skip: a caller that forgot the judge
// gets a summary marked unverified, which is true, instead of one that quietly
// looks checked.
var ErrNoJudge = errors.New("copilot/grounding: Check was given no Judge; support was never assessed")

// entry is one fetched trace, tagged with which tool result carried it.
//
// The tag exists so a claim is judged against the path reading from ITS OWN tool
// result rather than the newest one in the conversation. Those differ during the
// incident this is for: a queue that was filling when the traces were fetched and
// has since drained is the explanation, and pairing the claim with a later,
// healthier reading would erase it.
type entry struct {
	ref   copilot.TraceRef
	path  *copilot.TelemetryPath
	order int
}

// index maps every fetched trace ID to the evidence and path that carried it.
//
// A trace returned by two tool results keeps the LATER one. Re-querying during an
// incident is normal and the second answer is the fresher fact.
func index(c *copilot.Conversation) map[string]entry {
	out := map[string]entry{}
	if c == nil {
		return out
	}

	order := 0
	for _, turn := range c.Turns() {
		if turn.Role != copilot.RoleToolResult || turn.Result == nil {
			continue
		}
		order++
		for _, ref := range turn.Result.Evidence {
			if ref.TraceID == "" {
				continue
			}
			out[strings.ToLower(ref.TraceID)] = entry{
				ref: ref, path: turn.Result.Path, order: order,
			}
		}
	}
	return out
}

// resolve splits a claim's citations into evidence that exists and IDs that do
// not, and picks the path reading to judge it against.
func resolve(claim Claim, fetched map[string]entry) (evidence []copilot.TraceRef, fabricated []string, path *copilot.TelemetryPath) {
	newest := -1
	for _, id := range claim.CitedTraceIDs {
		got, ok := fetched[id]
		if !ok {
			fabricated = append(fabricated, id)
			continue
		}
		evidence = append(evidence, got.ref)

		// The newest reading among the claim's OWN citations — not the newest in
		// the conversation. See entry.
		if got.order > newest {
			newest, path = got.order, got.path
		}
	}
	sort.Strings(fabricated)
	return evidence, fabricated, path
}

// AllSupported reports whether every claim in a summary was supported.
//
// STRICT, AND EMPTY IS FALSE. One unsupported claim taints the summary: a reader
// who is told "grounded" does not go back and re-read for the sentence that was
// not. And an empty assessment list — a summary that made no claims at all — is
// not grounded either, which is the same trap copilot.Grounded avoids. "Nothing
// failed" is not "something passed".
func AllSupported(as []Assessment) bool {
	if len(as) == 0 {
		return false
	}
	for _, a := range as {
		if a.Verdict != Supported {
			return false
		}
	}
	return true
}

// Unsupported returns the claims that did not survive the check, in order. It is
// what a caller logs, or what an Eval Harness scores (#20).
func UnsupportedClaims(as []Assessment) []Assessment {
	var out []Assessment
	for _, a := range as {
		if a.Verdict != Supported {
			out = append(out, a)
		}
	}
	return out
}

// Markers are what a reader sees against a claim that did not pass.
//
// The text names the FAILURE, not a confidence score. "Low confidence" invites a
// reader to discount the claim a little and carry on; "the cited trace does not
// show this" tells them what went wrong and what to check. During an incident the
// second is actionable and the first is noise.
const (
	markUnsupported = "[UNSUPPORTED: the cited evidence does not bear this out]"
	markUncited     = "[UNCITED: no evidence was offered for this]"
	markUnchecked   = "[UNCHECKED: support could not be assessed]"
)

// Render writes the summary back with every failing claim marked in place.
//
// MARKED, NOT DELETED. ADR 0009 permits suppressing or flagging, and flagging is
// the one that leaves a reader able to audit. Silently dropping a sentence loses
// something an operator may need — including the fact that the Copilot asserted
// something it could not back, which is itself a signal about the incident and
// about the Copilot. A summary that has been edited without saying so is a worse
// artefact than one that admits its weak parts.
//
// A fully supported summary comes back UNCHANGED. Marking everything, or adding a
// banner, would train a reader to skip the markers — and then the marking is
// decoration. The absence of a marker has to mean something.
func Render(as []Assessment) string {
	var b strings.Builder
	for i, a := range as {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(a.Claim.Text)
		if mark := marker(a); mark != "" {
			b.WriteString(" ")
			b.WriteString(mark)
		}
	}
	return b.String()
}

// marker is the note against one claim, or "" for a claim that passed.
func marker(a Assessment) string {
	switch a.Verdict {
	case Supported:
		return ""
	case Uncited:
		return markUncited
	case Unchecked:
		return markUnchecked
	default:
		if len(a.Fabricated) > 0 {
			return "[UNSUPPORTED: cites a trace no tool returned: " +
				strings.Join(a.Fabricated, ", ") + "]"
		}
		return markUnsupported
	}
}
