package grounding_test

import (
	"testing"

	"github.com/ghantakiran/OTEL/copilot/grounding"
)

const (
	traceA = "fe3852be4562dca17922b0b2758ff910"
	traceB = "aa11bb22cc33dd44ee55ff6677889900"
)

// THE TRACER BULLET for grounding. A summary is not one claim — it is several,
// and they are not equally supported. Checking the summary as a whole would give
// one verdict for a paragraph in which two sentences are solid and a third is
// invented, and the operator would have no way to tell which.
func TestASummaryBreaksIntoOneClaimPerSentence(t *testing.T) {
	summary := "Trace " + traceA + " shows a 42ms root span. " +
		"The queue for otlp/cold-archive is holding, per " + traceB + "."

	claims := grounding.Claims(summary)

	if len(claims) != 2 {
		t.Fatalf("got %d claims, want 2:\n%+v", len(claims), claims)
	}
	if claims[0].CitedTraceIDs[0] != traceA {
		t.Errorf("claim 0 cites %v, want %s", claims[0].CitedTraceIDs, traceA)
	}
	if claims[1].CitedTraceIDs[0] != traceB {
		t.Errorf("claim 1 cites %v, want %s", claims[1].CitedTraceIDs, traceB)
	}
}

// A SENTENCE THAT CITES NOTHING IS STILL A CLAIM, and it is the one ADR 0009 is
// most concerned with — an ungrounded hypothesis stated confidently. Dropping it
// here would make the check pass by ignoring the thing it exists to catch.
func TestAnUncitedSentenceIsStillAClaim(t *testing.T) {
	summary := "The service is degraded and should be rolled back. Trace " + traceA + " is slow."

	claims := grounding.Claims(summary)

	if len(claims) != 2 {
		t.Fatalf("got %d claims, want 2 — the uncited sentence must not be dropped", len(claims))
	}
	if len(claims[0].CitedTraceIDs) != 0 {
		t.Errorf("claim 0 should cite nothing, got %v", claims[0].CitedTraceIDs)
	}
	if !claims[0].Uncited() {
		t.Error("a claim citing no trace does not report itself as uncited")
	}
	if claims[1].Uncited() {
		t.Error("a claim citing a trace reports itself as uncited")
	}
}

// One sentence may rest on several traces. All of them are its evidence — taking
// only the first would judge the claim against a fraction of what it cites.
func TestAClaimCarriesEveryTraceItCites(t *testing.T) {
	summary := "Both " + traceA + " and " + traceB + " show the same stall."

	claims := grounding.Claims(summary)

	if len(claims) != 1 {
		t.Fatalf("got %d claims, want 1", len(claims))
	}
	if len(claims[0].CitedTraceIDs) != 2 {
		t.Fatalf("claim cites %v, want both traces", claims[0].CitedTraceIDs)
	}
}

// Sentence splitting must not be fooled by the full stops inside identifiers,
// version numbers, or abbreviations that appear constantly in this domain.
func TestSplittingIsNotFooledByDotsInsideIdentifiers(t *testing.T) {
	summary := "The service.name is checkout-api and otel.platform.config_version is sha256:abc. " +
		"Trace " + traceA + " confirms it."

	claims := grounding.Claims(summary)

	if len(claims) != 2 {
		t.Fatalf("got %d claims, want 2:\n%+v", len(claims), claims)
	}
	if !contains(claims[0].Text, "otel.platform.config_version") {
		t.Errorf("an attribute name was split across claims: %q", claims[0].Text)
	}
}

// Whitespace and empty fragments are not claims. A trailing full stop should not
// produce a third, empty thing to judge.
func TestEmptyFragmentsAreNotClaims(t *testing.T) {
	for _, summary := range []string{"", "   ", "\n\n", "."} {
		if claims := grounding.Claims(summary); len(claims) != 0 {
			t.Errorf("%q produced %d claims, want none: %+v", summary, len(claims), claims)
		}
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
