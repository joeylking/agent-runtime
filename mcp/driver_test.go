package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

// TestDriver_ApprovalNamesThePrefixedTool runs a whole loop over a server
// tool: the policy stops the run before a local mutation, the approval binds
// the name the model used, and resuming executes exactly that call against
// the server.
func TestDriver_ApprovalNamesThePrefixedTool(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.add("write_file", "Write a file.", writeSchema, nil, echoHandler)
	m := mustPin(t, f)
	tools, rep, err := load(t, f, m, Rules{"write_file": {SideEffect: agentrt.LocalMutation, Timeout: time.Second}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rep.Registered) != 1 {
		t.Fatalf("report = %s", rep)
	}

	store, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	driver, err := agentrt.NewDriver(agentrt.Config{
		Store: store,
		Agent: &scripted.Agent{Decisions: []agentrt.Decision{
			scripted.ToolCall("fs_write_file", `{"path":"notes.txt","text":"hello"}`, "write the note"),
			scripted.Complete(`{"wrote":"notes.txt"}`),
		}},
		Policy: agentrt.SideEffectPolicy{
			agentrt.ReadOnly:       agentrt.Allow,
			agentrt.LocalMutation:  agentrt.RequireApproval,
			agentrt.RemoteMutation: agentrt.Deny,
			agentrt.Destructive:    agentrt.Deny,
		},
		Tools: tools,
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}

	run, err := driver.Start(ctx, "write a note", agentrt.DefaultLimits())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("status = %s, want the run paused", run.Status)
	}
	approvals, err := store.ListApprovals(ctx, run.ID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals = %v, err = %v", approvals, err)
	}
	var capability struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(approvals[0].Capability, &capability); err != nil {
		t.Fatalf("capability: %v", err)
	}
	if capability.Tool != "fs_write_file" {
		t.Errorf("capability tool = %q, want the prefixed name the model used", capability.Tool)
	}

	if err := driver.Approve(ctx, run.ID, approvals[0].ID, "joey", "ok"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	run, err = driver.Resume(ctx, run.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if run.Status != agentrt.StatusCompleted {
		t.Fatalf("status = %s, reason = %s %s", run.Status, run.Reason, run.ReasonDetail)
	}
	steps, err := store.ListSteps(ctx, run.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	sent := text(t, steps[0].Observation.Content)
	if !strings.Contains(sent, `"path":"notes.txt"`) {
		t.Errorf("the server received %s, want the approved arguments", sent)
	}
}
