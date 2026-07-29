package copilot

import (
	"context"
	"errors"
	"fmt"
)

// THE ONE LIST.
//
// Before this file there were two ideas of what tools the Copilot had, and
// nothing held them together:
//
//	claude.Config.Tools   a []ToolSchema the caller handed the model adapter.
//	                      Whatever was in it went out on the wire as `tools`.
//	invoke()              an `if call.Name != QueryTracesTool` that answered
//	                      exactly one name and rejected every other.
//
// Nothing checked that those agreed. A caller could advertise four tools and
// dispatch one, and the failure would not look like a wiring bug: the model
// would call an advertised tool, get "no such tool" back as an ordinary tool
// result, and — being a model — would apologise and try something else. The
// operator sees a Copilot that is oddly bad at answering, during an incident,
// with every test green.
//
// With one tool that gap is invisible. #17 adds query_metrics, query_logs,
// get_contract and get_standards, which is when a surface that can lie about
// itself becomes the seam everything else routes through. So it is closed
// first, and closed by TYPE rather than by discipline, in the same way the
// injection boundary is:
//
//   - A Tool is a schema AND its handler. They are one value; there is no
//     constructor that makes one without the other.
//   - A ToolSet is the only thing that can produce the list the model is shown
//     (Schemas) and the only thing that can dispatch a call (Invoke). Both read
//     the same map.
//   - claude.Config.Tools takes a *ToolSet, not a []ToolSchema. A schema with no
//     handler behind it can no longer reach the wire, because there is no longer
//     a path from a bare schema to a request.
//
// The deletion test: delete ToolSet and the two lists reappear — one in every
// caller that builds a Config, one in the loop — and the only thing keeping them
// equal is that somebody remembers to edit both.

// Handler answers one tool call.
//
// It returns a ToolResult rather than (ToolResult, error) on purpose. A tool
// that fails is not a loop that fails: the loop reports the failure to the model
// as a result with Err set, and the model adapts. That behaviour predates this
// file and is deliberate — a Go error here would invite a caller to abort the
// exchange on a Backend hiccup.
//
// Every Err a Handler sets must be authored by this package, from a constant.
// A Backend's own error text is as attacker-influenced as a span name.
type Handler func(ctx context.Context, call ToolUse) ToolResult

// Tool is one typed tool: what the model is shown, and what answers it.
//
// The pairing is the whole point of the type. A ToolSchema alone is a promise,
// and a promise with nothing behind it is exactly the drift this file exists to
// make unrepresentable.
type Tool struct {
	// Schema is what the model is shown. Vendor-neutral by construction — see
	// QueryTracesSchema for what that means and what it forbids.
	Schema ToolSchema
	// Handle answers a call to this tool.
	Handle Handler
}

// ToolSet is the Copilot's tool surface.
//
// It is deliberately not a map[string]Tool exposed directly: a caller holding
// the map could add a schema without a handler, and the invariant would be back
// to being a convention.
type ToolSet struct {
	byName map[string]Tool
	// order preserves registration order, so Schemas returns a stable list.
	// A map's range order is random, and a tool list that reshuffles between
	// runs makes two identical requests differ — which costs a prompt-cache hit
	// and makes a diff of two recorded requests unreadable.
	order []string
}

// The ways a tool surface can be malformed, named so a caller can tell them
// apart. Every one of them is refused at construction rather than at the wire:
// a surface that is wrong is wrong before a single request is sent, and finding
// out during an incident is the wrong time.
var (
	// ErrNoTools reports an empty surface. A Copilot with no tools is a model
	// answering from memory, which is the ungrounded failure the whole layer
	// exists to prevent (ADR 0009) — the same reason ErrNoTraceStore exists.
	ErrNoTools = errors.New("copilot: a ToolSet needs at least one tool; a Copilot with no tools answers from memory")
	// ErrUnnamedTool reports a tool with no name. The model addresses a tool by
	// name, so an unnamed one can never be called and would sit on the surface
	// as pure noise.
	ErrUnnamedTool = errors.New("copilot: a tool must have a name")
	// ErrNoHandler reports a schema with nothing behind it. This is the drift
	// this file exists to prevent, caught at the one place it can still happen.
	ErrNoHandler = errors.New("copilot: a tool must have a handler; a schema with no handler is a tool the model can call and nothing can answer")
)

// NewToolSet builds the surface, refusing one that cannot be honoured.
func NewToolSet(tools ...Tool) (*ToolSet, error) {
	if len(tools) == 0 {
		return nil, ErrNoTools
	}

	s := &ToolSet{byName: make(map[string]Tool, len(tools)), order: make([]string, 0, len(tools))}
	for _, t := range tools {
		switch {
		case t.Schema.Name == "":
			return nil, ErrUnnamedTool
		case t.Handle == nil:
			return nil, fmt.Errorf("%w: %s", ErrNoHandler, t.Schema.Name)
		}
		// A duplicate is refused rather than last-one-wins. Two tools under one
		// name means the model was shown a description that a different handler
		// answers, which is the same lie as an unhandled schema wearing a
		// different hat.
		if _, seen := s.byName[t.Schema.Name]; seen {
			return nil, fmt.Errorf("copilot: two tools are named %s", t.Schema.Name)
		}
		s.byName[t.Schema.Name] = t
		s.order = append(s.order, t.Schema.Name)
	}
	return s, nil
}

// Schemas is what the model is shown, in registration order.
//
// This is the ONLY way to obtain the advertised list, and it can only be built
// from tools that have handlers. That is what makes "advertised" and
// "dispatchable" the same set rather than two sets that happen to agree.
//
// Nil-safe, because a zero Config is a legal thing to construct and a nil
// surface should serialize as no tools rather than panic.
func (s *ToolSet) Schemas() []ToolSchema {
	if s == nil {
		return nil
	}
	out := make([]ToolSchema, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.byName[name].Schema)
	}
	return out
}

// Names is the dispatchable set, in registration order.
func (s *ToolSet) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

// Has reports whether this surface can answer a call by that name.
func (s *ToolSet) Has(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.byName[name]
	return ok
}

// Invoke runs one tool call, or reports that there is no such tool.
//
// The unknown-tool result echoes the name back because a model that asked for
// the wrong tool needs to know which one — but that name is MODEL-AUTHORED text
// going into a message the model then reads, so it is bounded rather than
// trusted. Unbounded, it is a channel a model could write anything into.
func (s *ToolSet) Invoke(ctx context.Context, call ToolUse) ToolResult {
	tool, ok := s.byName[call.Name]
	if !ok {
		return ToolResult{ToolUseID: call.ID, Err: "no such tool: " + boundedToolName(call.Name)}
	}
	return tool.Handle(ctx, call)
}

// PlatformTools is the Copilot's complete tool surface.
//
// ONE PLACE A TOOL IS ADDED. query_metrics, query_logs, get_contract and
// get_standards join this function and nothing else changes shape — the model
// adapter reads Schemas, the loop reads Invoke, and the vendor-neutrality test
// walks whatever this returns. A tool added anywhere else is a tool the
// neutrality check never sees, which is why there is exactly one of these.
//
// paths may be nil: a Backend that can answer for traces but not for the
// collector's own self-telemetry is a real configuration, and a nil Path on a
// result says "not known" rather than "healthy".
func PlatformTools(store TraceStore, paths TelemetryPathStore) (*ToolSet, error) {
	return NewToolSet(QueryTraces(store, paths))
}

// QueryTraces binds the query_traces schema to the handler that answers it.
//
// The stores are captured rather than passed per call, so a Handler's signature
// stays the same whatever a particular tool needs to reach. get_contract and
// get_standards will front the platform's own artifacts rather than a Backend
// (#17), and this is what lets them do that without widening every other tool's
// parameters.
func QueryTraces(store TraceStore, paths TelemetryPathStore) Tool {
	return Tool{
		Schema: QueryTracesSchema(),
		Handle: func(ctx context.Context, call ToolUse) ToolResult {
			return queryTraces(ctx, store, paths, call)
		},
	}
}
