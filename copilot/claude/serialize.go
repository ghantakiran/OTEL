// Package claude is the second designated adapter in Layer 3, and the only one
// that knows a model vendor exists.
//
// It mirrors copilot/backend exactly. That package is the one place that knows a
// Backend's query language; this one is the one place that knows a model API's
// message shape. Both import `copilot`; `copilot` imports neither, so nothing
// vendor-shaped can reach the typed tool surface (ADR 0007, ADR 0011).
//
// THIS FILE HOLDS THE ONLY TraceRef → string FUNCTION IN THE REPOSITORY, and it
// is unexported. Until now no such conversion existed anywhere — that absence is
// what made "telemetry cannot reach a prompt" a structural claim rather than a
// convention. Writing it is the moment the claim stops being free, so it is
// written in exactly one place, reachable from exactly one caller: the code that
// builds a tool_result block.
package claude

import (
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/ghantakiran/OTEL/copilot"
)

// Serialize renders a Conversation and the tool surface into a request.
//
// It returns the SDK's own parameter type rather than a hand-rolled struct: the
// wire shape is then the SDK's problem, and a shape error is a compile error
// instead of a 400 at 3am.
//
// TWO FIELDS ARE THE CALLER'S, AND THEY FAIL DIFFERENTLY — worth knowing before
// the first real call rather than after it:
//
//	Model       omitted entirely when unset, because the SDK marshals it with
//	            omitempty. Deliberately unset here: ADR 0011 names a model that
//	            has since been superseded (#55), and a serializer that picked one
//	            would put a stale identifier in every request.
//	MaxTokens   NOT omitted when unset — it marshals as `"max_tokens": 0`, which
//	            the API rejects. So an unset MaxTokens is an invalid request
//	            rather than an absent field, and the 400 says nothing about which
//	            of the two the caller forgot.
//
// Both are the caller's to set. The asymmetry is the SDK's, not this package's,
// and it is written down because it is invisible from the type.
//
// WHERE EVIDENCE LANDS ON THE WIRE, and why it needs saying: a tool_result block
// travels inside a `role: "user"` message. There is no tool-result role in the
// API. So the moment this function runs, telemetry is sitting in a user turn —
// the very surface ADR 0011 is about.
//
// That is not a violation, and the distinction is structural rather than a matter
// of care: a user message this function emits holds EITHER authored text blocks
// OR tool_result blocks, never both. Nothing merges them, so "telemetry never
// enters the prompt as platform-authored instruction" remains checkable at the
// wire — a text block in a user message is ours, a tool_result block is not.
func Serialize(c *copilot.Conversation, tools []copilot.ToolSchema) anthropic.MessageNewParams {
	req := anthropic.MessageNewParams{
		// The system prompt goes in the API's own system field, not in a user
		// turn. An instruction in a user turn is text the model weighs against
		// everything else in the conversation; the system field is the operator
		// channel. Which one the platform's framing travels in is not a
		// formatting choice.
		System: []anthropic.TextBlockParam{{Text: c.System()}},
	}

	for _, tool := range tools {
		req.Tools = append(req.Tools, toolParam(tool))
	}

	for _, turn := range c.Turns() {
		switch turn.Role {
		case copilot.RoleUser:
			req.Messages = append(req.Messages,
				anthropic.NewUserMessage(anthropic.NewTextBlock(turn.Text)))

		case copilot.RoleAssistant:
			req.Messages = append(req.Messages, assistantMessage(turn))

		case copilot.RoleToolResult:
			req.Messages = append(req.Messages, toolResultMessage(turn))
		}
	}
	return req
}

// assistantMessage renders the model's own turn back to it.
//
// The text and every tool_use block have to travel together in one message: the
// API pairs a tool_result to its tool_use by ID, and a tool_use the model never
// sees echoed is one it cannot be answered about.
func assistantMessage(turn copilot.Turn) anthropic.MessageParam {
	var blocks []anthropic.ContentBlockParamUnion
	if turn.Text != "" {
		blocks = append(blocks, anthropic.NewTextBlock(turn.Text))
	}
	for _, call := range turn.Calls {
		var input any
		// The model's own arguments, echoed back as it sent them. A parse failure
		// is not fatal: an empty object keeps the block well-formed and the ID
		// pairing intact, which is what the next turn actually depends on.
		_ = json.Unmarshal(call.Input, &input)
		if input == nil {
			input = map[string]any{}
		}
		blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, input, call.Name))
	}
	return anthropic.NewAssistantMessage(blocks...)
}

// toolResultMessage renders evidence.
//
// This is the only caller of renderEvidence, and it can only produce a
// tool_result block. There is no path from here to a text block.
func toolResultMessage(turn copilot.Turn) anthropic.MessageParam {
	r := turn.Result
	if r == nil {
		return anthropic.NewUserMessage()
	}
	if r.Err != "" {
		// Authored by copilot, from a constant — never a Backend's own words.
		return anthropic.NewUserMessage(
			anthropic.NewToolResultBlock(r.ToolUseID, r.Err, true))
	}
	return anthropic.NewUserMessage(
		anthropic.NewToolResultBlock(r.ToolUseID, renderResult(*r), false))
}

// resultJSON is the wire form of one tool answer: the traces, and the state of
// the road they travelled.
//
// Both ride in ONE tool result because they answer one question. A model that had
// to remember to ask twice would sometimes answer from half the evidence — and
// the half it would skip is the one that distinguishes a broken service from a
// broken telemetry path.
type resultJSON struct {
	Traces        []evidenceJSON `json:"traces"`
	TelemetryPath *pathJSON      `json:"telemetry_path,omitempty"`
}

// pathJSON is the wire form of a TelemetryPath.
//
// `collectors_reporting` is stated rather than left to be inferred from an empty
// array: "no collector has reported" and "every collector is healthy" are
// different findings, and an empty list reads like the second.
type pathJSON struct {
	ConfigVersion       string         `json:"collector_config_version,omitempty"`
	CollectorsReporting bool           `json:"collectors_reporting"`
	PerBackend          []exporterJSON `json:"per_backend"`
	TelemetryDropped    bool           `json:"telemetry_dropped"`
}

// exporterJSON is one Backend's delivery health. The name is the Backend's.
type exporterJSON struct {
	Backend       string  `json:"backend"`
	QueueSize     float64 `json:"queue_size"`
	QueueCapacity float64 `json:"queue_capacity"`
	EnqueueFailed float64 `json:"telemetry_dropped_count"`
	SendFailed    float64 `json:"send_failed_count"`
}

// evidenceJSON is the wire form of a TraceRef.
//
// It is a separate type rather than json tags on TraceRef because the two answer
// to different owners: TraceRef is the platform's vocabulary, and this is what a
// model reads. Tagging TraceRef itself would make every future field a wire
// change by default, which is how a vendor's expectations leak upward.
type evidenceJSON struct {
	TraceID       string `json:"trace_id"`
	ServiceName   string `json:"service_name"`
	Namespace     string `json:"service_namespace,omitempty"`
	Tier          string `json:"service_tier,omitempty"`
	RootSpanName  string `json:"root_span_name"`
	StartRFC3339  string `json:"start"`
	DurationMs    int64  `json:"duration_ms"`
	ConfigVersion string `json:"collector_config_version,omitempty"`
}

// renderResult is THE telemetry → string function. There is no other, and it is
// unexported so there cannot be one outside this file.
//
// JSON rather than prose, deliberately. Prose would require this function to
// write sentences ABOUT telemetry, and a sentence is the shape an instruction
// takes — the rendering step is exactly where "data, not instructions" is easiest
// to lose. A JSON array reads as a record set; nothing here addresses the model.
//
// Note what is absent: no preamble, no "here are the traces", no framing. The
// framing lives in the system prompt, which is ours. Putting it here would place
// authored text in the same block as attacker-controlled values, and then the two
// are one string.
func renderResult(r copilot.ToolResult) string {
	out := resultJSON{Traces: make([]evidenceJSON, 0, len(r.Evidence))}
	for _, r := range r.Evidence {
		out.Traces = append(out.Traces, evidenceJSON{
			TraceID:       r.TraceID,
			ServiceName:   r.Service.Name,
			Namespace:     r.Service.Namespace,
			Tier:          r.Service.Tier,
			RootSpanName:  r.RootSpanName,
			StartRFC3339:  r.Start.UTC().Format("2006-01-02T15:04:05Z07:00"),
			DurationMs:    r.Duration.Milliseconds(),
			ConfigVersion: r.ConfigVersion,
		})
	}

	if p := r.Path; p != nil {
		path := &pathJSON{
			ConfigVersion:       p.ConfigVersion,
			CollectorsReporting: len(p.PerExporter) > 0,
			PerBackend:          make([]exporterJSON, 0, len(p.PerExporter)),
			TelemetryDropped:    p.Dropping(),
		}
		for _, e := range p.PerExporter {
			path.PerBackend = append(path.PerBackend, exporterJSON{
				Backend:       e.Name,
				QueueSize:     e.QueueSize,
				QueueCapacity: e.QueueCapacity,
				EnqueueFailed: e.EnqueueFailed,
				SendFailed:    e.SendFailed,
			})
		}
		out.TelemetryPath = path
	}

	// A marshalling failure cannot come from the fields above — every one is a
	// string, a bool or a number — but returning an empty object rather than the
	// zero value keeps the tool result parseable whatever happens.
	body, err := json.Marshal(out)
	if err != nil {
		return `{"traces":[]}`
	}
	return string(body)
}

// toolParam renders a typed tool as the API describes one.
//
// The schema crosses unchanged. It was built in `copilot` with no vendor in it,
// and nothing here adds one — which is what makes swapping the model a change to
// this package alone.
func toolParam(t copilot.ToolSchema) anthropic.ToolUnionParam {
	schema := anthropic.ToolInputSchemaParam{}
	if props, ok := t.InputSchema["properties"].(map[string]any); ok {
		schema.Properties = props
	}
	if required, ok := t.InputSchema["required"].([]string); ok {
		schema.Required = required
	}

	tool := anthropic.ToolParam{
		Name:        t.Name,
		Description: anthropic.String(t.Description),
		InputSchema: schema,
	}
	return anthropic.ToolUnionParam{OfTool: &tool}
}
