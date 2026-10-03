package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// PolicyCase is one row of a conformance table: a request, the run state
// the policy sees, and the outcome it must give.
type PolicyCase struct {
	Name string
	// Tool is a registered tool's name and Args its JSON arguments; empty
	// Args mean {}. Both go through the runtime's validation first, as a
	// decision does, and the policy sees the ToolRequest the runtime built.
	Tool string
	Args string
	// View is the run state handed to the policy. Its Run.ID, when set,
	// is also the request's.
	View agentrt.RunView
	// Want is the outcome, or Refused when the runtime must refuse the
	// arguments before the policy sees them.
	Want agentrt.PolicyOutcome
	// Kind, when set, is the approval kind a require_approval must carry.
	// Reason, Capability, and Presentation, when set, are substrings the
	// decision's reason, capability JSON, and presentation JSON must
	// contain.
	Kind         string
	Reason       string
	Capability   string
	Presentation string
}

// Refused is the Want of a PolicyCase whose arguments the runtime refuses
// before policy: an unknown tool, arguments that fail the tool's schema, or
// JSON the boundary rejects. It is the kit's value, never a policy's.
const Refused agentrt.PolicyOutcome = "refused"

// CheckPolicy evaluates every case against p and reports the ones that
// disagree as one table, with what the policy answered and why. Validation
// is the runtime's own, agentrt.CompileTool and its Check, and a case the
// runtime refuses never reaches p.
func CheckPolicy(t testing.TB, p agentrt.Policy, tools []agentrt.Tool, cases []PolicyCase) {
	t.Helper()
	probes := make([]probe, len(cases))
	for i, c := range cases {
		probes[i] = probe{tool: c.Tool, args: json.RawMessage(c.Args)}
	}
	validated := validate(t, tools, probes)
	ctx := context.Background()
	var rows []string
	for i, c := range cases {
		got, detail := Refused, validated[i].refusal
		var d agentrt.PolicyDecision
		if req := validated[i].req; req != nil {
			if c.View.Run.ID != "" {
				req.RunID = c.View.Run.ID
			}
			var err error
			if d, err = p.Evaluate(ctx, *req, c.View); err != nil {
				got, detail = "error", err.Error()
			} else {
				got, detail = d.Outcome, d.Reason
			}
		}
		switch {
		case got != c.Want:
		case c.Kind != "" && d.Kind != c.Kind:
			detail = "kind " + d.Kind + "; " + detail
		case c.Reason != "" && !strings.Contains(detail, c.Reason):
			detail = "reason lacks " + quoted(c.Reason) + "; " + detail
		case c.Capability != "" && !strings.Contains(string(d.Capability), c.Capability):
			detail = "capability lacks " + quoted(c.Capability) + ": " + string(d.Capability)
		case c.Presentation != "" && !strings.Contains(string(d.Presentation), c.Presentation):
			detail = "presentation lacks " + quoted(c.Presentation) + ": " + string(d.Presentation)
		default:
			continue
		}
		want := string(c.Want)
		if c.Kind != "" {
			want += " kind " + c.Kind
		}
		detail = strings.Join(strings.Fields(detail), " ")
		rows = append(rows, fmt.Sprintf("%s\t%s %s\t%s\t%s\t%s", c.Name, c.Tool, orEmpty(c.Args), want, got, detail))
	}
	if len(rows) == 0 {
		return
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "case\trequest\twant\tgot\tdetail")
	for _, r := range rows {
		fmt.Fprintln(w, r)
	}
	w.Flush()
	t.Errorf("testkit: %d of %d policy cases disagree:\n%s", len(rows), len(cases), b.String())
}

// Never asserts that p never answers outcome for a tool whose side effect
// is class, over f.Count generated arguments per such tool that pass the
// runtime's validation, in each of views (the empty view when none are
// given). A class with no tool fails: the assertion would hold of nothing.
// A consumer that has no tool of the class, and asserts that its policy
// never allows one, adds StandIn(name, class) to tools: a tool the policy
// has never heard of, which it must answer for all the same.
func Never(t testing.TB, p agentrt.Policy, tools []agentrt.Tool, class agentrt.SideEffect, outcome agentrt.PolicyOutcome, f Fuzz, views ...agentrt.RunView) {
	t.Helper()
	if len(views) == 0 {
		views = []agentrt.RunView{{}}
	}
	ctx := context.Background()
	found := false
	for _, tool := range tools {
		spec := tool.Spec()
		if spec.SideEffect != class {
			continue
		}
		found = true
		args, err := f.Valid(spec.InputSchema)
		if err != nil {
			t.Errorf("testkit: %s: %v", spec.Name, err)
			continue
		}
		probes := make([]probe, len(args))
		for i, a := range args {
			probes[i] = probe{tool: spec.Name, args: a}
		}
		for i, v := range validate(t, tools, probes) {
			if v.req == nil {
				t.Errorf("testkit: %s: generated arguments %s were refused by the runtime: %s", spec.Name, args[i], v.refusal)
				continue
			}
			for j, view := range views {
				d, err := p.Evaluate(ctx, *v.req, view)
				if err != nil {
					t.Errorf("testkit: %s %s (view %d): policy error: %v", spec.Name, args[i], j, err)
					continue
				}
				if d.Outcome == outcome {
					t.Errorf("testkit: %s %s (view %d): %s is %s, which a %s tool must never be: %s", spec.Name, args[i], j, spec.Name, outcome, class, d.Reason)
				}
			}
		}
	}
	if !found {
		t.Errorf("testkit: no tool has side effect %s", class)
	}
}

// StandIn is a tool of side effect class for a tool set that has none,
// as Never needs: its schema is any JSON object, and a call returns the
// fixed result {"stand_in":name} without doing anything. In a Scenario its
// calls are recorded in Result.Calls, as every tool's are.
func StandIn(name string, class agentrt.SideEffect) agentrt.Tool {
	return standIn{agentrt.ToolSpec{Name: name, Description: "stand-in " + string(class) + " tool " + name + "; it does nothing", InputSchema: []byte(`{"type":"object"}`), SideEffect: class, Timeout: time.Second}}
}

type standIn struct{ spec agentrt.ToolSpec }

func (s standIn) Spec() agentrt.ToolSpec { return s.spec }

func (s standIn) Call(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
	content, err := json.Marshal(map[string]string{"stand_in": s.spec.Name})
	return agentrt.ToolResult{Content: content, Summary: "stand-in " + s.spec.Name}, err
}

// probe is one request to put through the runtime's validation.
type probe struct {
	tool string
	args json.RawMessage
}

// validated is what the runtime made of a probe: the request the policy
// would see, or why the arguments were refused before policy.
type validated struct {
	req     *agentrt.ToolRequest
	refusal string
}

// validate puts every probe through the runtime's own acceptance of a
// tool call, agentrt.CompileTool's check of each tool as NewDriver makes
// it and the compiled schema's Check, and builds the request the loop
// would hand the policy for each one it accepts. The request's ids are
// stand-ins: a case's View.Run.ID replaces the run's.
func validate(t testing.TB, tools []agentrt.Tool, probes []probe) []validated {
	t.Helper()
	out := make([]validated, len(probes))
	if len(probes) == 0 {
		return out
	}
	known := compileTools(t, tools)
	for i, p := range probes {
		switch k, ok := known[p.tool]; {
		case p.tool == "":
			out[i].refusal = "tool_call without a tool name"
		case !ok:
			out[i].refusal = fmt.Sprintf("unknown tool %q", p.tool)
		default:
			if err := k.schema.Check(p.args); err != nil {
				out[i].refusal = err.Error()
				continue
			}
			args := p.args
			if len(bytes.TrimSpace(args)) == 0 {
				args = json.RawMessage("{}")
			}
			out[i].req = &agentrt.ToolRequest{RunID: "testkit-run", StepID: fmt.Sprintf("testkit-step-%d", i), Spec: k.spec, Args: args}
		}
	}
	return out
}

// compiled is a registered tool's spec and its compiled schema.
type compiled struct {
	spec   agentrt.ToolSpec
	schema *agentrt.ToolSchema
}

// compileTools checks tools as NewDriver does and fails the test with
// NewDriver's error for a set it would refuse.
func compileTools(t testing.TB, tools []agentrt.Tool) map[string]compiled {
	t.Helper()
	out := make(map[string]compiled, len(tools))
	for _, tool := range tools {
		spec := tool.Spec()
		if _, dup := out[spec.Name]; dup && spec.Name != "" {
			t.Fatalf("testkit: agentrt: duplicate tool %q", spec.Name)
		}
		ts, err := agentrt.CompileTool(spec)
		if err != nil {
			t.Fatalf("testkit: %v", err)
		}
		out[spec.Name] = compiled{spec: spec, schema: ts}
	}
	return out
}

func orEmpty(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

func quoted(s string) string { return fmt.Sprintf("%q", s) }
