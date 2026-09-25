// Package anthropic adapts the Claude Messages API to agentrt.Model through
// the official SDK. It is the paid provider: a consumer runs it with the
// price table below and a cost limit, and nothing in this repository's tests
// or CI reaches the API.
//
// The contract beyond the shared one:
//
//   - The SDK's own retries are off, so the runtime's accounting caller sees
//     and records every attempt.
//   - A response that ends in refusal or max_tokens is returned with its
//     usage and without its tool uses, so a partial call can never execute
//     and the attempt is still charged as the provider charged it.
//   - An assistant turn carried as one RawAssistantBlock is replayed from
//     the provider's own message, so thinking blocks and their signatures
//     survive a continuation.
//   - Cache writes have no column in agentrt.Usage, so they are folded into
//     InputTokens at the 1.25 multiple they are billed at; cache reads are
//     reported as CachedInputTokens and priced at the cached rate.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
)

// DefaultRawAssistantBlock is the content block type an assistant turn uses
// to carry a provider message verbatim. A consumer whose recordings already
// name it something else sets Config.RawAssistantBlock instead, because the
// type appears in every rendered message.
const DefaultRawAssistantBlock = "anthropic.raw_assistant"

// DefaultMaxTokens caps the output when neither the request nor the config
// says otherwise. The API requires a value.
const DefaultMaxTokens = 4096

// Prices per million tokens in USD micros, read from the Claude API
// reference on 2026-09-25. Cache reads are a tenth of input, except on
// Claude Fable 5.1 where they are a fortieth. Cache writes have no column
// in agentrt.Usage and are folded into input at their 1.25 multiple, so the
// accounting never understates a bill.
func Prices() agentrt.PriceTable {
	return agentrt.PriceTable{
		"anthropic:claude-fable-5-1":  {InputPerMTok: 10_000_000, OutputPerMTok: 50_000_000, CachedInputPerMTok: 250_000},
		"anthropic:claude-fable-5":    {InputPerMTok: 10_000_000, OutputPerMTok: 50_000_000, CachedInputPerMTok: 1_000_000},
		"anthropic:claude-opus-5":     {InputPerMTok: 5_000_000, OutputPerMTok: 25_000_000, CachedInputPerMTok: 500_000},
		"anthropic:claude-opus-4-8":   {InputPerMTok: 5_000_000, OutputPerMTok: 25_000_000, CachedInputPerMTok: 500_000},
		"anthropic:claude-opus-4-7":   {InputPerMTok: 5_000_000, OutputPerMTok: 25_000_000, CachedInputPerMTok: 500_000},
		"anthropic:claude-opus-4-6":   {InputPerMTok: 5_000_000, OutputPerMTok: 25_000_000, CachedInputPerMTok: 500_000},
		"anthropic:claude-sonnet-5":   {InputPerMTok: 2_000_000, OutputPerMTok: 10_000_000, CachedInputPerMTok: 200_000},
		"anthropic:claude-sonnet-4-6": {InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000, CachedInputPerMTok: 300_000},
		"anthropic:claude-haiku-4-5":  {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000, CachedInputPerMTok: 100_000},
	}
}

// Config configures the adapter.
type Config struct {
	// Model is the Claude API model id, for example claude-opus-5. It is
	// required.
	Model string
	// Name overrides what Name() reports. The default is
	// "anthropic:<model>", which is what Prices is keyed by; an override
	// exists for a consumer whose recordings or price table already use
	// another name.
	Name string
	// APIKey overrides the environment credential. Empty leaves the SDK to
	// read ANTHROPIC_API_KEY at the first request, so an adapter with no
	// key anywhere constructs fine and fails when first called; a consumer
	// that wants to refuse earlier checks the environment itself.
	APIKey string
	// BaseURL overrides the API endpoint, which is how the tests point the
	// adapter at a fake.
	BaseURL string
	// Effort is the output_config effort. Empty leaves the API default.
	Effort string
	// MaxTokens caps the output when a request does not. Zero means
	// DefaultMaxTokens.
	MaxTokens int
	// Strict reduces each tool's schema to the subset strict tool use
	// accepts and asks for strict validation. The runtime keeps validating
	// arguments against the full schema, so no constraint is lost.
	Strict bool
	// RawAssistantBlock is the content block type that carries a provider
	// message verbatim. Empty means DefaultRawAssistantBlock.
	RawAssistantBlock string
	// HTTPClient overrides the client the SDK uses.
	HTTPClient *http.Client
}

// Model is one Claude model.
type Model struct {
	cfg    Config
	name   string
	client sdk.Client
}

// New builds the adapter. It refuses an empty model id.
func New(cfg Config) (*Model, error) {
	if cfg.Model == "" {
		return nil, errors.New("anthropic: model is required")
	}
	m := &Model{cfg: cfg, name: cfg.Name}
	if m.name == "" {
		m.name = "anthropic:" + cfg.Model
	}
	if m.cfg.MaxTokens <= 0 {
		m.cfg.MaxTokens = DefaultMaxTokens
	}
	if m.cfg.RawAssistantBlock == "" {
		m.cfg.RawAssistantBlock = DefaultRawAssistantBlock
	}
	// Retries are the accounting caller's, so the SDK does none.
	opts := []option.RequestOption{option.WithMaxRetries(0)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	m.client = sdk.NewClient(opts...)
	return m, nil
}

// Name implements agentrt.Model.
func (m *Model) Name() string { return m.name }

// RawAssistantBlock is the content block type this adapter replays
// verbatim. A consumer's renderer needs it to build such a turn.
func (m *Model) RawAssistantBlock() string { return m.cfg.RawAssistantBlock }

// Generate implements agentrt.Model.
func (m *Model) Generate(ctx context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	params, err := m.params(req)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	resp, err := m.client.Messages.New(ctx, params)
	if err != nil {
		return agentrt.ModelResponse{}, classify(err)
	}
	out := agentrt.ModelResponse{
		StopReason: string(resp.StopReason),
		Usage:      foldUsage(resp.Usage),
		Raw:        json.RawMessage(resp.RawJSON()),
	}
	var text strings.Builder
	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case sdk.TextBlock:
			if v.Text == "" {
				continue
			}
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(v.Text)
		case sdk.ToolUseBlock:
			out.ToolUses = append(out.ToolUses, agentrt.ToolUse{ID: v.ID, Name: v.Name, Args: orEmptyObject(json.RawMessage(v.JSON.Input.Raw()))})
		}
	}
	out.Text = text.String()
	// A truncated or refused turn may carry a partial tool use; it must
	// never execute. The usage stays: the provider served and charged it.
	if resp.StopReason == sdk.StopReasonMaxTokens || resp.StopReason == sdk.StopReasonRefusal {
		out.ToolUses = nil
	}
	return out, nil
}

// foldUsage maps the provider's usage onto the runtime's three counters.
// Cache writes are billed at 1.25 times base input and have no counter of
// their own, so they are charged as input at that multiple, rounded up.
func foldUsage(u sdk.Usage) agentrt.Usage {
	return agentrt.Usage{
		InputTokens:       int(u.InputTokens + u.CacheReadInputTokens + (u.CacheCreationInputTokens*5+3)/4),
		CachedInputTokens: int(u.CacheReadInputTokens),
		OutputTokens:      int(u.OutputTokens),
	}
}

func (m *Model) params(req agentrt.ModelRequest) (sdk.MessageNewParams, error) {
	p := sdk.MessageNewParams{Model: sdk.Model(m.cfg.Model), MaxTokens: int64(req.MaxOutputTokens)}
	if p.MaxTokens <= 0 {
		p.MaxTokens = int64(m.cfg.MaxTokens)
	}
	if req.System != "" {
		p.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	if m.cfg.Effort != "" {
		p.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(m.cfg.Effort)}
	}
	if len(req.Tools) > 0 {
		// The runtime executes one call per step, so parallel calls are
		// refused rather than discarded after the fact.
		p.ToolChoice = sdk.ToolChoiceUnionParam{OfAuto: &sdk.ToolChoiceAutoParam{DisableParallelToolUse: sdk.Bool(true)}}
	}
	for _, t := range req.Tools {
		tp, err := m.toolParam(t)
		if err != nil {
			return p, err
		}
		p.Tools = append(p.Tools, sdk.ToolUnionParam{OfTool: &tp})
	}
	for _, msg := range req.Messages {
		mp, err := m.messageParam(msg)
		if err != nil {
			return p, err
		}
		p.Messages = append(p.Messages, mp)
	}
	return p, nil
}

// toolParam maps a runtime tool spec onto a tool definition. Under Strict
// the provider's copy is reduced to the strict-mode subset; otherwise the
// schema is passed through whole.
func (m *Model) toolParam(t agentrt.ToolSpec) (sdk.ToolParam, error) {
	var schema map[string]any
	if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
		return sdk.ToolParam{}, fmt.Errorf("anthropic: tool %s: schema: %w", t.Name, err)
	}
	tp := sdk.ToolParam{Name: t.Name, Description: sdk.String(t.Description)}
	if m.cfg.Strict {
		schema = StrictSubset(schema).(map[string]any)
		tp.Strict = sdk.Bool(true)
		tp.InputSchema = inputSchema(schema, map[string]any{"additionalProperties": false})
		return tp, nil
	}
	extra := map[string]any{}
	for k, v := range schema {
		switch k {
		case "type", "properties", "required":
		default:
			extra[k] = v
		}
	}
	tp.InputSchema = inputSchema(schema, extra)
	return tp, nil
}

// inputSchema splits a decoded schema into the fields the SDK models and
// the extras it passes through.
func inputSchema(schema, extra map[string]any) sdk.ToolInputSchemaParam {
	in := sdk.ToolInputSchemaParam{Properties: map[string]any{}}
	if len(extra) > 0 {
		in.ExtraFields = extra
	}
	if props, ok := schema["properties"]; ok {
		in.Properties = props
	}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				in.Required = append(in.Required, s)
			}
		}
	}
	return in
}

// unsupportedInStrict are the JSON Schema keywords strict tool use rejects
// with a 400, per the structured-outputs limitations page (read 2026-09-21):
// numeric bounds, string length bounds, maxItems, pattern, uniqueItems.
var unsupportedInStrict = map[string]bool{"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true, "minLength": true, "maxLength": true, "maxItems": true, "pattern": true, "uniqueItems": true}

// StrictSubset returns a copy of a schema reduced to what strict tool use
// accepts: unsupported keywords removed, minItems clamped to 0 or 1, and
// additionalProperties false on every object. It is exported because a
// consumer that shows an operator the schema it sent needs the same
// transform.
func StrictSubset(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if unsupportedInStrict[k] {
				continue
			}
			if k == "minItems" {
				if f, ok := val.(float64); ok && f > 1 {
					continue
				}
			}
			out[k] = StrictSubset(val)
		}
		if out["type"] == "object" {
			out["additionalProperties"] = false
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = StrictSubset(val)
		}
		return out
	default:
		return v
	}
}

func (m *Model) messageParam(msg agentrt.Message) (sdk.MessageParam, error) {
	// A raw assistant turn is replayed exactly as the provider produced it.
	if msg.Role == "assistant" && len(msg.Content) == 1 && msg.Content[0].Type == m.cfg.RawAssistantBlock {
		var raw sdk.Message
		if err := json.Unmarshal([]byte(msg.Content[0].Text), &raw); err != nil {
			return sdk.MessageParam{}, fmt.Errorf("anthropic: raw assistant turn: %w", err)
		}
		return raw.ToParam(), nil
	}
	var blocks []sdk.ContentBlockParamUnion
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			// The API rejects an empty text block, which a renderer's
			// nudge can produce when the model returned nothing at all.
			if strings.TrimSpace(c.Text) == "" {
				continue
			}
			blocks = append(blocks, sdk.NewTextBlock(c.Text))
		case "tool_use":
			var input any
			if err := json.Unmarshal(orEmptyObject(c.Input), &input); err != nil {
				return sdk.MessageParam{}, fmt.Errorf("anthropic: tool use %s: input: %w", c.Name, err)
			}
			blocks = append(blocks, sdk.NewToolUseBlock(c.ToolUseID, input, c.Name))
		case "tool_result":
			blocks = append(blocks, sdk.NewToolResultBlock(c.ToolUseID, c.Content, c.IsError))
		default:
			return sdk.MessageParam{}, fmt.Errorf("anthropic: unsupported content block type %q", c.Type)
		}
	}
	if len(blocks) == 0 {
		// A message must carry at least one block.
		blocks = []sdk.ContentBlockParamUnion{sdk.NewTextBlock("(no output)")}
	}
	if msg.Role == "assistant" {
		return sdk.NewAssistantMessage(blocks...), nil
	}
	return sdk.NewUserMessage(blocks...), nil
}

// classify maps an SDK error onto the runtime's retry classes. A status of
// zero means the SDK reported no HTTP response, which is a transport
// failure.
func classify(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		return providers.ClassifyAPIError(apiErr.StatusCode, err)
	}
	return providers.ClassifyAPIError(0, err)
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
