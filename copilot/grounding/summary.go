package grounding

import (
	"context"
	"fmt"

	"github.com/ghantakiran/OTEL/copilot"
)

// Summary is an incident summary that has been through the grounding check.
//
// BOTH TEXTS ARE KEPT, and that is the point of the type. Text is what an
// operator should read; Raw is what the model actually wrote. Discarding Raw
// would make the check unauditable — nobody could see what was marked or ask
// whether the marking was right — and an Eval Harness (#20) scores the model, not
// the marker, so it needs the unedited answer.
type Summary struct {
	// Text is the summary with every failing claim marked in place. This is the
	// artefact.
	Text string
	// Raw is the model's answer, unedited.
	Raw string
	// Assessments is one verdict per claim, in the order the claims appear.
	Assessments []Assessment
	// Conversation is the whole exchange the summary came out of: the questions
	// asked, the evidence returned, the turns taken. Kept so a citation can be
	// followed and a run can be replayed.
	Conversation *copilot.Conversation
}

// Grounded reports whether every claim in the summary was supported.
//
// Strict, and false for a summary that made no claims — see AllSupported. A
// caller gating on this is asking "may I present this as established?", and the
// answer for an unverified or empty summary is no.
func (s *Summary) Grounded() bool {
	if s == nil {
		return false
	}
	return AllSupported(s.Assessments)
}

// Unsupported returns the claims that did not pass, for logging or scoring.
func (s *Summary) Unsupported() []Assessment {
	if s == nil {
		return nil
	}
	return UnsupportedClaims(s.Assessments)
}

// Run drives the Tool Runner loop and grounds what comes out of it.
//
// THIS IS THE WIRING, and it lives here rather than in `copilot` because of the
// import direction. `grounding` imports `copilot`; the reverse would put a
// grounding check — and through the Judge, eventually a model — underneath the
// typed tool surface. So the loop stays unaware that anything checks it, and the
// checking composes on top.
//
// paths may be nil, exactly as in copilot.RunWithPath. j may not: a nil Judge
// produces a summary whose every claim is Unchecked, which is honest but useless,
// and Check records ErrNoJudge against each one rather than pretending.
//
// P1 SCOPE: the only tool is query_traces, so the only evidence a claim can rest
// on is a TraceRef and the TelemetryPath that came with it. query_metrics,
// query_logs, get_contract and get_standards arrive with P2 (#17) and widen what
// "supported" can mean; nothing here needs to change when they do, because a
// Judge is handed evidence rather than going to find it.
func Run(ctx context.Context, m copilot.Model, store copilot.TraceStore, paths copilot.TelemetryPathStore, j Judge, question string) (*Summary, error) {
	c, err := copilot.RunWithPath(ctx, m, store, paths, question)
	if err != nil {
		// The conversation is returned alongside the error rather than dropped.
		// A loop that failed on turn six still fetched evidence on turns one to
		// five, and during an incident that is worth having.
		return &Summary{Conversation: c}, fmt.Errorf("grounding: %w", err)
	}
	return Ground(ctx, j, c), nil
}

// Ground checks a conversation that has already run.
//
// Separate from Run so that a recorded conversation can be re-checked without
// re-running the model: that is how an Eval Harness scores a corpus (#20), and
// how a change to the grounding rules can be evaluated against yesterday's
// incidents rather than only against new ones.
func Ground(ctx context.Context, j Judge, c *copilot.Conversation) *Summary {
	raw := FinalAnswer(c)
	assessments := Check(ctx, j, raw, c)

	return &Summary{
		Text:         Render(assessments),
		Raw:          raw,
		Assessments:  assessments,
		Conversation: c,
	}
}

// FinalAnswer is the model's last assistant turn — the summary itself.
//
// THE LAST TURN WITH TEXT AND NO TOOL CALLS. Not simply the last assistant turn:
// the loop appends an assistant turn for every step, and the ones that requested
// tools carry running commentary ("Looking at that service's traces") rather than
// an answer. Grounding the commentary would mark the Copilot down for narrating
// its own work, and worse, would miss the sentences that actually make claims.
//
// Empty when the loop never produced an answer — a summary that does not exist is
// not a grounded one, and Check on "" yields no claims, which AllSupported
// correctly reports as not grounded.
func FinalAnswer(c *copilot.Conversation) string {
	if c == nil {
		return ""
	}

	turns := c.Turns()
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if t.Role == copilot.RoleAssistant && len(t.Calls) == 0 && t.Text != "" {
			return t.Text
		}
	}
	return ""
}
