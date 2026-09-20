package agentrt_test

import (
	"context"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

// clock is a controllable time source for the driver.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

// slowTool advances the clock while it runs, so each step has a duration.
func slowTool(c *clock, per time.Duration) *fakeTool {
	return newTool("work", agentrt.ReadOnly, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		c.advance(per)
		return agentrt.ToolResult{Content: []byte(`{}`)}, nil
	})
}

func timedDriver(t *testing.T, h *harness, c *clock, agent agentrt.Agent, tools ...agentrt.Tool) *agentrt.Driver {
	t.Helper()
	d, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: tools, Now: c.Now,
		Observer: func(e agentrt.Event) { h.events = append(h.events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLimits_ActiveTimeExcludesApprovalWait(t *testing.T) {
	h := newHarness(t, ":memory:")
	c := &clock{now: time.Unix(1700000000, 0)}
	work := slowTool(c, 10*time.Second)
	push := echoTool("push", agentrt.RemoteMutation)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("work", `{}`, ""), scripted.ToolCall("work", `{}`, ""),
		scripted.ToolCall("push", `{"n":1}`, ""),
		scripted.ToolCall("work", `{}`, ""), scripted.Complete(`{}`),
	}}
	l := limits(20, 3)
	l.MaxActiveTime = 45 * time.Second
	d := timedDriver(t, h, c, agent, work, push)
	run, err := d.Start(context.Background(), "g", l)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusWaitingForApproval || run.ActiveTime != 20*time.Second {
		t.Fatalf("run = %s active %s", run.Status, run.ActiveTime)
	}
	// A long wait for the human does not count.
	c.advance(3 * time.Hour)
	approvals, _ := h.store.ListApprovals(context.Background(), run.ID)
	if err := d.Approve(context.Background(), run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	run, err = d.Resume(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if run.ActiveTime < 30*time.Second || run.ActiveTime > 31*time.Second {
		t.Fatalf("active time = %s", run.ActiveTime)
	}
}

func TestLimits_ActiveTimeStopsRun(t *testing.T) {
	h := newHarness(t, ":memory:")
	c := &clock{now: time.Unix(1700000000, 0)}
	work := slowTool(c, 10*time.Second)
	decisions := make([]agentrt.Decision, 0, 10)
	for i := 0; i < 10; i++ {
		decisions = append(decisions, scripted.ToolCall("work", `{}`, ""))
	}
	l := limits(20, 3)
	l.MaxActiveTime = 25 * time.Second
	run, _ := timedDriver(t, h, c, &scripted.Agent{Decisions: decisions}, work).Start(context.Background(), "g", l)
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonLimitActiveTime)
	if run.StepCount != 3 || len(work.calls) != 3 {
		t.Fatalf("steps = %d calls = %d, want 3 (limit checked before the fourth)", run.StepCount, len(work.calls))
	}
}

func TestLimits_ElapsedTimeIncludesWaits(t *testing.T) {
	h := newHarness(t, ":memory:")
	c := &clock{now: time.Unix(1700000000, 0)}
	push := echoTool("push", agentrt.RemoteMutation)
	read := echoTool("read", agentrt.ReadOnly)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":1}`, ""), scripted.Complete(`{}`)}}
	l := limits(20, 3)
	l.MaxElapsedTime = time.Hour
	d := timedDriver(t, h, c, agent, push, read)
	run, _ := d.Start(context.Background(), "g", l)
	approvals, _ := h.store.ListApprovals(context.Background(), run.ID)
	c.advance(2 * time.Hour)
	if err := d.Approve(context.Background(), run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	run, err := d.Resume(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The approved step executes, then the elapsed limit stops the loop.
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonLimitElapsedTime)
	if len(push.calls) != 1 || len(read.calls) != 0 {
		t.Fatalf("push %d read %d", len(push.calls), len(read.calls))
	}
}

func TestApproval_ExpiryCancelsRun(t *testing.T) {
	for _, via := range []string{"approve", "resume"} {
		t.Run(via, func(t *testing.T) {
			h := newHarness(t, ":memory:")
			c := &clock{now: time.Unix(1700000000, 0)}
			push := echoTool("push", agentrt.RemoteMutation)
			agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, "")}}
			l := limits(20, 3)
			l.ApprovalTTL = 10 * time.Minute
			d := timedDriver(t, h, c, agent, push)
			run, _ := d.Start(context.Background(), "g", l)
			approvals, _ := h.store.ListApprovals(context.Background(), run.ID)
			if approvals[0].ExpiresAt.Sub(c.now) != 10*time.Minute {
				t.Fatalf("expires at %s", approvals[0].ExpiresAt)
			}
			c.advance(11 * time.Minute)
			var err error
			if via == "approve" {
				// Approve uses wall-clock time; the TTL is long past.
				err = d.Approve(context.Background(), run.ID, approvals[0].ID, "joey", "")
				if err == nil {
					t.Fatal("expired approval accepted")
				}
			} else {
				_, err = d.Resume(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			got, _ := h.store.GetRun(context.Background(), run.ID)
			requireStatus(t, got, agentrt.StatusCancelled, agentrt.ReasonApprovalExpired)
			approvals, _ = h.store.ListApprovals(context.Background(), run.ID)
			if approvals[0].Status != agentrt.ApprovalExpired || len(push.calls) != 0 {
				t.Fatalf("approval = %+v calls = %d", approvals[0], len(push.calls))
			}
		})
	}
}
