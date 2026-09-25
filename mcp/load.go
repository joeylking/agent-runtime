package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// namePattern is the strictest name a provider accepts, so a tool that
// matches it is callable through all of them.
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Rule is the operator's classification of one server tool. Policy is
// evaluated on a side-effect class and no server may choose its own, so a
// tool without a Rule is not registered.
type Rule struct {
	// SideEffect is required.
	SideEffect agentrt.SideEffect
	// Timeout bounds one call. It is required unless the Server names a
	// DefaultTimeout.
	Timeout time.Duration
	// Terminal completes the run with this tool's result when it succeeds.
	Terminal bool
	// Description, when set, replaces the server's description in what the
	// model sees.
	Description string
	// Deny names parameters the model may not set: they are removed from the
	// schema it is shown and a call that carries one anyway fails.
	Deny []string
	// Fixed names parameters the operator sets. They are removed from the
	// schema the model sees and injected into every call. A fixed value is
	// operator configuration rather than part of the request, so it sits
	// outside the approval hash on purpose: the approval binds what the model
	// asked for, and the operator already knows their own configuration.
	Fixed map[string]json.RawMessage
	// Rename is the name the model sees instead of "<server>_<tool>".
	Rename string
	// AllowHintMismatch registers a tool whose annotations contradict this
	// classification. The Report records the override.
	AllowHintMismatch bool
}

// Rules maps a server's own tool names to their classifications. One Rules
// belongs to one server: Load refuses a rule that names no pinned tool,
// because a misspelled name would otherwise silently drop a tool.
type Rules map[string]Rule

func (r Rules) validate(m *Manifest) error {
	var unknown []string
	for name := range r {
		if _, ok := m.Tools[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("mcp: server %q: rules name tools the manifest does not pin: %s", m.Server, strings.Join(unknown, ", "))
}

// Report is what Load decided about every tool, for an operator to read at
// startup. Anything not in Registered is unavailable to the model.
type Report struct {
	Server string `json:"server"`
	// Connection is the session the registered tools share. The consumer
	// closes it when the run is over; Load closes it itself when it returns
	// an error.
	Connection *Connection `json:"-"`
	// Registered is what the model may call.
	Registered []Registration `json:"registered,omitempty"`
	// Refused is a pinned, classified tool that failed a check, with the
	// reason.
	Refused []Refusal `json:"refused,omitempty"`
	// Unpinned tools are offered by the live server and absent from the
	// manifest; Unclassified tools are pinned and have no Rule. Both are
	// ignored.
	Unpinned     []string `json:"unpinned,omitempty"`
	Unclassified []string `json:"unclassified,omitempty"`
}

// Registration is one tool the model may call.
type Registration struct {
	// Tool is the server's own name and Name is what the model sees.
	Tool       string             `json:"tool"`
	Name       string             `json:"name"`
	SideEffect agentrt.SideEffect `json:"side_effect"`
	// HintMismatch is the contradiction AllowHintMismatch let through, if
	// any.
	HintMismatch string `json:"hint_mismatch,omitempty"`
}

// Refusal is one tool that will not be registered, and why.
type Refusal struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

// String renders the report as one line per tool, for a consumer that logs
// what it loaded.
func (r *Report) String() string {
	var b strings.Builder
	for _, reg := range r.Registered {
		fmt.Fprintf(&b, "%s: registered %s as %s (%s)", r.Server, reg.Tool, reg.Name, reg.SideEffect)
		if reg.HintMismatch != "" {
			fmt.Fprintf(&b, " [hint mismatch allowed: %s]", reg.HintMismatch)
		}
		b.WriteString("\n")
	}
	for _, ref := range r.Refused {
		fmt.Fprintf(&b, "%s: refused %s: %s\n", r.Server, ref.Tool, ref.Reason)
	}
	for _, name := range r.Unclassified {
		fmt.Fprintf(&b, "%s: unclassified %s: no rule\n", r.Server, name)
	}
	for _, name := range r.Unpinned {
		fmt.Fprintf(&b, "%s: unpinned %s: not in the manifest\n", r.Server, name)
	}
	return b.String()
}

// Load connects, lists the server's tools again, and returns an agentrt.Tool
// for every pinned tool that has a Rule and still matches its pin. A tool
// that fails a check is refused on its own and the others still register; it
// is an error for nothing to register at all. The returned tools share the
// Report's Connection, which the consumer closes after the run.
func Load(ctx context.Context, server Server, manifest *Manifest, rules Rules) ([]agentrt.Tool, *Report, error) {
	if err := server.validate(); err != nil {
		return nil, nil, err
	}
	if manifest == nil {
		return nil, nil, errors.New("mcp: manifest is required; run Pin first")
	}
	if manifest.Server != server.Name {
		return nil, nil, fmt.Errorf("mcp: manifest is for server %q, not %q", manifest.Server, server.Name)
	}
	if err := rules.validate(manifest); err != nil {
		return nil, nil, err
	}
	checker, err := newSpecChecker()
	if err != nil {
		return nil, nil, err
	}
	defer checker.close()

	conn, err := connect(ctx, server)
	if err != nil {
		return nil, nil, err
	}
	live, err := conn.listTools(ctx)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}

	rep := &Report{Server: server.Name, Connection: conn}
	for _, name := range sortedNames(live) {
		if _, ok := manifest.Tools[name]; !ok {
			rep.Unpinned = append(rep.Unpinned, name)
		}
	}
	var tools []agentrt.Tool
	taken := map[string]string{}
	for _, name := range sortedTools(manifest.Tools) {
		rule, ok := rules[name]
		if !ok {
			rep.Unclassified = append(rep.Unclassified, name)
			continue
		}
		t, reg, err := build(server, conn, manifest.Tools[name], rule, live[name], checker)
		if err != nil {
			rep.Refused = append(rep.Refused, Refusal{Tool: name, Reason: err.Error()})
			continue
		}
		if other, dup := taken[reg.Name]; dup {
			rep.Refused = append(rep.Refused, Refusal{Tool: name, Reason: fmt.Sprintf("the name %q is already taken by tool %q", reg.Name, other)})
			continue
		}
		taken[reg.Name] = name
		tools = append(tools, t)
		rep.Registered = append(rep.Registered, reg)
	}
	if len(tools) == 0 {
		conn.Close()
		rep.Connection = nil
		return nil, rep, fmt.Errorf("mcp: server %q: no tool registered", server.Name)
	}
	return tools, rep, nil
}

// build checks one pinned tool against the live server and the operator's
// rule, and returns the tool the runtime will register. Every failure is the
// reason the report gives, so the checks read in the order an operator would
// ask them.
func build(server Server, conn *Connection, pinned PinnedTool, rule Rule, live *sdk.Tool, checker *specChecker) (agentrt.Tool, Registration, error) {
	var none Registration
	if live == nil {
		return nil, none, errors.New("the server no longer offers it")
	}
	schema, err := canonical(live.InputSchema)
	if err != nil {
		return nil, none, fmt.Errorf("the server's input schema is not JSON: %v", err)
	}
	if hashBytes(schema) != pinned.SchemaHash {
		return nil, none, errors.New("the input schema changed since the pin")
	}
	if hashString(live.Description) != pinned.DescriptionHash {
		return nil, none, errors.New("the description changed since the pin, and a description is model input")
	}
	hint := mismatch(rule, pinned.Annotations)
	if hint == "" {
		if now := mismatch(rule, live.Annotations); now != "" {
			hint = "the server now says: " + now
		}
	}
	if hint != "" && !rule.AllowHintMismatch {
		return nil, none, errors.New(hint)
	}
	timeout := rule.Timeout
	if timeout <= 0 {
		timeout = server.DefaultTimeout
	}
	if timeout <= 0 {
		return nil, none, errors.New("no timeout: set Rule.Timeout or Server.DefaultTimeout")
	}
	name := rule.Rename
	if name == "" {
		name = server.Name + "_" + pinned.Name
	}
	if !namePattern.MatchString(name) {
		return nil, none, fmt.Errorf("the name %q does not match %s", name, namePattern)
	}
	if err := objectSchema(pinned.InputSchema); err != nil {
		return nil, none, err
	}
	hidden := make([]string, 0, len(rule.Deny)+len(rule.Fixed))
	hidden = append(hidden, rule.Deny...)
	for param, value := range rule.Fixed {
		if !json.Valid(value) {
			return nil, none, fmt.Errorf("the fixed value for %q is not valid JSON", param)
		}
		hidden = append(hidden, param)
	}
	restricted, err := restrict(pinned.InputSchema, hidden)
	if err != nil {
		return nil, none, fmt.Errorf("the input schema cannot be restricted: %v", err)
	}
	description := rule.Description
	if description == "" {
		description = pinned.Description
	}
	t := &tool{
		conn:       conn,
		remote:     pinned.Name,
		deny:       rule.Deny,
		fixed:      rule.Fixed,
		maxContent: server.contentCap(),
		spec: agentrt.ToolSpec{
			Name:        name,
			Description: description,
			InputSchema: restricted,
			SideEffect:  rule.SideEffect,
			Timeout:     timeout,
			Terminal:    rule.Terminal,
		},
	}
	if err := checker.accepts(t); err != nil {
		return nil, none, fmt.Errorf("the runtime refuses it: %v", err)
	}
	return t, Registration{Tool: pinned.Name, Name: name, SideEffect: rule.SideEffect, HintMismatch: hint}, nil
}

// mismatch reports how the server's annotations contradict the operator, or
// "" when they do not. Absent annotations never contradict anything: their
// defaults are the permissive ones, so silence says nothing. A hint can only
// dispute a classification, never relax it, so a server calling a tool
// read-only does not make it ReadOnly.
func mismatch(rule Rule, a *sdk.ToolAnnotations) string {
	if a == nil {
		return ""
	}
	switch {
	case rule.SideEffect == agentrt.ReadOnly && !a.ReadOnlyHint:
		return "classified read_only, but the server does not claim readOnlyHint"
	case rule.SideEffect != agentrt.Destructive && !a.ReadOnlyHint && a.DestructiveHint != nil && *a.DestructiveHint:
		return fmt.Sprintf("classified %s, but the server claims destructiveHint", rule.SideEffect)
	}
	return ""
}

// objectSchema refuses a schema that does not describe an object, because
// tool arguments are an object and a schema that says otherwise would accept
// nothing the model could send.
func objectSchema(raw json.RawMessage) error {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return errors.New("the input schema is not a JSON object")
	}
	if t, _ := doc["type"].(string); t != "object" {
		return errors.New(`the input schema is not an object schema ("type":"object")`)
	}
	return nil
}

// restrict removes the operator's denied and fixed parameters from the schema
// the model sees, from properties and from required both, so the model is
// never shown a parameter it may not set.
func restrict(schema json.RawMessage, remove []string) (json.RawMessage, error) {
	if len(remove) == 0 {
		return schema, nil
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	gone := make(map[string]bool, len(remove))
	for _, name := range remove {
		gone[name] = true
	}
	if props, ok := doc["properties"].(map[string]any); ok {
		for name := range gone {
			delete(props, name)
		}
	}
	if required, ok := doc["required"].([]any); ok {
		kept := make([]any, 0, len(required))
		for _, r := range required {
			if name, ok := r.(string); ok && gone[name] {
				continue
			}
			kept = append(kept, r)
		}
		if len(kept) == 0 {
			delete(doc, "required")
		} else {
			doc["required"] = kept
		}
	}
	return marshalCanonical(doc)
}

// specChecker asks the runtime itself whether a tool would register, so a
// server schema the driver cannot compile is refused at Load rather than at
// the first call. Building a driver is the check; a second copy of the
// runtime's rules would be a second thing to keep true.
type specChecker struct{ store *agentrt.Store }

func newSpecChecker() (*specChecker, error) {
	store, err := agentrt.OpenStore(":memory:")
	if err != nil {
		return nil, fmt.Errorf("mcp: schema check: %w", err)
	}
	return &specChecker{store: store}, nil
}

func (c *specChecker) accepts(t agentrt.Tool) error {
	_, err := agentrt.NewDriver(agentrt.Config{
		Store:  c.store,
		Agent:  unusedAgent{},
		Policy: agentrt.SideEffectPolicy{},
		Tools:  []agentrt.Tool{t},
	})
	return err
}

func (c *specChecker) close() { c.store.Close() }

// unusedAgent satisfies the driver the schema check builds and never runs.
type unusedAgent struct{}

func (unusedAgent) Decide(context.Context, agentrt.StepInput) (agentrt.Decision, error) {
	return agentrt.Decision{}, errors.New("mcp: the schema check driver never runs")
}

func sortedNames(tools map[string]*sdk.Tool) []string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedTools(tools map[string]PinnedTool) []string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
