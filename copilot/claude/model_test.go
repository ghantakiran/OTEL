package claude_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ghantakiran/OTEL/copilot"
	"github.com/ghantakiran/OTEL/copilot/claude"
)

// RECORDED RESPONSE BODIES, in the shape the Messages API returns them. No key is
// read and no request leaves the machine — an httptest server answers instead, so
// `go test ./...` stays hermetic on a laptop and in CI alike.
const (
	// The turn that drives the loop: a sentence, then a tool call.
	respToolUse = `{
      "id": "msg_01ABC",
      "type": "message",
      "role": "assistant",
      "model": "claude-opus-5",
      "content": [
        {"type": "text", "text": "Looking at that service's traces."},
        {"type": "tool_use", "id": "toolu_01", "name": "query_traces",
         "input": {"service_name": "checkout-api", "limit": 5}}
      ],
      "stop_reason": "tool_use",
      "usage": {"input_tokens": 1200, "output_tokens": 80}
    }`

	// The turn that ends it.
	respFinalText = `{
      "id": "msg_02DEF",
      "type": "message",
      "role": "assistant",
      "model": "claude-opus-5",
      "content": [
        {"type": "text", "text": "Trace fe3852be4562dca17922b0b2758ff910 shows a 42ms root span."}
      ],
      "stop_reason": "end_turn",
      "usage": {"input_tokens": 1400, "output_tokens": 30}
    }`

	// A safety classifier declining. HTTP 200, empty content, stop_reason refusal.
	// Code that reads content[0] unconditionally breaks here.
	respRefusal = `{
      "id": "msg_03GHI",
      "type": "message",
      "role": "assistant",
      "model": "claude-opus-5",
      "content": [],
      "stop_reason": "refusal",
      "stop_details": {"type": "refusal", "category": "cyber"},
      "usage": {"input_tokens": 900, "output_tokens": 0}
    }`

	// Thinking is ON BY DEFAULT on this model, so a thinking block can lead the
	// content array. It carries no answer and must not be mistaken for one.
	respThinkingThenText = `{
      "id": "msg_04JKL",
      "type": "message",
      "role": "assistant",
      "model": "claude-opus-5",
      "content": [
        {"type": "thinking", "thinking": "", "signature": "abc"},
        {"type": "text", "text": "The service looks healthy."}
      ],
      "stop_reason": "end_turn",
      "usage": {"input_tokens": 1000, "output_tokens": 20}
    }`
)

// answering stands up a stub API returning the given body, and a Client pointed at
// it. It also captures the request, so a test can assert on what was SENT rather
// than only on what came back.
func answering(t *testing.T, body string) (*claude.Client, *[]byte) {
	t.Helper()
	var sent []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// io.ReadAll, not a single Read. Read may return a SHORT read, and the
		// assertions below are absence checks — a truncated body would make
		// "no sampling parameter reached the wire" pass for the wrong reason.
		buf, _ := io.ReadAll(r.Body)
		sent = buf
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c, err := claude.New(claude.Config{
		Model:     "claude-opus-5",
		MaxTokens: 8192,
		BaseURL:   srv.URL,
		APIKey:    "not-a-real-key",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, &sent
}

// THE TRACER BULLET. A real Messages API response becomes a copilot.Assistant:
// the text, and the tool call the loop acts on.
func TestAToolUseInTheResponseBecomesAToolCall(t *testing.T) {
	c, _ := answering(t, respToolUse)

	got, err := c.Next(context.Background(), copilot.NewConversation("Why is checkout-api slow?"))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	if got.Text != "Looking at that service's traces." {
		t.Errorf("Text = %q", got.Text)
	}
	if len(got.Calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(got.Calls))
	}
	if got.Calls[0].ID != "toolu_01" {
		t.Errorf("call ID = %q — without it a result cannot be paired", got.Calls[0].ID)
	}
	if got.Calls[0].Name != copilot.QueryTracesTool {
		t.Errorf("call name = %q", got.Calls[0].Name)
	}
	// The arguments must survive as JSON the tool layer can unmarshal into a
	// typed TraceQuery — that is the whole point of the seam above this.
	if len(got.Calls[0].Input) == 0 {
		t.Error("the tool call carries no input")
	}
}

// A turn with no tool call ends the loop. Run stops when Calls is empty, so a
// dropped tool call would look like a finished answer.
func TestAFinalTextTurnCarriesNoToolCalls(t *testing.T) {
	c, _ := answering(t, respFinalText)

	got, err := c.Next(context.Background(), copilot.NewConversation("?"))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	if len(got.Calls) != 0 {
		t.Errorf("a final turn carries %d tool calls", len(got.Calls))
	}
	if got.Text == "" {
		t.Error("the final turn carries no text")
	}
}

// A REFUSAL IS NOT AN ANSWER. The classifiers can decline a request: HTTP 200,
// empty content, stop_reason "refusal". Returning that as an ordinary empty turn
// would end the loop silently and read as "the Copilot had nothing to say",
// which is a different and much worse claim than "the request was declined".
func TestARefusalIsSurfacedRatherThanReadAsAnEmptyAnswer(t *testing.T) {
	c, _ := answering(t, respRefusal)

	_, err := c.Next(context.Background(), copilot.NewConversation("?"))

	if err == nil {
		t.Fatal("a refusal came back as a successful empty turn")
	}
	if !errors.Is(err, claude.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused so a caller can tell it from a transport failure", err)
	}
}

// Thinking is on by default on this model, so a thinking block can lead the
// content array. It carries no answer — the text after it does.
func TestAThinkingBlockIsNotMistakenForTheAnswer(t *testing.T) {
	c, _ := answering(t, respThinkingThenText)

	got, err := c.Next(context.Background(), copilot.NewConversation("?"))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	if got.Text != "The service looks healthy." {
		t.Errorf("Text = %q, want the text block rather than the thinking block", got.Text)
	}
}

// The two caller-supplied fields are refused at construction, not at the API.
// An empty Model or a zero MaxTokens produces a request the API rejects, and its
// 400 says nothing about which one was missing — so they are named here instead.
func TestAModelWithNoIdIsRefusedAtConstruction(t *testing.T) {
	_, err := claude.New(claude.Config{MaxTokens: 8192})

	if !errors.Is(err, claude.ErrNoModel) {
		t.Fatalf("err = %v, want ErrNoModel", err)
	}
}

func TestAModelWithNoTokenBudgetIsRefusedAtConstruction(t *testing.T) {
	_, err := claude.New(claude.Config{Model: "claude-opus-5"})

	if !errors.Is(err, claude.ErrNoMaxTokens) {
		t.Fatalf("err = %v, want ErrNoMaxTokens", err)
	}
}

// No sampling parameter may reach the wire. They are rejected with a 400 on this
// model, and the failure would arrive as an opaque API error rather than
// anything naming the cause.
func TestNoSamplingParameterReachesTheWire(t *testing.T) {
	c, sent := answering(t, respFinalText)

	if _, err := c.Next(context.Background(), copilot.NewConversation("?")); err != nil {
		t.Fatalf("Next: %v", err)
	}

	body := string(*sent)
	for _, param := range []string{"temperature", "top_p", "top_k"} {
		if contains(body, `"`+param+`"`) {
			t.Errorf("%s reached the wire; this model rejects it with a 400", param)
		}
	}
}

// The request carries the configured model and token budget — the two fields the
// serializer deliberately leaves to the caller.
func TestTheConfiguredModelAndBudgetReachTheWire(t *testing.T) {
	c, sent := answering(t, respFinalText)

	if _, err := c.Next(context.Background(), copilot.NewConversation("?")); err != nil {
		t.Fatalf("Next: %v", err)
	}

	body := string(*sent)
	if !contains(body, `"claude-opus-5"`) {
		t.Errorf("the configured model did not reach the wire:\n%s", body)
	}
	if !contains(body, `"max_tokens":8192`) {
		t.Errorf("the configured max_tokens did not reach the wire:\n%s", body)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringsIndex(haystack, needle) >= 0
}

func stringsIndex(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// A transport or API failure reaches the caller intact.
//
// The contrast with a Backend's error text is deliberate. A tool's failure text
// is swallowed because it goes back to the MODEL on the next turn, where an
// attacker-influenced string would be read as content. This error goes to the
// CALLER — Run returns it rather than appending it — so it never enters the
// conversation, and swallowing it would cost an operator the difference between
// a bad key, a rate limit and a network that is down. During an incident, that
// is the wrong thing to lose.
func TestATransportFailureReachesTheCallerIntact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()

	c, err := claude.New(claude.Config{
		Model: "claude-opus-5", MaxTokens: 8192, BaseURL: srv.URL, APIKey: "not-a-real-key",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Next(context.Background(), copilot.NewConversation("?"))
	if err == nil {
		t.Fatal("a rate-limited request came back as a successful turn")
	}
	if errors.Is(err, claude.ErrRefused) {
		t.Error("a transport failure was reported as a model refusal")
	}
	// The operator must be able to tell WHICH failure this was.
	if !contains(err.Error(), "429") && !contains(err.Error(), "rate_limit") {
		t.Errorf("the failure is unidentifiable: %v", err)
	}
}
