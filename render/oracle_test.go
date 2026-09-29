package render_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/render"
)

// oracleMessages is Messages as it was before its allocations were cut,
// kept verbatim as the reference the current one must match byte for byte.
func oracleMessages(in agentrt.StepInput, opts render.Options) []agentrt.Message {
	if opts.Opening == nil {
		panic("render: Options.Opening is required")
	}
	recent := opts.RecentResults
	if recent <= 0 {
		recent = render.DefaultRecentResults
	}
	maxContent := opts.MaxContentBytes
	if maxContent <= 0 {
		maxContent = render.DefaultMaxContentBytes
	}
	toolUseID := func(st agentrt.Step) string {
		if opts.ToolUseID != nil {
			return opts.ToolUseID(st)
		}
		return fmt.Sprintf("step_%d", st.Index)
	}
	msgs := []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: opts.Opening(in)}}}}
	n := 0
	for _, st := range in.Steps {
		if st.Decision != nil {
			n++
		}
	}
	i := 0
	for _, st := range in.Steps {
		if st.Decision == nil {
			continue
		}
		full := i >= n-recent
		i++
		id := toolUseID(st)
		blocks := oracleSynthesize(st, id)
		if opts.Assistant != nil {
			if custom := opts.Assistant(st, id); custom != nil {
				blocks = custom
			}
		}
		block := oracleDefaultObservation(st, id, full, maxContent)
		if opts.Observation != nil {
			if custom := opts.Observation(st, id, full); custom.Type != "" {
				block = custom
			}
		}
		msgs = append(msgs,
			agentrt.Message{Role: "assistant", Content: blocks},
			agentrt.Message{Role: "user", Content: []agentrt.ContentBlock{block}})
	}
	return msgs
}

func oracleSynthesize(st agentrt.Step, toolUseID string) []agentrt.ContentBlock {
	d := st.Decision
	if d.Kind != agentrt.DecideToolCall || d.Tool == "" {
		text := d.Reason
		if text == "" {
			text = "(no tool call)"
		}
		return []agentrt.ContentBlock{{Type: "text", Text: text}}
	}
	var blocks []agentrt.ContentBlock
	if d.Reason != "" {
		blocks = append(blocks, agentrt.ContentBlock{Type: "text", Text: d.Reason})
	}
	return append(blocks, agentrt.ContentBlock{Type: "tool_use", ToolUseID: toolUseID, Name: d.Tool, Input: oracleOrEmptyObject(d.Args)})
}

func oracleDefaultObservation(st agentrt.Step, toolUseID string, full bool, maxBytes int) agentrt.ContentBlock {
	if st.Decision == nil || st.Decision.Kind != agentrt.DecideToolCall || st.Decision.Tool == "" || st.Observation == nil {
		return agentrt.ContentBlock{Type: "text", Text: oracleNudge(st)}
	}
	o := st.Observation
	content := string(o.Content)
	if !full || len(content) > maxBytes {
		content = "[summary] " + o.Summary
	}
	if o.Kind == agentrt.ObserveInterrupted {
		content = render.InterruptedText
	}
	return agentrt.ContentBlock{Type: "tool_result", ToolUseID: toolUseID, Content: content, IsError: o.Failure()}
}

func oracleNudge(st agentrt.Step) string {
	var kind agentrt.DecisionKind
	if st.Decision != nil {
		kind = st.Decision.Kind
	}
	switch kind {
	case agentrt.KindNoToolCall:
		return "Your reply contained no executable tool call. Respond with exactly one tool call."
	case agentrt.KindTruncated:
		return "Your reply hit the output token cap before the tool call was complete, so nothing ran. Respond with exactly one short tool call."
	}
	msg := "Your reply was not an executable tool call"
	if st.Observation != nil {
		msg += " (" + st.Observation.Summary + ")"
	}
	return msg + ". Respond with exactly one valid tool call."
}

func oracleOrEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

// randomStep draws a step from every shape the renderer distinguishes:
// no decision, each decision kind, a tool call with or without a tool, a
// reason or none, arguments absent, blank, or JSON, every observation kind
// or none, and content on either side of the byte cap, some of it not
// UTF-8.
func randomStep(r *rand.Rand, index, maxBytes int) agentrt.Step {
	st := agentrt.Step{ID: fmt.Sprintf("s%d", index), Index: index, Status: agentrt.StepDone}
	pick := func(xs ...string) string { return xs[r.IntN(len(xs))] }
	if r.IntN(8) > 0 {
		d := &agentrt.Decision{
			Kind:   agentrt.DecisionKind(pick("tool_call", "tool_call", "tool_call", "complete", "fail", "no_tool_call", "truncated", "", "weird")),
			Tool:   pick("read", "write", ""),
			Reason: pick("", "look", "<b>&amp; é</b>"),
		}
		switch r.IntN(4) {
		case 1:
			d.Args = json.RawMessage("  ")
		case 2:
			d.Args = json.RawMessage(`{"n":1}`)
		case 3:
			d.Args = json.RawMessage{}
		}
		st.Decision = d
	}
	if r.IntN(5) > 0 {
		size := maxBytes + r.IntN(5) - 2
		if r.IntN(2) == 0 {
			size = r.IntN(40)
		}
		content := make([]byte, max(size, 0))
		for i := range content {
			content[i] = "ab\"é\xff{}"[r.IntN(8)]
		}
		st.Observation = &agentrt.Observation{
			Kind:    agentrt.ObservationKind(pick("tool_result", "tool_error", "policy_denied", "invalid_decision", "interrupted")),
			Content: content,
			Summary: pick("", "summary", "ünïcode"),
		}
	}
	return st
}

// Messages renders exactly what it rendered before, over a corpus of runs
// with every step shape, window, cap, and hook combination.
func TestMessages_MatchesOracle(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for c := range 3000 {
		opts := render.Options{Opening: fixedOpening("Goal <g>"), RecentResults: r.IntN(9) - 1, MaxContentBytes: []int{0, -1, 5, 16, 30}[r.IntN(5)]}
		maxBytes := opts.MaxContentBytes
		if maxBytes <= 0 {
			maxBytes = render.DefaultMaxContentBytes
		}
		if r.IntN(3) == 0 {
			opts.ToolUseID = func(st agentrt.Step) string { return fmt.Sprintf("toolu_%d", st.Index) }
		}
		if r.IntN(3) == 0 {
			opts.Assistant = func(st agentrt.Step, id string) []agentrt.ContentBlock {
				if st.Index%2 == 0 {
					return nil
				}
				return []agentrt.ContentBlock{{Type: "raw", Text: id}}
			}
		}
		if r.IntN(3) == 0 {
			opts.Observation = func(st agentrt.Step, id string, full bool) agentrt.ContentBlock {
				switch st.Index % 3 {
				case 0:
					return agentrt.ContentBlock{}
				case 1:
					return render.DefaultObservation(st, id, !full, 3)
				}
				return agentrt.ContentBlock{Type: "tool_result", ToolUseID: id, Content: fmt.Sprint(full)}
			}
		}
		steps := make([]agentrt.Step, r.IntN(20))
		for i := range steps {
			steps[i] = randomStep(r, i, maxBytes)
		}
		in := input(steps...)
		want, got := oracleMessages(in, opts), render.Messages(in, opts)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("case %d: rendered differently:\n want %+v\n got  %+v", c, want, got)
		}
		wj, _ := json.Marshal(want)
		gj, _ := json.Marshal(got)
		if !bytes.Equal(wj, gj) {
			t.Fatalf("case %d: encoded differently", c)
		}
		for _, st := range steps {
			for _, full := range []bool{true, false} {
				for _, mb := range []int{0, 5, 16, 30, maxBytes} {
					if w, g := oracleDefaultObservation(st, "x", full, mb), render.DefaultObservation(st, "x", full, mb); !reflect.DeepEqual(w, g) {
						t.Fatalf("case %d: DefaultObservation(%v, %d) = %+v, want %+v", c, full, mb, g, w)
					}
				}
			}
		}
	}
}

// Turns share allocations, so a caller appending to one turn's blocks must
// not overwrite the next turn's.
func TestMessages_AppendingToATurnLeavesTheNextAlone(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	steps := make([]agentrt.Step, 12)
	for i := range steps {
		steps[i] = randomStep(r, i, 30)
		steps[i].Decision = &agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: "read", Reason: fmt.Sprint(i % 2)}
	}
	in := input(steps...)
	opts := render.Options{Opening: fixedOpening("g")}
	msgs := render.Messages(in, opts)
	for i := range msgs {
		_ = append(msgs[i].Content, agentrt.ContentBlock{Type: "appended"})
	}
	if want := render.Messages(in, opts); !reflect.DeepEqual(msgs, want) {
		t.Fatal("appending to one turn changed another")
	}
}
