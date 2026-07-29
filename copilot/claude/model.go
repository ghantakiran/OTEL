package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/ghantakiran/OTEL/copilot"
)

// Config is what a Client needs before it can run.
//
// Model and MaxTokens are required and refused here rather than at the API. Both
// produce a request the API rejects, and its 400 names neither — so a missing one
// is caught at construction, where the error can say which.
type Config struct {
	// Model is the Claude model ID. Settled in #55 and recorded in CONTEXT.md
	// under Copilot Model: `claude-opus-5` for incident summaries and deep
	// root-cause reasoning.
	//
	// There is deliberately no default. A default here is how a superseded ID
	// survives a model generation — which is the drift #55 was filed for.
	Model string

	// MaxTokens caps the response. It must leave room for THINKING AS WELL AS
	// TEXT: thinking is on by default on this model and both are billed against
	// this one budget, so a limit sized around the answer alone truncates
	// mid-response (ADR 0011, amendment).
	MaxTokens int64

	// BaseURL overrides the API host. Empty means the SDK's default; a test
	// points it at an httptest server.
	BaseURL string

	// APIKey is the credential. Empty lets the SDK resolve one from the
	// environment, which is what a deployment wants and a test never relies on.
	APIKey string

	// Tools is the surface the model is shown. Built in `copilot` with no vendor
	// in it; this package only renders it.
	Tools []copilot.ToolSchema
}

// The two construction failures, named so a caller can tell them apart from each
// other and from a transport error.
var (
	ErrNoModel     = errors.New("copilot/claude: a model ID is required (see CONTEXT.md, Copilot Model)")
	ErrNoMaxTokens = errors.New("copilot/claude: a positive MaxTokens is required; it caps thinking and response text together")
)

// ErrRefused reports that the model's safety classifiers declined the request.
//
// It is an error rather than an empty turn on purpose. A refusal arrives as a
// successful HTTP 200 with no content, so a loop that read it as an ordinary turn
// would simply stop — and "the Copilot had nothing to say" is a different and
// much worse claim than "the request was declined". An incident responder needs
// to be able to tell those apart.
var ErrRefused = errors.New("copilot/claude: the model declined the request")

// Client calls the Messages API. It satisfies copilot.Model, which is the only
// thing the loop above knows about it.
type Client struct {
	api       anthropic.Client
	model     string
	maxTokens int64
	tools     []copilot.ToolSchema
}

var _ copilot.Model = (*Client)(nil)

// New builds a Client, refusing an unusable configuration up front.
func New(cfg Config) (*Client, error) {
	if cfg.Model == "" {
		return nil, ErrNoModel
	}
	if cfg.MaxTokens <= 0 {
		return nil, ErrNoMaxTokens
	}

	var opts []option.RequestOption
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}

	return &Client{
		api:       anthropic.NewClient(opts...),
		model:     cfg.Model,
		maxTokens: cfg.MaxTokens,
		tools:     cfg.Tools,
	}, nil
}

// Next is one model turn.
//
// NOTHING HERE SETS A SAMPLING PARAMETER. `temperature`, `top_p` and `top_k` are
// rejected with a 400 on this model, and the resulting error would name the
// parameter but not the reason. Behaviour is steered by the system prompt, which
// is a constant in `copilot` (ADR 0011, amendment).
func (c *Client) Next(ctx context.Context, conv *copilot.Conversation) (copilot.Assistant, error) {
	req := Serialize(conv, c.tools)
	req.Model = anthropic.Model(c.model)
	req.MaxTokens = c.maxTokens

	resp, err := c.api.Messages.New(ctx, req)
	if err != nil {
		// THE ERROR IS PASSED THROUGH, and the contrast with a Backend's error
		// text is the point. A tool's failure text is swallowed because it goes
		// back to the MODEL on the next turn, where an attacker-influenced string
		// would be read as content. This error goes to the CALLER — Run returns
		// it rather than appending it, so it never enters the conversation at all.
		//
		// Swallowing it would cost an operator the difference between a bad key,
		// a rate limit, and a network that is down, during an incident. That is
		// the wrong trade for a tool whose job is telling failures apart.
		return copilot.Assistant{}, fmt.Errorf("copilot/claude: the model could not be reached: %w", err)
	}

	// CHECKED BEFORE CONTENT IS READ. On a refusal `content` is empty, so
	// anything that indexes it breaks here rather than reporting the refusal.
	if resp.StopReason == anthropic.StopReasonRefusal {
		return copilot.Assistant{}, ErrRefused
	}

	return assistantFrom(resp), nil
}

// assistantFrom maps a response back into the loop's vocabulary.
//
// Only two block types mean anything above this seam: text is the answer, and
// tool_use is the request to act. A thinking block is neither — it carries no
// content on this model (`display` defaults to omitted) and must not be mistaken
// for the answer. Anything else is ignored rather than guessed at.
func assistantFrom(resp *anthropic.Message) copilot.Assistant {
	var out copilot.Assistant

	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			// Concatenated rather than overwritten: a turn may carry several text
			// blocks, and keeping only the last would silently drop the answer.
			if out.Text != "" {
				out.Text += "\n"
			}
			out.Text += b.Text

		case anthropic.ToolUseBlock:
			// The raw JSON, not a re-marshalled map. The tool layer unmarshals it
			// into a typed query; round-tripping through `any` would reorder keys
			// and turn every number into a float.
			out.Calls = append(out.Calls, copilot.ToolUse{
				ID:    b.ID,
				Name:  b.Name,
				Input: json.RawMessage(b.JSON.Input.Raw()),
			})
		}
	}
	return out
}
