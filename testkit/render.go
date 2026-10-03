package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/render"
)

// Rendered is the model request agent makes for in: Decide is called with
// a ModelCaller that records the first request and answers it with an
// empty end_turn reply. It is the request a recording keys on, so what a
// replay depends on is exactly what it returns.
func Rendered(t testing.TB, agent agentrt.Agent, in agentrt.StepInput) agentrt.ModelRequest {
	t.Helper()
	rec := &recordingCaller{}
	in.Model = rec
	if _, err := agent.Decide(context.Background(), in); err != nil {
		t.Fatalf("testkit: agent: %v", err)
	}
	if !rec.called {
		t.Fatalf("testkit: the agent made no model request")
	}
	return rec.req
}

type recordingCaller struct {
	req    agentrt.ModelRequest
	called bool
}

func (c *recordingCaller) Generate(_ context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	if !c.called {
		c.req, c.called = req, true
	}
	return agentrt.ModelResponse{StopReason: render.StopEndTurn}, nil
}

// SameRenders runs the Scenario fresh returns twice, with the same crashes,
// and fails unless the agent fresh returns renders a byte-identical model
// request from what the two runs handed their agents at every decision,
// the decisions after a pause or a crash included. fresh is called once
// per run: a Scenario whose tools keep state, or an agent that caches by
// run, must not be shared, and a deterministic Now and NewID in the
// Scenario make the two records identical in ids and times, which an
// agent that renders either depends on.
func SameRenders(t testing.TB, fresh func() (Scenario, agentrt.Agent), crashes ...Crash) {
	t.Helper()
	sa, aa := fresh()
	a := Run(t, sa, crashes...)
	sb, ab := fresh()
	b := Run(t, sb, crashes...)
	if len(a.Inputs) != len(b.Inputs) {
		t.Errorf("testkit: the runs asked their agents %d and %d times", len(a.Inputs), len(b.Inputs))
		return
	}
	for i := range a.Inputs {
		ra, rb := Rendered(t, aa, a.Inputs[i]), Rendered(t, ab, b.Inputs[i])
		ja, _ := json.Marshal(ra)
		jb, _ := json.Marshal(rb)
		if bytes.Equal(ja, jb) {
			continue
		}
		at := "the request outside its messages"
		for k := range ra.Messages {
			if k >= len(rb.Messages) {
				at = "message count"
				break
			}
			ma, _ := json.Marshal(ra.Messages[k])
			mb, _ := json.Marshal(rb.Messages[k])
			if !bytes.Equal(ma, mb) {
				at = "message " + itoa(k) + ":\n  " + clip(string(ma), 600) + "\n  " + clip(string(mb), 600)
				break
			}
		}
		if len(ra.Messages) != len(rb.Messages) {
			at = "message count"
		}
		t.Errorf("testkit: decision %d renders differently across two runs, at %s", i, at)
	}
}
