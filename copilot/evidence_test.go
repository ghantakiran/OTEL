package copilot_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/copilot"
)

// Citations is the kind-blind view of what a conversation fetched. It is what
// provenance reads, so that adding an evidence kind does not mean editing the
// provenance check.
func TestCitationsReportEveryPieceOfEvidenceThatEntered(t *testing.T) {
	c := copilot.NewConversation("Why is checkout-api slow?")
	c.AppendAssistant("Looking.", queryTracesCall())
	c.AppendToolResult(copilot.ToolResult{ToolUseID: "toolu_01", Traces: recordedEvidence()})

	got := c.Citations()
	want := []copilot.Citation{{Kind: copilot.KindTrace, ID: traceID}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Citations() = %+v, want %+v", got, want)
	}
}

// A citation carries its kind, so an ID is never ambiguous. A trace ID and a
// Standard's name are both strings; a provenance check comparing bare strings
// could match one against the other, and it would do so in the direction that
// reads as "verified".
func TestACitationCarriesItsKind(t *testing.T) {
	ref := copilot.TraceRef{TraceID: traceID}
	if got := ref.Citation(); got.Kind != copilot.KindTrace || got.ID != traceID {
		t.Errorf("TraceRef.Citation() = %+v", got)
	}
}

// An exchange that fetched nothing cites nothing — and that is not the same as
// one that cited correctly. Grounded() depends on the difference.
func TestAnExchangeWithNoEvidenceHasNoCitations(t *testing.T) {
	c := copilot.NewConversation("?")
	if got := c.Citations(); len(got) != 0 {
		t.Errorf("Citations() = %+v on an exchange that fetched nothing", got)
	}
}

// THE TRIPWIRE.
//
// It does not verify that a new evidence kind is handled — no reflection test
// can, because "handled" means rendered correctly for a model and round-tripped
// through storage, which only a real test of that kind can say. What it does is
// make adding a kind IMPOSSIBLE TO DO SILENTLY.
//
// The failure it exists for: someone adds `Metrics []MetricRef` to ToolResult,
// wires up query_metrics, and forgets renderResult. The metrics are fetched,
// stored, and never shown to the model. Nothing errors. The Copilot simply
// answers without them, and the only symptom is a worse answer during an
// incident — which is indistinguishable from a model having a bad day.
//
// So: this test names the evidence-bearing fields it knows about. Add one and
// this goes red with a list of what else needs a case.
func TestAddingAnEvidenceKindTripsThisTest(t *testing.T) {
	// The fields on ToolResult that carry evidence, as opposed to the ones that
	// carry identity (ToolUseID), health (Path) or an authored failure (Err).
	known := map[string]bool{"Traces": true}

	var found []string
	rt := reflect.TypeOf(copilot.ToolResult{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		// An evidence field is a slice whose element knows its own citation.
		if f.Type.Kind() != reflect.Slice {
			continue
		}
		if _, ok := f.Type.Elem().MethodByName("Citation"); !ok {
			continue
		}
		found = append(found, f.Name)
	}

	if len(found) == 0 {
		t.Fatal("no evidence-bearing field found on ToolResult; this test has stopped checking anything")
	}
	for _, name := range found {
		if !known[name] {
			t.Errorf(`ToolResult.%s is a new evidence kind.

Adding a kind means touching all of these, and none of them will fail on their own:

  copilot/toolrunner.go     Conversation.Citations() — or the kind is invisible to
                            provenance and to the grounding index
  copilot/claude/serialize.go
                            renderResult — or the model never sees the evidence
  copilot/transcript.go     toolResultJSON — or the evidence does not survive a
                            save and a load
  copilot/adversarial/      a fixture per attacker-controlled field on the new ref
                            type — the boundary is per field

Then add %q to this test's known set.`, name, name)
		}
	}
}

// The transcript key was renamed from `evidence` to `traces` in #17, so that
// `metrics` and `logs` can sit beside it and a reader can still tell what each
// record is. Documents written before that must still load: the alternative was
// bumping TranscriptVersion, and a version bump is a migration (#68), which does
// not exist yet.
func TestATranscriptWrittenUnderTheOldEvidenceKeyStillLoads(t *testing.T) {
	legacy := `{
      "version": "otel-platform/copilot-transcript/v1",
      "system": ` + jsonString(copilot.SystemPrompt) + `,
      "turns": [
        {"role": "user", "text": "Why is checkout-api slow?"},
        {"role": "assistant", "text": "Looking.", "calls": [
          {"id": "toolu_01", "name": "query_traces", "input": {"service_name":"checkout-api"}}
        ]},
        {"role": "tool_result", "result": {
          "tool_use_id": "toolu_01",
          "evidence": [
            {"trace_id": "` + traceID + `",
             "service": {"name": "checkout-api"},
             "root_span_name": "POST /checkout",
             "start": "2026-07-28T12:03:22.123456789Z",
             "duration_ns": 42000000}
          ]
        }}
      ]
    }`

	var c copilot.Conversation
	if err := c.UnmarshalJSON([]byte(legacy)); err != nil {
		t.Fatalf("a transcript written under the old key no longer loads: %v", err)
	}

	traces := c.Traces()
	if len(traces) != 1 {
		t.Fatalf("got %d traces from the legacy document, want 1", len(traces))
	}
	if traces[0].TraceID != traceID {
		t.Errorf("trace ID = %q", traces[0].TraceID)
	}
	// And it comes back as a citation, so provenance sees it.
	if got := c.Citations(); len(got) != 1 || got[0].Kind != copilot.KindTrace {
		t.Errorf("Citations() = %+v after loading a legacy document", got)
	}
}

// jsonString quotes a string as a JSON literal, for building fixture documents.
func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}
