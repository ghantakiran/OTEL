package copilot

// Transcript persistence: how a Conversation survives the process that made it.
//
// This lives in `copilot` rather than in an adapter for one reason that is not
// stylistic — a Conversation's fields are unexported, and they are unexported on
// purpose: AppendToolResult is the only route by which telemetry enters an
// exchange, and an exported `turns` slice would be a second one. Marshalling has
// to happen where that invariant is already enforced.
//
// THE FAILURE MODE THIS FILE EXISTS TO MAKE IMPOSSIBLE. A tool result is the sole
// carrier of telemetry into a conversation (see ToolResult). Within one turn that
// is held by the type: Evidence is []TraceRef and cannot be confused with authored
// text. The moment a transcript is written to disk and read back, that guarantee
// is only as good as the wire format — a store that flattened Evidence into a
// rendered string would hand the next turn's prompt-builder a plain string with no
// way left to tell it from something the platform wrote. The block-level
// separation that ADR 0011 depends on would survive the turn and die at the file.
//
// So the storage form keeps evidence as a typed array, and Unmarshal REFUSES a
// document that carries telemetry anywhere else — a tool-result turn with text on
// it, or an authored turn with a result attached. A flattened transcript does not
// load with a warning; it does not load.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TranscriptVersion is the storage format's identifier, and it is checked on the
// way in.
//
// Same discipline as a Telemetry Contract's apiVersion (contract.Load refuses a
// document that declares none): a format that is not declared is a format that
// gets guessed at, and a guess about which field held the evidence is the guess
// this file is here to prevent.
const TranscriptVersion = "otel-platform/copilot-transcript/v1"

var (
	// ErrUnknownTranscriptVersion is returned for a document whose version is
	// missing or unrecognised. Refusing beats interpreting: the fields this format
	// is careful about are exactly the ones a future version might move.
	ErrUnknownTranscriptVersion = errors.New("copilot: unrecognised transcript version")

	// ErrTelemetryInAuthoredTurn is THE guard. It is returned when a stored
	// document puts tool-result content on an authored turn, or authored text on a
	// tool-result turn.
	//
	// This is what a string-concatenating store produces, and it is refused rather
	// than repaired. Repairing it would mean deciding which half of a merged string
	// was evidence, which is not a decision that can be made correctly.
	ErrTelemetryInAuthoredTurn = errors.New("copilot: a stored turn mixes authored text with tool-result content")

	// ErrSystemPromptMismatch is returned when a stored system prompt is not the
	// constant this build ships.
	//
	// The system prompt is the operator channel — the one surface where text is
	// read as instruction rather than weighed as content. Reading it back from a
	// file would make it whatever the file says, and a transcript is a thing that
	// sits on a disk. So it is compared, not trusted.
	//
	// THE COST, NAMED: changing SystemPrompt makes every transcript written before
	// the change unloadable. That is deliberate for a tracer bullet — a transcript
	// replayed under a prompt it was not produced under is a different exchange
	// wearing the same ID — but it is a migration this format does not yet have,
	// and #20's Eval Harness replay is what will need one first. Tracked by #68.
	ErrSystemPromptMismatch = errors.New("copilot: stored system prompt is not this build's SystemPrompt")

	// ErrNoTranscript is returned by a TranscriptStore for an ID it does not hold.
	ErrNoTranscript = errors.New("copilot: no such transcript")

	// ErrBadTranscriptID is returned for an ID that could not be used as a storage
	// key safely.
	ErrBadTranscriptID = errors.New("copilot: invalid transcript ID")

	// ErrUnpairedToolUse is returned when a stored exchange has a tool call with no
	// tool-result turn answering it, or a tool result answering nothing.
	//
	// THIS IS WHAT CATCHES A FLATTENING STORE, and it is the reason the pairing is
	// checked at all. A store that renders evidence into prose does not usually
	// leave a malformed tool-result turn behind — it drops the turn entirely and
	// writes the text somewhere authored. The result is a document whose every turn
	// is individually well-formed: an assistant turn that asked for traces, and a
	// user turn that happens to contain them.
	//
	// Nothing about that user turn is detectable on its own. A question from an
	// operator and a rendered tool result are both just text, and no format can
	// tell them apart. What IS detectable is the hole the flattening left: a
	// tool_use with nothing answering it. The API requires that pairing anyway —
	// a tool_use the model never sees answered is one it cannot continue from — so
	// this refuses a document that was already unusable, and catches the flattening
	// on the way.
	ErrUnpairedToolUse = errors.New("copilot: a stored tool call has no matching tool result")
)

// TranscriptStore is where an exchange is kept between turns.
//
// Two methods, because a follow-up turn needs exactly two things: the exchange
// that happened, and somewhere to put the exchange that is happening. It is an
// interface for the same reason TraceStore is one — the tests here run against an
// in-memory implementation and touch no disk, and a file store is one adapter
// rather than the only possibility.
type TranscriptStore interface {
	// Save records the exchange under id, replacing any exchange already there.
	Save(ctx context.Context, id string, c *Conversation) error
	// Load returns the exchange stored under id.
	//
	// It returns ErrNoTranscript when there is none, which is a finding rather
	// than a failure — a follow-up on an exchange that was never saved is a
	// caller's mistake worth naming, not an I/O error.
	Load(ctx context.Context, id string) (*Conversation, error)
}

// ------------------------------------------------------------ the wire form ----

// transcriptJSON is the stored shape of a whole exchange.
//
// It is a separate set of types from the ones in copilot/claude, and the reason is
// the same one written down there: those are what a MODEL reads, and these are
// what this platform keeps. They differ where their owners differ — a duration is
// milliseconds on the model's wire because that is readable, and nanoseconds here
// because a store that loses precision is a store that fails a round-trip test for
// a reason nobody can find later.
type transcriptJSON struct {
	Version string     `json:"version"`
	System  string     `json:"system"`
	Turns   []turnJSON `json:"turns"`
}

// turnJSON is one stored message.
//
// Text and Result are separate fields and neither is ever the other. That is the
// whole format: there is no field into which a rendered tool result could be
// written and still be a valid turn.
type turnJSON struct {
	Role string `json:"role"`
	// Text is authored — platform or model. Never telemetry-derived.
	Text string `json:"text,omitempty"`
	// Calls is the model's tool requests, on an assistant turn.
	Calls []toolUseJSON `json:"calls,omitempty"`
	// Result is the tool's answer, on a tool-result turn, and the only place in
	// this document where telemetry may appear.
	Result *toolResultJSON `json:"result,omitempty"`
}

type toolUseJSON struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type toolResultJSON struct {
	ToolUseID string `json:"tool_use_id"`
	// Evidence is an ARRAY of records, and that is the load-bearing part of this
	// format. A string here is the bug this file was written to prevent.
	Evidence []traceRefJSON `json:"evidence"`
	// TelemetryPath rides on the same result it did in memory, for the same
	// reason: the two answer one question.
	TelemetryPath *telemetryPathJSON `json:"telemetry_path,omitempty"`
	// Error is authored by this package, from a constant. A Backend's own words
	// never reach it, in memory or here.
	Error string `json:"error,omitempty"`
}

type traceRefJSON struct {
	TraceID       string              `json:"trace_id"`
	Service       serviceIdentityJSON `json:"service"`
	RootSpanName  string              `json:"root_span_name"`
	Start         string              `json:"start"`
	DurationNanos int64               `json:"duration_ns"`
	ConfigVersion string              `json:"collector_config_version,omitempty"`
}

type serviceIdentityJSON struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Tier      string `json:"tier,omitempty"`
}

type telemetryPathJSON struct {
	ConfigVersion string         `json:"collector_config_version,omitempty"`
	PerExporter   []exporterJSON `json:"per_exporter"`
}

type exporterJSON struct {
	Name          string  `json:"name"`
	QueueSize     float64 `json:"queue_size"`
	QueueCapacity float64 `json:"queue_capacity"`
	EnqueueFailed float64 `json:"enqueue_failed"`
	SendFailed    float64 `json:"send_failed"`
}

// --------------------------------------------------------------- marshalling ----

// MarshalJSON writes the exchange in the storage form.
//
// Times go out in UTC at nanosecond precision, so a reloaded TraceRef compares
// equal to the one that was stored. A Start that came back in a different location
// would still be the same instant, but it would not be the same value, and a
// round-trip test that has to know about that is a test that will be weakened.
func (c *Conversation) MarshalJSON() ([]byte, error) {
	if c == nil {
		return nil, errors.New("copilot: cannot marshal a nil Conversation")
	}

	doc := transcriptJSON{
		Version: TranscriptVersion,
		System:  c.system,
		Turns:   make([]turnJSON, 0, len(c.turns)),
	}

	for i, t := range c.turns {
		out := turnJSON{Role: string(t.Role), Text: t.Text}

		for _, call := range t.Calls {
			out.Calls = append(out.Calls, toolUseJSON{
				ID: call.ID, Name: call.Name, Input: compactJSON(call.Input),
			})
		}

		if t.Result != nil {
			r := t.Result
			res := &toolResultJSON{
				ToolUseID: r.ToolUseID,
				Evidence:  make([]traceRefJSON, 0, len(r.Evidence)),
				Error:     r.Err,
			}
			for _, e := range r.Evidence {
				res.Evidence = append(res.Evidence, traceRefJSON{
					TraceID: e.TraceID,
					Service: serviceIdentityJSON{
						Name: e.Service.Name, Namespace: e.Service.Namespace, Tier: e.Service.Tier,
					},
					RootSpanName:  e.RootSpanName,
					Start:         e.Start.UTC().Format(time.RFC3339Nano),
					DurationNanos: int64(e.Duration),
					ConfigVersion: e.ConfigVersion,
				})
			}
			if p := r.Path; p != nil {
				path := &telemetryPathJSON{
					ConfigVersion: p.ConfigVersion,
					PerExporter:   make([]exporterJSON, 0, len(p.PerExporter)),
				}
				for _, ex := range p.PerExporter {
					path.PerExporter = append(path.PerExporter, exporterJSON{
						Name:          ex.Name,
						QueueSize:     ex.QueueSize,
						QueueCapacity: ex.QueueCapacity,
						EnqueueFailed: ex.EnqueueFailed,
						SendFailed:    ex.SendFailed,
					})
				}
				res.TelemetryPath = path
			}
			out.Result = res
		}

		// Refuse to WRITE what we would refuse to read. A store that can emit a
		// document its own loader rejects is one that loses an exchange at reload
		// time, when the operator is mid-incident and least able to do anything
		// about it.
		if err := checkTurnSeparation(out); err != nil {
			return nil, fmt.Errorf("copilot: turn %d: %w", i, err)
		}

		doc.Turns = append(doc.Turns, out)
	}

	if err := checkToolUsePairing(doc.Turns); err != nil {
		return nil, err
	}

	return json.Marshal(doc)
}

// UnmarshalJSON reads a stored exchange back, and refuses one whose turns do not
// keep authored text and tool-result content apart.
//
// The refusal is the point. This is the seam where a flattened transcript would
// otherwise become an ordinary Conversation with telemetry sitting in Text, and
// every downstream guard — PlatformAuthoredText, the serializer's block split,
// ADR 0011 at the wire — reads Text as authored by definition. None of them could
// catch it, because by then there is nothing left to catch.
func (c *Conversation) UnmarshalJSON(b []byte) error {
	var doc transcriptJSON
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("copilot: reading transcript: %w", err)
	}

	if doc.Version != TranscriptVersion {
		return fmt.Errorf("%w: %q", ErrUnknownTranscriptVersion, doc.Version)
	}
	if doc.System != SystemPrompt {
		return ErrSystemPromptMismatch
	}

	turns := make([]Turn, 0, len(doc.Turns))
	for i, in := range doc.Turns {
		switch Role(in.Role) {
		case RoleUser, RoleAssistant, RoleToolResult:
		default:
			return fmt.Errorf("copilot: turn %d: unknown role %q", i, in.Role)
		}
		if err := checkTurnSeparation(in); err != nil {
			return fmt.Errorf("copilot: turn %d: %w", i, err)
		}

		t := Turn{Role: Role(in.Role), Text: in.Text}

		for _, call := range in.Calls {
			t.Calls = append(t.Calls, ToolUse{ID: call.ID, Name: call.Name, Input: compactJSON(call.Input)})
		}

		if in.Result != nil {
			r := &ToolResult{ToolUseID: in.Result.ToolUseID, Err: in.Result.Error}
			for j, e := range in.Result.Evidence {
				start, err := time.Parse(time.RFC3339Nano, e.Start)
				if err != nil {
					return fmt.Errorf("copilot: turn %d evidence %d: start is not RFC3339", i, j)
				}
				r.Evidence = append(r.Evidence, TraceRef{
					TraceID: e.TraceID,
					Service: ServiceIdentity{
						Name: e.Service.Name, Namespace: e.Service.Namespace, Tier: e.Service.Tier,
					},
					RootSpanName:  e.RootSpanName,
					Start:         start.UTC(),
					Duration:      time.Duration(e.DurationNanos),
					ConfigVersion: e.ConfigVersion,
				})
			}
			if p := in.Result.TelemetryPath; p != nil {
				path := &TelemetryPath{ConfigVersion: p.ConfigVersion}
				for _, ex := range p.PerExporter {
					path.PerExporter = append(path.PerExporter, ExporterHealth{
						Name:          ex.Name,
						QueueSize:     ex.QueueSize,
						QueueCapacity: ex.QueueCapacity,
						EnqueueFailed: ex.EnqueueFailed,
						SendFailed:    ex.SendFailed,
					})
				}
				r.Path = path
			}
			t.Result = r
		}

		turns = append(turns, t)
	}

	if err := checkToolUsePairing(doc.Turns); err != nil {
		return err
	}

	// SystemPrompt, not doc.System. They are equal — that was just checked — and
	// assigning the constant is what keeps them equal if this check is ever
	// relaxed into a warning by someone who needs an old transcript to load.
	c.system = SystemPrompt
	c.turns = turns
	return nil
}

// checkTurnSeparation is the one rule, in one place, applied on the way out and on
// the way in.
//
// A tool-result turn carries a result and no text. An authored turn carries text
// and no result. There is no third shape, and in particular there is no shape in
// which a rendered tool result could sit in Text and still be a turn this format
// admits.
func checkTurnSeparation(t turnJSON) error {
	if Role(t.Role) == RoleToolResult {
		if t.Text != "" {
			return fmt.Errorf("%w: a tool_result turn carries text", ErrTelemetryInAuthoredTurn)
		}
		if t.Result == nil {
			return fmt.Errorf("%w: a tool_result turn carries no result", ErrTelemetryInAuthoredTurn)
		}
		if len(t.Calls) > 0 {
			return fmt.Errorf("%w: a tool_result turn carries tool calls", ErrTelemetryInAuthoredTurn)
		}
		return nil
	}
	if t.Result != nil {
		return fmt.Errorf("%w: a %s turn carries a tool result", ErrTelemetryInAuthoredTurn, t.Role)
	}
	return nil
}

// checkToolUsePairing verifies that every tool call was answered by a tool-result
// turn, and that every tool result answers a call.
//
// See ErrUnpairedToolUse for why this is the check that catches a flattening
// store: the flattened turn itself is undetectable, but the missing answer is not.
func checkToolUsePairing(turns []turnJSON) error {
	called := map[string]bool{}
	answered := map[string]bool{}

	for _, t := range turns {
		for _, c := range t.Calls {
			if called[c.ID] {
				return fmt.Errorf("%w: tool call %q appears twice", ErrUnpairedToolUse, c.ID)
			}
			called[c.ID] = true
		}
		if t.Result != nil {
			id := t.Result.ToolUseID
			if answered[id] {
				return fmt.Errorf("%w: tool call %q is answered twice", ErrUnpairedToolUse, id)
			}
			answered[id] = true
		}
	}

	for id := range called {
		if !answered[id] {
			return fmt.Errorf("%w: %q was called and never answered", ErrUnpairedToolUse, id)
		}
	}
	for id := range answered {
		if !called[id] {
			return fmt.Errorf("%w: a result answers %q, which was never called", ErrUnpairedToolUse, id)
		}
	}
	return nil
}

// compactJSON normalises a raw JSON value so a round trip is byte-stable.
//
// The store writes indented documents because an operator reads them during an
// incident, and indenting re-formats an embedded json.RawMessage — so the model's
// own arguments would come back differing from what went in, by whitespace alone.
// Compacting on both sides makes that a non-question, and also means a transcript
// someone reformatted by hand still reloads equal.
//
// An invalid value is passed through untouched. It came from the model, this
// package does not author it, and a parse failure here is not worth losing the
// tool_use ID pairing over.
func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return json.RawMessage(buf.Bytes())
}

// ------------------------------------------------------------------ resuming ----

// AppendUser records a follow-up question from whoever is running the incident.
//
// It is the second entry point for authored text, after NewConversation, and it
// exists because a reloaded exchange needs one: a Conversation that could only
// ever be started could never be continued. Like the first question, this is
// operator-authored and never telemetry — the type says nothing about that, so
// the doc comment has to, and the guard that actually holds it is that no code
// path in this package passes evidence to it.
func (c *Conversation) AppendUser(text string) {
	c.turns = append(c.turns, Turn{Role: RoleUser, Text: text})
}

// Resume loads a stored exchange and appends the operator's follow-up question,
// returning a Conversation ready for another turn.
//
// It deliberately does NOT run the loop. Handing back a *Conversation rather than
// driving one keeps this file a persistence seam: what a resumed exchange costs in
// turns, and how it should end, is the Tool Runner's bounded-loop question and not
// this one.
func Resume(ctx context.Context, store TranscriptStore, id, question string) (*Conversation, error) {
	if store == nil {
		return nil, errors.New("copilot: Resume needs a TranscriptStore")
	}
	c, err := store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	c.AppendUser(question)
	return c, nil
}
