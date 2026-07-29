package copilot_test

import (
	"testing"

	"github.com/ghantakiran/OTEL/copilot"
)

const (
	fetchedTrace = "fe3852be4562dca17922b0b2758ff910"
	otherFetched = "aa11bb22cc33dd44ee55ff6677889900"
	neverFetched = "deadbeefdeadbeefdeadbeefdeadbeef"
)

// withEvidence builds a conversation whose tools returned the given trace IDs.
func withEvidence(ids ...string) *copilot.Conversation {
	c := copilot.NewConversation("Why is checkout-api slow?")
	refs := make([]copilot.TraceRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, copilot.TraceRef{TraceID: id})
	}
	c.AppendToolResult(copilot.ToolResult{ToolUseID: "toolu_01", Evidence: refs})
	return c
}

// THE TRACER BULLET. A summary citing a trace the tool returned is provenance-
// clean, and the ID is reported as cited.
func TestASummarysCitationsAreAllTracesThatWereFetched(t *testing.T) {
	c := withEvidence(fetchedTrace, otherFetched)
	summary := "The slow path is trace " + fetchedTrace + ", with " + otherFetched + " showing the same shape."

	cited, unknown := copilot.Citations(summary, c)

	if len(unknown) != 0 {
		t.Errorf("unknown = %v, want none — both traces were fetched", unknown)
	}
	if len(cited) != 2 {
		t.Fatalf("cited = %v, want both traces", cited)
	}
	if !copilot.Grounded(summary, c) {
		t.Error("a summary citing only fetched traces is not reported as grounded")
	}
}

// THE FAILURE THIS EXISTS TO CATCH. A trace ID that looks right and was never
// returned by any tool is a fabricated citation, and it is reported as unknown.
func TestACitedTraceThatWasNeverFetchedIsFlagged(t *testing.T) {
	c := withEvidence(fetchedTrace)
	summary := "Root cause is visible in " + neverFetched + "."

	cited, unknown := copilot.Citations(summary, c)

	if len(cited) != 0 {
		t.Errorf("cited = %v, want none", cited)
	}
	if len(unknown) != 1 || unknown[0] != neverFetched {
		t.Fatalf("unknown = %v, want the fabricated ID", unknown)
	}
	if copilot.Grounded(summary, c) {
		t.Error("a summary citing a trace nobody fetched is reported as grounded")
	}
}

// A summary that cites NOTHING is not grounded. This is the check most likely to
// be written backwards: "no unknown IDs" is trivially true of a summary with no
// citations at all, which is the exact failure ADR 0009 is about.
func TestASummaryThatCitesNothingIsNotGrounded(t *testing.T) {
	c := withEvidence(fetchedTrace)
	summary := "The service appears to be degraded and I recommend a rollback."

	cited, unknown := copilot.Citations(summary, c)

	if len(cited) != 0 || len(unknown) != 0 {
		t.Fatalf("cited = %v, unknown = %v, want both empty", cited, unknown)
	}
	if copilot.Grounded(summary, c) {
		t.Error("a summary resting on no evidence at all is reported as grounded")
	}
}

// A model writes citations in whatever shape the sentence wants. A pattern that
// assumed one shape would silently miss the others and report a clean summary
// because it found nothing to check.
func TestACitationIsFoundWhateverPunctuationSurroundsIt(t *testing.T) {
	c := withEvidence(fetchedTrace)

	for _, summary := range []string{
		"See " + fetchedTrace + ".",
		"See (" + fetchedTrace + ") for detail.",
		"See `" + fetchedTrace + "`.",
		"trace_id=" + fetchedTrace,
		"[" + fetchedTrace + "]",
		fetchedTrace,
	} {
		cited, unknown := copilot.Citations(summary, c)
		if len(cited) != 1 || len(unknown) != 0 {
			t.Errorf("%q: cited = %v, unknown = %v", summary, cited, unknown)
		}
	}
}

// The same trace cited three times is one citation, not three. A caller comparing
// runs should be comparing findings.
func TestARepeatedCitationIsCountedOnce(t *testing.T) {
	c := withEvidence(fetchedTrace)
	summary := fetchedTrace + " and again " + fetchedTrace + " and once more " + fetchedTrace

	cited, _ := copilot.Citations(summary, c)

	if len(cited) != 1 {
		t.Errorf("cited = %v, want one entry for one trace", cited)
	}
}

// Trace IDs are hex and case is not meaningful. A model that upper-cases one is
// citing the same trace, and reporting it as fabricated would be a false alarm
// on correct behaviour — the kind that gets a check switched off.
func TestACitationMatchesRegardlessOfCase(t *testing.T) {
	c := withEvidence(fetchedTrace)
	summary := "See FE3852BE4562DCA17922B0B2758FF910."

	cited, unknown := copilot.Citations(summary, c)

	if len(unknown) != 0 {
		t.Errorf("an upper-cased ID was reported as fabricated: %v", unknown)
	}
	if len(cited) != 1 {
		t.Errorf("cited = %v, want the trace", cited)
	}
}

// A conversation whose tools returned nothing cannot ground anything. Every ID in
// the summary is unknown — which is the honest reading, not an error.
func TestWithNoEvidenceEveryCitationIsUnknown(t *testing.T) {
	c := copilot.NewConversation("?")
	summary := "See " + fetchedTrace + "."

	cited, unknown := copilot.Citations(summary, c)

	if len(cited) != 0 {
		t.Errorf("cited = %v from a conversation with no evidence", cited)
	}
	if len(unknown) != 1 {
		t.Errorf("unknown = %v, want the uncorroborated ID", unknown)
	}
}
