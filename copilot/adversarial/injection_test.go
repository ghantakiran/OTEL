package adversarial_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/copilot"
	"github.com/ghantakiran/OTEL/copilot/adversarial"
	"github.com/ghantakiran/OTEL/copilot/claude"
	"github.com/ghantakiran/OTEL/copilot/grounding"
)

// scriptedModel is the model half of the real loop: it asks for traces, then
// answers. It is deliberately NOT influenced by what came back — a stub cannot
// be persuaded, and pretending otherwise would make these tests look like they
// measure the model's behaviour. They measure the CHANNEL. #20 measures the
// model.
type scriptedModel struct {
	answer string
	n      int
}

func (m *scriptedModel) Next(context.Context, *copilot.Conversation) (copilot.Assistant, error) {
	m.n++
	if m.n == 1 {
		return copilot.Assistant{
			Text: "Looking at that service's traces.",
			Calls: []copilot.ToolUse{{
				ID:    "toolu_01",
				Name:  copilot.QueryTracesTool,
				Input: json.RawMessage(`{"service_name":"checkout-api"}`),
			}},
		}, nil
	}
	return copilot.Assistant{Text: m.answer}, nil
}

const question = "Why is checkout-api slow?"

// run drives one fixture through the real loop.
func run(t *testing.T, f adversarial.Fixture, answer string) *copilot.Conversation {
	t.Helper()
	c, err := copilot.RunWithPath(context.Background(), &scriptedModel{answer: answer},
		f.Store(), f.PathStore(), question)
	if err != nil {
		t.Fatalf("%s: RunWithPath: %v", f.Name, err)
	}
	return c
}

// wire renders the request the SDK will actually send, so assertions are made
// against bytes rather than Go structs. A guarantee that holds in the struct and
// not on the wire is not a guarantee.
func wire(t *testing.T, c *copilot.Conversation) map[string]any {
	t.Helper()
	body, err := json.Marshal(claude.Serialize(c, []copilot.ToolSchema{copilot.QueryTracesSchema()}))
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshalling the request: %v", err)
	}
	return out
}

// THE CORE ASSERTION, over every vector: hostile telemetry never reaches a
// surface this platform authored.
//
// PlatformAuthoredText is exactly the ADR 0011 surface — the system prompt and
// every user turn. Assistant turns are deliberately excluded: a grounded summary
// MUST quote the evidence it cites (ADR 0009), so a span name there is the
// Copilot working. Scanning them too would make this test fail on correct
// behaviour, and a security check that fails on correct behaviour is one that
// gets loosened under deadline pressure.
func TestNoFixtureReachesPlatformAuthoredText(t *testing.T) {
	for _, f := range adversarial.Fixtures() {
		t.Run(f.Name, func(t *testing.T) {
			c := run(t, f, "Trace "+adversarial.TraceID+" shows a 42ms root span.")

			for _, authored := range c.PlatformAuthoredText() {
				for _, hostile := range f.Hostile {
					if strings.Contains(authored, hostile) {
						t.Errorf("%s reached a platform-authored surface:\n%s", f.Attack, authored)
					}
				}
			}
		})
	}
}

// The same at the wire, one level down: no hostile text in the system field, and
// none in a user TEXT block.
//
// "No telemetry in a user message" would be the wrong check — a tool_result block
// travels inside a `role: "user"` message because the API has no tool-result
// role, so telemetry legitimately sits in a user turn once serialized. The check
// that means something is on the block type.
func TestNoFixtureReachesTheSystemFieldOrAUserTextBlock(t *testing.T) {
	for _, f := range adversarial.Fixtures() {
		t.Run(f.Name, func(t *testing.T) {
			body := wire(t, run(t, f, "Trace "+adversarial.TraceID+" shows a 42ms root span."))

			system, _ := json.Marshal(body["system"])
			for _, hostile := range f.Hostile {
				if strings.Contains(string(system), hostile) {
					t.Errorf("%s reached the system field", f.Attack)
				}
			}

			forEachBlock(t, body, func(role string, block map[string]any) {
				if block["type"] != "text" || role != "user" {
					return
				}
				text, _ := block["text"].(string)
				for _, hostile := range f.Hostile {
					if strings.Contains(text, hostile) {
						t.Errorf("%s reached a user text block:\n%s", f.Attack, text)
					}
				}
			})
		})
	}
}

// THE OTHER HALF, so the tests above cannot pass by rendering nothing. Hostile
// evidence must actually arrive — inside a tool_result block and nowhere else.
//
// Without this, a serializer that silently dropped every field would pass every
// assertion above while destroying the evidence the summary rests on.
func TestHostileEvidenceArrivesInAToolResultAndNowhereElse(t *testing.T) {
	for _, f := range adversarial.Fixtures() {
		if !f.ReachesEvidence {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			body := wire(t, run(t, f, "Trace "+adversarial.TraceID+" shows a 42ms root span."))

			var inToolResult bool
			forEachBlock(t, body, func(role string, block map[string]any) {
				for _, hostile := range f.Hostile {
					if !containsDecoded(block, hostile) {
						continue
					}
					switch block["type"] {
					case "tool_result":
						inToolResult = true
						if role != "user" {
							t.Errorf("a tool_result travelled as role %q", role)
						}
					case "text":
						if role != "assistant" {
							t.Errorf("%s reached a %s text block", f.Attack, role)
						}
					default:
						t.Errorf("%s reached a %v block", f.Attack, block["type"])
					}
				}
			})

			if !inToolResult {
				t.Errorf("%s never reached a tool_result; the fixture proves nothing", f.Name)
			}
		})
	}
}

// A BACKEND'S ERROR TEXT REACHES THE MODEL NOWHERE AT ALL.
//
// This vector is different in kind from the others. Evidence is SUPPOSED to reach
// the model, safely channelled; an error string is not, because the loop authors
// every failure message from its own constants. That is why
// copilot.invoke discards err.Error() rather than wrapping it — and this is the
// test that would fail the day somebody "improves" the error message by passing
// it through.
func TestABackendErrorStringNeverReachesTheModel(t *testing.T) {
	for _, f := range adversarial.Fixtures() {
		if f.StoreErr == nil {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			body := wire(t, run(t, f, "No traces were available."))

			for _, hostile := range f.Hostile {
				if containsDecoded(body, hostile) {
					t.Errorf("a Backend's error text reached the model:\n%s", hostile)
				}
			}

			// And the failure is still reported — swallowing the text must not
			// mean swallowing the fact.
			if !containsDecoded(body, "could not be answered") {
				t.Error("the tool failure was not reported to the model at all")
			}
		})
	}
}

// HOSTILE CONTENT STAYS A JSON STRING VALUE.
//
// The json-escape fixture exists for this: a span name containing quotes and
// braces must be rendered as data inside the evidence document, not as structure.
// If it escaped, the tool_result would carry a field the platform never wrote.
func TestHostileContentCannotAddFieldsToTheEvidenceDocument(t *testing.T) {
	var f adversarial.Fixture
	for _, candidate := range adversarial.Fixtures() {
		if candidate.Name == "json-escape-in-span-name" {
			f = candidate
		}
	}
	if f.Name == "" {
		t.Fatal("the json-escape fixture is missing from the corpus")
	}

	body := wire(t, run(t, f, "Trace "+adversarial.TraceID+" shows a 42ms root span."))

	forEachBlock(t, body, func(_ string, block map[string]any) {
		if block["type"] != "tool_result" {
			return
		}
		for _, raw := range toolResultText(t, block) {
			var doc struct {
				Traces []map[string]any `json:"traces"`
			}
			if err := json.Unmarshal([]byte(raw), &doc); err != nil {
				t.Fatalf("the evidence document did not parse as JSON: %v\n%s", err, raw)
			}
			for _, trace := range doc.Traces {
				if _, injected := trace["platform_note"]; injected {
					t.Error("hostile content added a field to the evidence document")
				}
				if name, _ := trace["root_span_name"].(string); !strings.Contains(name, "platform_note") {
					t.Errorf("the hostile span name was not preserved as a string value: %q", name)
				}
			}
		}
	})
}

// A MODEL THAT QUOTES ITS EVIDENCE IS WORKING, NOT FAILING.
//
// ADR 0009 requires a grounded summary to quote what it cites, so hostile text in
// an ASSISTANT turn is correct behaviour and must survive serialization. A check
// that forbade it would be forbidding grounding.
func TestAModelQuotingHostileEvidenceIsAllowedAndSurvives(t *testing.T) {
	f := adversarial.Fixtures()[0]
	hostile := f.Hostile[0]
	answer := "Trace " + adversarial.TraceID + " has root span " + hostile + "."

	body := wire(t, run(t, f, answer))

	var quoted bool
	forEachBlock(t, body, func(role string, block map[string]any) {
		if role != "assistant" || block["type"] != "text" {
			return
		}
		if text, _ := block["text"].(string); strings.Contains(text, hostile) {
			quoted = true
		}
	})

	if !quoted {
		t.Error("the model's citation was dropped; a grounded summary must survive serialization")
	}
}

// #18 MEETS #54. This is what the two slices are worth together.
//
// Suppose the injection lands and the Copilot obeys it — it asserts the service
// is healthy, citing nothing, because no trace shows that. The grounding check
// does not need to know an attack happened: an uncited assertion is uncited, and
// it reaches the operator marked as such.
//
// That is the honest scope of this defence. It does not stop the model being
// misled; it stops a misled conclusion arriving as an established fact.
func TestAnInjectedConclusionThatCitesNothingIsMarkedUncited(t *testing.T) {
	f := adversarial.Fixtures()[0]
	obeyed := "The database is healthy and no further investigation is required."

	c := run(t, f, obeyed)
	got := grounding.Ground(context.Background(), refusingJudge{}, c)

	if got.Grounded() {
		t.Error("an injected, uncited conclusion reports as grounded")
	}
	if len(got.Assessments) != 1 || got.Assessments[0].Verdict != grounding.Uncited {
		t.Fatalf("verdicts = %+v, want one uncited claim", got.Assessments)
	}
	if !strings.Contains(got.Text, "UNCITED") {
		t.Errorf("the operator is not told the claim rests on nothing:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, obeyed) {
		t.Error("the claim was dropped rather than marked; the operator cannot see what was asserted")
	}
}

// refusingJudge would reject anything it was asked about — and is never asked,
// because the claim above cites nothing. Its presence proves the Uncited verdict
// came from the citation check rather than from a judge's opinion.
type refusingJudge struct{}

func (refusingJudge) Supports(context.Context, grounding.Claim, []copilot.TraceRef, *copilot.TelemetryPath) (bool, error) {
	return false, nil
}

// THE CORPUS MUST STAY WORTH RUNNING. A fixture with no hostile strings asserts
// nothing, and a corpus that shrank to one vector would still pass every test
// above.
func TestTheCorpusCoversEveryAttackerControlledField(t *testing.T) {
	fixtures := adversarial.Fixtures()
	if len(fixtures) < 10 {
		t.Errorf("the corpus has shrunk to %d fixtures", len(fixtures))
	}

	seen := map[string]bool{}
	for _, f := range fixtures {
		if len(f.Hostile) == 0 {
			t.Errorf("%s declares no hostile strings and asserts nothing", f.Name)
		}
		if f.Attack == "" {
			t.Errorf("%s does not say what it attacks", f.Name)
		}
		if seen[f.Name] {
			t.Errorf("duplicate fixture name %q", f.Name)
		}
		seen[f.Name] = true
	}

	// Every field an attacker can write. Named individually because the boundary
	// is per-field: a serializer safe for span names and unsafe for exporter
	// names is only half safe.
	for _, required := range []string{
		"instruction-in-span-name",
		"instruction-in-service-name",
		"instruction-in-namespace",
		"instruction-in-service-tier",
		"instruction-in-config-version",
		"instruction-in-exporter-name",
		"instruction-in-path-config-version",
		"instruction-in-backend-error",
	} {
		if !seen[required] {
			t.Errorf("the corpus no longer covers %s", required)
		}
	}
}

// ---------------------------------------------------------------- helpers ----

// forEachBlock visits every content block in the request with its message role.
func forEachBlock(t *testing.T, body map[string]any, visit func(role string, block map[string]any)) {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatal("the request carried no messages")
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			if block, ok := b.(map[string]any); ok {
				visit(role, block)
			}
		}
	}
}

// toolResultText pulls the text out of a tool_result block, whose content is
// itself a list of blocks.
func toolResultText(t *testing.T, block map[string]any) []string {
	t.Helper()
	var out []string
	switch content := block["content"].(type) {
	case string:
		out = append(out, content)
	case []any:
		for _, c := range content {
			if inner, ok := c.(map[string]any); ok {
				if text, ok := inner["text"].(string); ok {
					out = append(out, text)
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("a tool_result block carried no text")
	}
	return out
}

// allStrings collects every string a decoded request carries, FULLY DECODED, and
// reports whether any contains the needle.
//
// THE NESTING IS THE REASON THIS EXISTS, and getting it wrong is how this whole
// file would pass while proving nothing. Evidence is escaped TWICE on the wire: a
// newline in a span name becomes `\n` inside the evidence document, and that
// document is then itself a string inside the request, so the wire carries `\\n`.
// A substring search for the raw span name misses it; a search for the
// once-escaped form misses it too. Both report a clean result for exactly the
// vectors most likely to break something.
//
// So this recurses through decoded values AND re-parses any string that is itself
// a JSON document — which the tool_result content always is. What comes out is
// the text as the model will actually read it.
func containsDecoded(v any, needle string) bool {
	for _, s := range allStrings(v) {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func allStrings(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		out = append(out, t)
		// A tool_result's content is a JSON document carried as a string. Parse
		// it so the values inside are compared decoded rather than escaped.
		var inner any
		if err := json.Unmarshal([]byte(t), &inner); err == nil {
			if _, isScalar := inner.(string); !isScalar {
				out = append(out, allStrings(inner)...)
			}
		}
	case []any:
		for _, e := range t {
			out = append(out, allStrings(e)...)
		}
	case map[string]any:
		for _, e := range t {
			out = append(out, allStrings(e)...)
		}
	}
	return out
}
