package grounding_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/copilot"
	"github.com/ghantakiran/OTEL/copilot/grounding"
)

// stubJudge answers however a test needs, and records what it was asked. What it
// is ASKED matters as much as what it answers: a judge shown the wrong evidence
// gives a confident verdict about the wrong thing.
type stubJudge struct {
	supports bool
	err      error
	asked    []grounding.Claim
	sawTrace []copilot.TraceRef
	sawPath  *copilot.TelemetryPath
}

func (j *stubJudge) Supports(_ context.Context, c grounding.Claim, evidence []copilot.TraceRef, path *copilot.TelemetryPath) (bool, error) {
	j.asked = append(j.asked, c)
	j.sawTrace = evidence
	j.sawPath = path
	return j.supports, j.err
}

// conversationWith builds a conversation whose tool returned the given traces.
func conversationWith(ids ...string) *copilot.Conversation {
	c := copilot.NewConversation("Why is checkout-api slow?")
	refs := make([]copilot.TraceRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, copilot.TraceRef{TraceID: id, RootSpanName: "POST /checkout"})
	}
	c.AppendToolResult(copilot.ToolResult{ToolUseID: "toolu_01", Evidence: refs})
	return c
}

// THE TRACER BULLET. A claim citing a fetched trace, which the judge says the
// trace bears out, is supported.
func TestAClaimTheEvidenceBearsOutIsSupported(t *testing.T) {
	j := &stubJudge{supports: true}
	summary := "Trace " + traceA + " shows a 42ms root span."

	got := grounding.Check(context.Background(), j, summary, conversationWith(traceA))

	if len(got) != 1 {
		t.Fatalf("got %d assessments, want 1", len(got))
	}
	if got[0].Verdict != grounding.Supported {
		t.Errorf("verdict = %q, want supported", got[0].Verdict)
	}
	if len(j.asked) != 1 {
		t.Fatalf("the judge was asked %d times, want 1", len(j.asked))
	}
}

// THE FAILURE THIS SLICE EXISTS FOR. The trace is real and was fetched — so
// provenance passes — but it does not show what the claim says. P1 called this
// grounded. It is not.
func TestARealTraceCitedForSomethingItDoesNotShowIsUnsupported(t *testing.T) {
	j := &stubJudge{supports: false}
	summary := "Trace " + traceA + " proves the database is down."

	got := grounding.Check(context.Background(), j, summary, conversationWith(traceA))

	if got[0].Verdict != grounding.Unsupported {
		t.Errorf("verdict = %q, want unsupported", got[0].Verdict)
	}
	if got[0].Fabricated != nil {
		t.Errorf("Fabricated = %v — the trace was real, it just does not support the claim", got[0].Fabricated)
	}
}

// A claim resting on a trace nobody fetched fails on PROVENANCE, and the judge is
// never asked. There is nothing to judge it against, and asking would invite a
// verdict on evidence that does not exist.
func TestAClaimOnAFabricatedTraceIsRejectedWithoutAskingTheJudge(t *testing.T) {
	j := &stubJudge{supports: true} // would say yes if asked
	summary := "Trace " + traceB + " shows the stall."

	got := grounding.Check(context.Background(), j, summary, conversationWith(traceA))

	if got[0].Verdict != grounding.Unsupported {
		t.Errorf("verdict = %q, want unsupported", got[0].Verdict)
	}
	if len(got[0].Fabricated) != 1 || got[0].Fabricated[0] != traceB {
		t.Errorf("Fabricated = %v, want the invented ID named", got[0].Fabricated)
	}
	if len(j.asked) != 0 {
		t.Error("the judge was asked about evidence that was never fetched")
	}
}

// An uncited sentence is the ungrounded hypothesis ADR 0009 is about. It gets its
// own verdict rather than being lumped in with unsupported — the operator needs
// to tell "cited the wrong thing" from "cited nothing at all".
func TestAnUncitedClaimIsMarkedUncitedRatherThanUnsupported(t *testing.T) {
	j := &stubJudge{supports: true}
	summary := "The service should be rolled back immediately."

	got := grounding.Check(context.Background(), j, summary, conversationWith(traceA))

	if got[0].Verdict != grounding.Uncited {
		t.Errorf("verdict = %q, want uncited", got[0].Verdict)
	}
	if len(j.asked) != 0 {
		t.Error("the judge was asked about a claim with no evidence")
	}
}

// A JUDGE THAT ERRORS MUST NOT PRODUCE A PASS. Failing open here would mean an
// outage in the checker silently turns every claim into a supported one — the
// check would be loudest exactly when it had stopped working.
func TestAJudgeFailureFailsClosed(t *testing.T) {
	j := &stubJudge{supports: true, err: errors.New("upstream exploded")}
	summary := "Trace " + traceA + " shows a stall."

	got := grounding.Check(context.Background(), j, summary, conversationWith(traceA))

	if got[0].Verdict == grounding.Supported {
		t.Fatal("a judge failure produced a supported verdict")
	}
	if got[0].Verdict != grounding.Unchecked {
		t.Errorf("verdict = %q, want unchecked", got[0].Verdict)
	}
}

// The judge is shown ONLY the evidence the claim cites, not everything fetched.
// Handing it the whole conversation would let a claim be "supported" by a trace
// it never mentioned, which is the same fabrication in a different place.
func TestTheJudgeSeesOnlyTheEvidenceTheClaimCites(t *testing.T) {
	j := &stubJudge{supports: true}
	summary := "Trace " + traceA + " is slow."

	grounding.Check(context.Background(), j, summary, conversationWith(traceA, traceB))

	if len(j.sawTrace) != 1 {
		t.Fatalf("the judge saw %d traces, want only the cited one", len(j.sawTrace))
	}
	if j.sawTrace[0].TraceID != traceA {
		t.Errorf("the judge saw %s, want the cited %s", j.sawTrace[0].TraceID, traceA)
	}
}

// Grounded is the whole-summary verdict, and it is strict on purpose: one
// unsupported claim taints the summary. A partially-checked answer read as
// verified is the failure this package exists to prevent.
func TestOneUnsupportedClaimMakesTheWholeSummaryUngrounded(t *testing.T) {
	j := &stubJudge{supports: false}
	summary := "Trace " + traceA + " is slow. Trace " + traceA + " proves nothing."

	got := grounding.Check(context.Background(), j, summary, conversationWith(traceA))

	if grounding.AllSupported(got) {
		t.Error("a summary containing an unsupported claim reports as fully supported")
	}
}

// A summary with no claims at all is not grounded either — the same trap
// copilot.Grounded avoids. "Nothing failed" is not "something passed".
func TestAnEmptySummaryIsNotGrounded(t *testing.T) {
	if grounding.AllSupported(nil) {
		t.Error("an empty assessment list reports as fully supported")
	}
}

// UNSUPPORTED CLAIMS ARE MARKED, NOT SILENTLY DROPPED. Dropping loses information
// an operator may need — including that the Copilot asserted something it could
// not back — and a silent edit is its own failure mode. ADR 0009 permits either;
// marking is the one that leaves the reader able to audit.
func TestRenderMarksUnsupportedClaimsRatherThanRemovingThem(t *testing.T) {
	j := &stubJudge{supports: false}
	summary := "Trace " + traceA + " proves the database is down."

	rendered := grounding.Render(grounding.Check(context.Background(), j, summary, conversationWith(traceA)))

	if !strings.Contains(rendered, "proves the database is down") {
		t.Error("the unsupported claim was removed rather than marked")
	}
	if !strings.Contains(strings.ToLower(rendered), "unsupported") {
		t.Errorf("the claim is not marked as unsupported:\n%s", rendered)
	}
}

// A supported claim renders unchanged. Marking everything would train a reader to
// ignore the markers.
func TestRenderLeavesSupportedClaimsAlone(t *testing.T) {
	j := &stubJudge{supports: true}
	summary := "Trace " + traceA + " shows a 42ms root span."

	rendered := grounding.Render(grounding.Check(context.Background(), j, summary, conversationWith(traceA)))

	if strings.Contains(strings.ToLower(rendered), "unsupported") {
		t.Errorf("a supported claim was marked:\n%s", rendered)
	}
	if strings.TrimSpace(rendered) != summary {
		t.Errorf("a fully supported summary was altered:\n%q", rendered)
	}
}

// THE PATH A CLAIM IS JUDGED AGAINST IS THE ONE FROM ITS OWN TOOL RESULT.
//
// This is the case that makes the pairing worth the code. A queue that was
// filling when the traces were fetched and has since drained is the explanation
// for the incident; judging that claim against the later, healthy reading would
// refute a correct claim with evidence from the wrong moment.
func TestAClaimIsJudgedAgainstThePathFromItsOwnToolResult(t *testing.T) {
	c := copilot.NewConversation("Why did checkout-api go quiet?")
	c.AppendToolResult(copilot.ToolResult{
		ToolUseID: "toolu_01",
		Evidence:  []copilot.TraceRef{{TraceID: traceA, RootSpanName: "POST /checkout"}},
		Path: &copilot.TelemetryPath{
			ConfigVersion: "sha256:while-it-was-dropping",
			PerExporter:   []copilot.ExporterHealth{{Name: "otlp/primary-apm", EnqueueFailed: 512}},
		},
	})
	c.AppendToolResult(copilot.ToolResult{
		ToolUseID: "toolu_02",
		Evidence:  []copilot.TraceRef{{TraceID: traceB, RootSpanName: "POST /checkout"}},
		Path: &copilot.TelemetryPath{
			ConfigVersion: "sha256:after-it-drained",
			PerExporter:   []copilot.ExporterHealth{{Name: "otlp/primary-apm", EnqueueFailed: 0}},
		},
	})

	j := &stubJudge{supports: true}
	grounding.Check(context.Background(), j, "Trace "+traceA+" was dropped on the way.", c)

	if j.sawPath == nil {
		t.Fatal("the judge was shown no telemetry path at all")
	}
	if j.sawPath.ConfigVersion != "sha256:while-it-was-dropping" {
		t.Errorf("the judge saw the path from the wrong tool result: %q", j.sawPath.ConfigVersion)
	}
	if !j.sawPath.Dropping() {
		t.Error("the judge was shown a healthy path for a claim fetched while telemetry was dropping")
	}
}

// A CALLER THAT FORGOT THE JUDGE GETS AN UNVERIFIED SUMMARY, NOT A VERIFIED ONE.
// The same fail-closed rule as a judge error: the dangerous default here is the
// one where nothing ran and everything reads as checked.
func TestAMissingJudgeLeavesClaimsUncheckedRatherThanSupported(t *testing.T) {
	got := grounding.Check(context.Background(), nil, "Trace "+traceA+" is slow.", conversationWith(traceA))

	if got[0].Verdict != grounding.Unchecked {
		t.Errorf("verdict = %q, want unchecked", got[0].Verdict)
	}
	if !errors.Is(got[0].Err, grounding.ErrNoJudge) {
		t.Errorf("Err = %v, want ErrNoJudge", got[0].Err)
	}
	if grounding.AllSupported(got) {
		t.Error("a summary nobody checked reports as fully supported")
	}
}

// A claim resting on one real trace and one invented one is unsupported, and only
// the invented ID is named. Naming the real one too would send an operator to
// check a trace that is fine.
func TestOnlyTheInventedTraceIsNamedWhenAClaimMixesBoth(t *testing.T) {
	j := &stubJudge{supports: true}
	summary := "Both " + traceA + " and " + traceB + " show the stall."

	got := grounding.Check(context.Background(), j, summary, conversationWith(traceA))

	if got[0].Verdict != grounding.Unsupported {
		t.Fatalf("verdict = %q, want unsupported", got[0].Verdict)
	}
	if len(got[0].Fabricated) != 1 || got[0].Fabricated[0] != traceB {
		t.Errorf("Fabricated = %v, want only %s", got[0].Fabricated, traceB)
	}
	if len(j.asked) != 0 {
		t.Error("the judge was asked about a claim resting partly on a trace nobody fetched")
	}
}

// Render marks an uncited claim as uncited rather than unsupported. The operator
// needs to tell "cited the wrong thing" from "cited nothing at all" in the text
// itself, not only in the Assessment.
func TestRenderDistinguishesUncitedFromUnsupported(t *testing.T) {
	j := &stubJudge{supports: false}
	summary := "The service should be rolled back immediately. Trace " + traceA + " proves nothing."

	rendered := grounding.Render(grounding.Check(context.Background(), j, summary, conversationWith(traceA)))

	if !strings.Contains(rendered, "UNCITED") {
		t.Errorf("the uncited claim is not marked as uncited:\n%s", rendered)
	}
	if !strings.Contains(rendered, "UNSUPPORTED") {
		t.Errorf("the unsupported claim is not marked as unsupported:\n%s", rendered)
	}
}

// UnsupportedClaims is what a caller logs or an Eval Harness scores (#20). It
// returns everything that is not Supported — an uncited or unchecked claim has
// not been shown to hold either, and a caller filtering only on Unsupported would
// silently pass those through.
func TestUnsupportedClaimsReturnsEverythingThatDidNotPass(t *testing.T) {
	j := &stubJudge{supports: true}
	summary := "Trace " + traceA + " is slow. Roll the service back now."

	got := grounding.UnsupportedClaims(grounding.Check(context.Background(), j, summary, conversationWith(traceA)))

	if len(got) != 1 {
		t.Fatalf("got %d failing claims, want 1: %+v", len(got), got)
	}
	if got[0].Verdict != grounding.Uncited {
		t.Errorf("verdict = %q, want the uncited claim", got[0].Verdict)
	}
}
