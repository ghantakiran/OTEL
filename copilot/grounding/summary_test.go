package grounding_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/copilot"
	"github.com/ghantakiran/OTEL/copilot/grounding"
)

// stubModel replays a fixed script of turns, one per call.
type stubModel struct {
	turns []copilot.Assistant
	err   error
	n     int
}

func (m *stubModel) Next(context.Context, *copilot.Conversation) (copilot.Assistant, error) {
	if m.err != nil {
		return copilot.Assistant{}, m.err
	}
	if m.n >= len(m.turns) {
		return copilot.Assistant{}, errors.New("stubModel: ran out of turns")
	}
	t := m.turns[m.n]
	m.n++
	return t, nil
}

// stubStore returns fixed traces for any query.
type stubStore struct{ refs []copilot.TraceRef }

func (s *stubStore) QueryTraces(context.Context, copilot.TraceQuery) ([]copilot.TraceRef, error) {
	return s.refs, nil
}

// askThenAnswer is the shape of every real run: a turn that calls the tool, then
// a turn that answers.
func askThenAnswer(answer string) *stubModel {
	return &stubModel{turns: []copilot.Assistant{
		{
			Text:  "Looking at that service's traces.",
			Calls: []copilot.ToolUse{{ID: "toolu_01", Name: copilot.QueryTracesTool, Input: []byte(`{"service_name":"checkout-api"}`)}},
		},
		{Text: answer},
	}}
}

// THE TRACER BULLET for the wiring: the loop runs, the answer is checked, and a
// supported summary comes back unmarked and grounded.
func TestAGroundedRunProducesAnUnmarkedSummary(t *testing.T) {
	answer := "Trace " + traceA + " shows a 42ms root span."
	store := &stubStore{refs: []copilot.TraceRef{{TraceID: traceA, RootSpanName: "POST /checkout"}}}

	got, err := grounding.Run(context.Background(), askThenAnswer(answer), store, nil,
		&stubJudge{supports: true}, "Why is checkout-api slow?")

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.Grounded() {
		t.Errorf("a fully supported summary does not report as grounded: %+v", got.Assessments)
	}
	if strings.TrimSpace(got.Text) != answer {
		t.Errorf("a supported summary was altered:\n got %q\nwant %q", got.Text, answer)
	}
	if got.Raw != answer {
		t.Errorf("Raw = %q, want the model's answer verbatim", got.Raw)
	}
}

// THE WIRING THAT MATTERS: an unsupported claim reaches the operator MARKED, and
// the sentence itself survives. A silent edit would leave nobody able to see that
// the Copilot asserted something it could not back.
func TestAnUnsupportedClaimIsMarkedInTheSummaryTheOperatorReads(t *testing.T) {
	answer := "Trace " + traceA + " proves the database is down."
	store := &stubStore{refs: []copilot.TraceRef{{TraceID: traceA, RootSpanName: "POST /checkout"}}}

	got, err := grounding.Run(context.Background(), askThenAnswer(answer), store, nil,
		&stubJudge{supports: false}, "Why is checkout-api slow?")

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Grounded() {
		t.Error("a summary resting on unsupported evidence reports as grounded")
	}
	if !strings.Contains(got.Text, "proves the database is down") {
		t.Errorf("the claim was dropped rather than marked:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "UNSUPPORTED") {
		t.Errorf("the claim was not marked:\n%s", got.Text)
	}
	if got.Raw != answer {
		t.Error("Raw was edited; the unmarked answer must survive for auditing")
	}
	if len(got.Unsupported()) != 1 {
		t.Errorf("Unsupported() returned %d claims, want 1", len(got.Unsupported()))
	}
}

// THE SUMMARY IS THE FINAL ANSWER, NOT THE RUNNING COMMENTARY.
//
// The loop appends an assistant turn per step, and the tool-calling ones narrate.
// Grounding the narration would mark the Copilot down for describing its own work
// and would miss the sentences that actually make claims.
func TestTheCommentaryTurnsAreNotGraded(t *testing.T) {
	answer := "Trace " + traceA + " shows a 42ms root span."
	store := &stubStore{refs: []copilot.TraceRef{{TraceID: traceA, RootSpanName: "POST /checkout"}}}

	got, err := grounding.Run(context.Background(), askThenAnswer(answer), store, nil,
		&stubJudge{supports: true}, "Why is checkout-api slow?")

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got.Assessments) != 1 {
		t.Fatalf("got %d assessments, want 1 — the commentary turn was graded: %+v",
			len(got.Assessments), got.Assessments)
	}
	if strings.Contains(got.Text, "Looking at that service's traces") {
		t.Error("the commentary turn leaked into the summary")
	}
}

// A loop that failed still returns the evidence it gathered. During an incident,
// five turns of fetched traces are worth more than nothing.
func TestAFailedRunStillReturnsTheConversation(t *testing.T) {
	store := &stubStore{refs: []copilot.TraceRef{{TraceID: traceA}}}
	m := &stubModel{err: errors.New("the model could not be reached")}

	got, err := grounding.Run(context.Background(), m, store, nil, &stubJudge{supports: true}, "Why?")

	if err == nil {
		t.Fatal("a failing model produced no error")
	}
	if got == nil || got.Conversation == nil {
		t.Fatal("the conversation was dropped on failure")
	}
	if got.Grounded() {
		t.Error("a failed run reports as grounded")
	}
}

// Ground re-checks a recorded conversation without re-running the model. This is
// how an Eval Harness scores a corpus (#20) and how a change to the rules is
// evaluated against yesterday's incidents.
func TestGroundRechecksARecordedConversationWithoutTheModel(t *testing.T) {
	c := copilot.NewConversation("Why is checkout-api slow?")
	c.AppendAssistant("Looking.", []copilot.ToolUse{{ID: "toolu_01", Name: copilot.QueryTracesTool}})
	c.AppendToolResult(copilot.ToolResult{
		ToolUseID: "toolu_01",
		Evidence:  []copilot.TraceRef{{TraceID: traceA, RootSpanName: "POST /checkout"}},
	})
	c.AppendAssistant("Trace "+traceA+" shows a 42ms root span.", nil)

	got := grounding.Ground(context.Background(), &stubJudge{supports: true}, c)

	if !got.Grounded() {
		t.Errorf("a recorded grounded conversation does not re-check as grounded: %+v", got.Assessments)
	}
}

// A run whose model never answered is not grounded. "Nothing failed" is not
// "something passed", and an empty summary is the emptiest version of that.
func TestARunThatNeverAnsweredIsNotGrounded(t *testing.T) {
	c := copilot.NewConversation("Why is checkout-api slow?")
	c.AppendAssistant("Looking.", []copilot.ToolUse{{ID: "toolu_01", Name: copilot.QueryTracesTool}})

	got := grounding.Ground(context.Background(), &stubJudge{supports: true}, c)

	if got.Grounded() {
		t.Error("a conversation with no answer reports as grounded")
	}
	if got.Raw != "" {
		t.Errorf("Raw = %q, want empty", got.Raw)
	}
}

// A nil Summary is not grounded. The pointer comes back non-nil from Run even on
// failure, but a caller that built one itself should not get a panic or a pass.
func TestANilSummaryIsNotGrounded(t *testing.T) {
	var s *grounding.Summary
	if s.Grounded() {
		t.Error("a nil summary reports as grounded")
	}
	if s.Unsupported() != nil {
		t.Error("a nil summary returned unsupported claims")
	}
}
