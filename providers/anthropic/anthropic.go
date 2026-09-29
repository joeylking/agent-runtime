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
//     InputTokens at the multiple they are billed at, 1.25 for a five-minute
//     write and 2 for a one-hour write; cache reads are reported as
//     CachedInputTokens and priced at the cached rate.
//   - A reply the API served but that does not decode is an
//     agentrt.ServedError carrying whatever usage it reported.
//   - Parallel tool use is disabled in the request. Should a reply carry
//     several tool uses anyway, every one is returned; the runtime executes
//     the first and records that the others were dropped.
//
// Nothing is taken from the environment but the key. The SDK's own
// environment chain (ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN, profiles,
// federation, custom headers) is switched off, and the adapter resolves the
// key as Config.APIKey, then ANTHROPIC_API_KEY, and the endpoint as
// Config.BaseURL, then DefaultBaseURL. New fails when no key is found.
//
// No SDK error leaves the adapter: an HTTP error becomes a
// providers.StatusError built from the status and body, so the request and
// its key header are never part of an error a consumer logs. Response
// bodies are bounded by providers.MaxBodyBytes.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
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

// DefaultBaseURL is the Claude API, used when Config.BaseURL is empty.
const DefaultBaseURL = "https://api.anthropic.com/"

// Prices per million tokens in USD micros, read from the Claude API
// reference on 2026-09-29. Cache reads are a tenth of input, except on
// Claude Fable 5.1 and Claude Mythos 5.1 where they are a fortieth and on
// Claude Opus 5.5 where they are a twentieth. Cache writes have no column
// in agentrt.Usage and are folded into input at their 1.25 and 2
// multiples, so the accounting never understates a bill.
func Prices() agentrt.PriceTable {
	return agentrt.PriceTable{
		"anthropic:claude-fable-5-1":  {InputPerMTok: 10_000_000, OutputPerMTok: 50_000_000, CachedInputPerMTok: 250_000},
		"anthropic:claude-mythos-5-1": {InputPerMTok: 10_000_000, OutputPerMTok: 50_000_000, CachedInputPerMTok: 250_000},
		"anthropic:claude-fable-5":    {InputPerMTok: 10_000_000, OutputPerMTok: 50_000_000, CachedInputPerMTok: 1_000_000},
		"anthropic:claude-mythos-5":   {InputPerMTok: 10_000_000, OutputPerMTok: 50_000_000, CachedInputPerMTok: 1_000_000},
		"anthropic:claude-opus-5-5":   {InputPerMTok: 4_000_000, OutputPerMTok: 20_000_000, CachedInputPerMTok: 200_000},
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
	// Name overrides what Name() reports. The default is Name(Model),
	// "anthropic:<model>", which is what Prices is keyed by, and New
	// refuses a model Prices does not hold under it. An override exists for
	// a consumer whose recordings or price table already use another name,
	// and that consumer prices the model itself.
	Name string
	// APIKey is the credential. Empty means ANTHROPIC_API_KEY, and New
	// fails when that is empty too. No other credential source is read.
	APIKey string
	// BaseURL overrides the API endpoint, which is how the tests point the
	// adapter at a fake. Empty means DefaultBaseURL; ANTHROPIC_BASE_URL is
	// not read.
	BaseURL string
	// Effort is the output_config effort. Empty leaves the API default.
	Effort string
	// MaxTokens caps the output when a request does not. Zero means
	// DefaultMaxTokens. The adapter does not stream, so New refuses a cap
	// the SDK would refuse to send without streaming.
	MaxTokens int
	// Strict reduces each tool's schema to the subset strict tool use
	// accepts and asks for strict validation. The runtime keeps validating
	// arguments against the full schema, so no dropped keyword is lost. A
	// schema strict mode cannot express at all (oneOf, a recursive $ref, a
	// schema-valued additionalProperties) fails the request instead.
	Strict bool
	// RawAssistantBlock is the content block type that carries a provider
	// message verbatim. Empty means DefaultRawAssistantBlock.
	RawAssistantBlock string
	// HTTPClient overrides the client the SDK uses. The adapter uses a copy
	// whose transport refuses a body over providers.MaxBodyBytes. Nil means
	// a client with providers.DefaultTimeout, a backstop to the caller's
	// context.
	HTTPClient *http.Client
}

// String redacts the key, so a Config that is logged or printed does not
// leak it.
func (c Config) String() string {
	return fmt.Sprintf("anthropic.Config{Model:%q Name:%q BaseURL:%q APIKey:%s}", c.Model, c.Name, c.BaseURL, redact(c.APIKey))
}

// GoString redacts the key for %#v.
func (c Config) GoString() string { return c.String() }

func redact(key string) string {
	if key == "" {
		return `""`
	}
	return "[redacted]"
}

// Model is one Claude model.
type Model struct {
	cfg    Config
	name   string
	client sdk.Client
}

// String redacts the key, so a Model that is logged or printed does not
// leak it.
func (m Model) String() string {
	return fmt.Sprintf("anthropic.Model{Name:%q Config:%s}", m.name, m.cfg)
}

// GoString redacts the key for %#v.
func (m Model) GoString() string { return m.String() }

// New builds the adapter. It refuses an empty model id, a model Prices
// does not hold when Config.Name is unset, an output cap too large to
// serve without streaming, and a configuration with no key.
func New(cfg Config) (*Model, error) {
	if cfg.Model == "" {
		return nil, errors.New("anthropic: model is required")
	}
	m := &Model{cfg: cfg, name: cfg.Name}
	if m.name == "" {
		m.name = Name(cfg.Model)
		if _, err := providers.PriceFor(Prices(), m.name); err != nil {
			return nil, fmt.Errorf("anthropic: %w; set Config.Name to price it yourself", err)
		}
	}
	if m.cfg.MaxTokens <= 0 {
		m.cfg.MaxTokens = DefaultMaxTokens
	}
	if err := m.fitsWithoutStreaming(m.cfg.MaxTokens); err != nil {
		return nil, err
	}
	if m.cfg.RawAssistantBlock == "" {
		m.cfg.RawAssistantBlock = DefaultRawAssistantBlock
	}
	if m.cfg.APIKey == "" {
		m.cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if m.cfg.APIKey == "" {
		return nil, errors.New("anthropic: no API key: set Config.APIKey or ANTHROPIC_API_KEY")
	}
	if m.cfg.BaseURL == "" {
		m.cfg.BaseURL = DefaultBaseURL
	}
	m.client = sdk.NewClient(
		// Nothing but the key comes from the environment, and the key is
		// resolved above.
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(m.cfg.BaseURL),
		option.WithAPIKey(m.cfg.APIKey),
		option.WithHTTPClient(providers.Client(cfg.HTTPClient)),
		// Retries are the accounting caller's, so the SDK does none.
		option.WithMaxRetries(0),
	)
	return m, nil
}

// fitsWithoutStreaming refuses an output cap the SDK would refuse to send
// on a non-streaming request. The SDK refuses it before dispatch, so the
// request would fail the same way on every retry.
func (m *Model) fitsWithoutStreaming(maxTokens int) error {
	if _, err := sdk.CalculateNonStreamingTimeout(maxTokens, sdk.Model(m.cfg.Model), nil); err != nil {
		return fmt.Errorf("anthropic: max_tokens %d is too large for a non-streaming request to %s: %v", maxTokens, m.cfg.Model, err)
	}
	return nil
}

// Name is what an adapter for model reports when Config.Name is unset, so
// a consumer can key a price table or a recording directory before it
// builds the adapter.
func Name(model string) string { return "anthropic:" + model }

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
	if err := m.fitsWithoutStreaming(int(params.MaxTokens)); err != nil {
		return agentrt.ModelResponse{}, err
	}
	// The body is taken raw and decoded here, so that the usage of a reply
	// that does not decode is still read.
	var body []byte
	if _, err := m.client.Messages.New(ctx, params, option.WithResponseBodyInto(&body)); err != nil {
		return agentrt.ModelResponse{}, classify(err)
	}
	var counts struct {
		Usage sdk.Usage `json:"usage"`
	}
	json.Unmarshal(body, &counts)
	var resp sdk.Message
	if err := json.Unmarshal(body, &resp); err != nil {
		return agentrt.ModelResponse{}, agentrt.ServedError{Usage: foldUsage(counts.Usage), Err: fmt.Errorf("anthropic: decode: %w", err)}
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
// Cache writes have no counter of their own, so they are charged as input
// at the multiple they are billed at: 2 for a one-hour write and 1.25,
// rounded up, for a five-minute write. A write the response does not break
// down by duration is charged at 1.25.
func foldUsage(u sdk.Usage) agentrt.Usage {
	hour := u.CacheCreation.Ephemeral1hInputTokens
	short := u.CacheCreationInputTokens - hour
	if short < 0 {
		short = 0
	}
	return agentrt.Usage{
		InputTokens:       int(u.InputTokens + u.CacheReadInputTokens + (short*5+3)/4 + hour*2),
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
		if err := strictExpressible(schema); err != nil {
			return sdk.ToolParam{}, fmt.Errorf("anthropic: tool %s: strict mode cannot express its schema: %w", t.Name, err)
		}
		schema = StrictSubset(schema).(map[string]any)
		tp.Strict = sdk.Bool(true)
	}
	extra := map[string]any{}
	for k, v := range schema {
		switch k {
		case "type", "properties", "required":
		default:
			extra[k] = v
		}
	}
	if m.cfg.Strict {
		extra["additionalProperties"] = false
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
// additionalProperties false on every object. It walks the schema's
// structure, so a property named like a keyword ("pattern", "maximum") is
// kept, and values such as enum, const, and default are copied untouched.
// It is exported because a consumer that shows an operator the schema it
// sent needs the same transform.
func StrictSubset(v any) any {
	x, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(x))
	for k, val := range x {
		switch {
		case unsupportedInStrict[k]:
			continue
		case k == "minItems":
			if f, ok := val.(float64); ok && f > 1 {
				continue
			}
			out[k] = val
		case schemaMaps[k]:
			out[k] = mapValues(val, StrictSubset)
		case schemaLists[k]:
			out[k] = listValues(val, StrictSubset)
		case schemaValued[k]:
			out[k] = StrictSubset(val)
		default:
			out[k] = val
		}
	}
	if isObject(out["type"]) {
		out["additionalProperties"] = false
	}
	return out
}

// Keywords whose value is a map of names to schemas, a list of schemas, or
// a schema. Everything else is a value, and its keys are never keywords.
var (
	schemaMaps   = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true}
	schemaLists  = map[string]bool{"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true}
	schemaValued = map[string]bool{"items": true, "additionalProperties": true, "not": true, "if": true, "then": true, "else": true,
		"contains": true, "propertyNames": true, "additionalItems": true, "unevaluatedProperties": true, "unevaluatedItems": true}
)

func mapValues(v any, f func(any) any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for name, s := range m {
		out[name] = f(s)
	}
	return out
}

func listValues(v any, f func(any) any) any {
	l, ok := v.([]any)
	if !ok {
		return f(v)
	}
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = f(s)
	}
	return out
}

// isObject reports whether a type keyword admits an object, alone or in a
// list such as ["object", "null"].
func isObject(t any) bool {
	switch x := t.(type) {
	case string:
		return x == "object"
	case []any:
		for _, e := range x {
			if e == "object" {
				return true
			}
		}
	}
	return false
}

// strictExpressible refuses a schema strict tool use cannot express, which
// the API would reject or which reducing would silently narrow: oneOf, a
// schema-valued additionalProperties, and a $ref that recurses.
func strictExpressible(schema map[string]any) error {
	var walk func(v any, at string) error
	walk = func(v any, at string) error {
		x, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		for k, val := range x {
			switch {
			case k == "oneOf":
				return fmt.Errorf("oneOf at %s", at)
			case k == "additionalProperties":
				if _, isBool := val.(bool); !isBool {
					return fmt.Errorf("schema-valued additionalProperties at %s", at)
				}
			case schemaMaps[k]:
				if m, ok := val.(map[string]any); ok {
					for name, s := range m {
						if err := walk(s, at+"/"+k+"/"+name); err != nil {
							return err
						}
					}
				}
			case schemaLists[k]:
				if l, ok := val.([]any); ok {
					for i, s := range l {
						if err := walk(s, fmt.Sprintf("%s/%s/%d", at, k, i)); err != nil {
							return err
						}
					}
				}
			case schemaValued[k]:
				if err := walk(val, at+"/"+k); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(schema, "#"); err != nil {
		return err
	}
	return recursiveRef(schema)
}

// recursiveRef refuses a $ref that leads back to itself, directly or
// through other definitions, or to the root.
func recursiveRef(schema map[string]any) error {
	defs := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		if m, ok := schema[key].(map[string]any); ok {
			for name, s := range m {
				defs["#/"+key+"/"+name] = s
			}
		}
	}
	refs := func(v any) []string {
		var out []string
		var collect func(any)
		collect = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, val := range x {
					if s, ok := val.(string); ok && k == "$ref" {
						out = append(out, s)
					} else {
						collect(val)
					}
				}
			case []any:
				for _, e := range x {
					collect(e)
				}
			}
		}
		collect(v)
		return out
	}
	const visiting, done = 1, 2
	state := map[string]int{}
	var visit func(ref string) error
	visit = func(ref string) error {
		if ref == "#" {
			return errors.New("a $ref to the root schema")
		}
		switch state[ref] {
		case visiting:
			return fmt.Errorf("a recursive $ref through %s", ref)
		case done:
			return nil
		}
		state[ref] = visiting
		for _, next := range refs(defs[ref]) {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[ref] = done
		return nil
	}
	for _, ref := range refs(schema) {
		if err := visit(ref); err != nil {
			return err
		}
	}
	return nil
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
			// The input is passed through as it was recorded: decoding it
			// would round an integer beyond float64 precision.
			input := orEmptyObject(c.Input)
			if !json.Valid(input) {
				return sdk.MessageParam{}, fmt.Errorf("anthropic: tool use %s: input is not JSON", c.Name)
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

// classify maps an SDK error onto the runtime's retry classes. An HTTP
// error becomes a providers.StatusError built from the status, the body,
// and any Retry-After header, so the SDK's error, which holds the request
// and its key header, never escapes. Anything else is a transport failure.
func classify(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		header := http.Header{}
		if apiErr.Response != nil {
			header = apiErr.Response.Header
		}
		return providers.ClassifyResponse("anthropic", &http.Response{StatusCode: apiErr.StatusCode, Header: header}, []byte(apiErr.RawJSON()))
	}
	return providers.ClassifyTransport(err)
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
