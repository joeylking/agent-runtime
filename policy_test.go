package agentrt_test

import (
	"context"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

type publication struct {
	ProposalID string `json:"proposal_id"`
	Files      int    `json:"files"`
}

func TestNeedApproval_PausesAndBindsWhatWasShown(t *testing.T) {
	h := newHarness(t, ":memory:")
	push := echoTool("push", agentrt.RemoteMutation)
	pres := publication{ProposalID: "p1", Files: 2}
	policy := agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, _ agentrt.RunView) (agentrt.PolicyDecision, error) {
		if req.Spec.SideEffect != agentrt.RemoteMutation {
			return agentrt.PolicyDecision{Outcome: agentrt.Allow}, nil
		}
		return agentrt.NeedApproval("publication", "publishing proposal p1", map[string]any{"tool": req.Spec.Name}, pres)
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":7}`, ""), scripted.Complete(`{}`)}}
	run, err := h.driver(agent, policy, push).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("status = %s", run.Status)
	}
	approvals, _ := h.store.ListApprovals(context.Background(), run.ID)
	a := approvals[0]
	if a.Kind != "publication" || string(a.Capability) != `{"tool":"push"}` {
		t.Fatalf("approval = %+v", a)
	}
	got, err := agentrt.DecodePresentation[publication](a)
	if err != nil || got != pres {
		t.Fatalf("presentation = %+v, %v, want %+v", got, err, pres)
	}
	if len(push.calls) != 0 {
		t.Fatal("tool ran before approval")
	}
}

func TestNeedApproval_RefusesUnmarshalableValues(t *testing.T) {
	if _, err := agentrt.NeedApproval("k", "r", make(chan int), nil); err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("err = %v", err)
	}
	if _, err := agentrt.NeedApproval("k", "r", nil, make(chan int)); err == nil || !strings.Contains(err.Error(), "presentation") {
		t.Fatalf("err = %v", err)
	}
	// A nil value is an empty object, not JSON null, so the stored approval
	// and its hash stay well formed.
	d, err := agentrt.NeedApproval("k", "r", nil, nil)
	if err != nil || d.Outcome != agentrt.RequireApproval || string(d.Capability) != "{}" || string(d.Presentation) != "{}" {
		t.Fatalf("decision = %+v, %v", d, err)
	}
}

func TestDecodePresentation_ReportsWhatIsWrong(t *testing.T) {
	if _, err := agentrt.DecodePresentation[publication](agentrt.Approval{ID: "a1"}); err == nil || !strings.Contains(err.Error(), "no presentation") {
		t.Fatalf("err = %v", err)
	}
	a := agentrt.Approval{ID: "a1", Presentation: []byte(`{"proposal_id":42}`)}
	if _, err := agentrt.DecodePresentation[publication](a); err == nil || !strings.Contains(err.Error(), "a1") {
		t.Fatalf("err = %v", err)
	}
}
