package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The status tool is the proxy's own, answered from the store: it reads,
// never executes, never decides, and is never recorded as a run. A model
// asked for an update looks for a tool like it, which is why it exists.
const (
	statusDescription = "Report the requests in this session that are waiting for an operator's approval, or, given approval_id, what became of that one: " +
		"pending, approved, executed, rejected, or expired. This tool only reports; it never runs or approves anything."
	statusSchema = `{"type":"object","properties":{"approval_id":{"type":"string","description":"The approval id a pending result named."}},"additionalProperties":false}`
	// statusRecent is how far back the status tool looks for an approval
	// it is asked about.
	statusRecent = time.Hour
	// statusRows bounds the calls one status reads.
	statusRows = 200
)

// PrefixStatus starts every answer of the status tool.
const PrefixStatus = "GATE_STATUS"

func (p *Proxy) statusHandler(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	if !p.enter() {
		return nil, errors.New("the proxy is shutting down")
	}
	defer p.inflight.Done()
	var in struct {
		ApprovalID string `json:"approval_id"`
	}
	raw := arguments(req.Params)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return invalidText("the only argument is approval_id, a string"), nil
	}
	return p.status(context.WithoutCancel(ctx), strings.TrimSpace(in.ApprovalID)), nil
}

// tracked is one approval of the session with its call and run.
type tracked struct {
	r  row
	st runState
	a  agentrt.Approval
}

// status reads the session's recent calls and describes every approval
// still waiting, or only the one named, from those within statusRecent.
func (p *Proxy) status(ctx context.Context, approvalID string) *sdk.CallToolResult {
	now := p.now()
	rows, err := p.idx.recent(ctx, p.session, now.Add(-statusRecent), statusRows)
	if err != nil {
		p.logf("error: %v", err)
		return unavailableText("the gate could not read its records")
	}
	var all []tracked
	for _, r := range rows {
		st, err := p.inspect(ctx, r.runID)
		if err != nil {
			p.logf("error: %v", err)
			return unavailableText("the gate could not read its records")
		}
		if !st.exists {
			continue
		}
		for _, a := range st.approvals {
			all = append(all, tracked{r: r, st: st, a: a})
		}
	}
	var b strings.Builder
	if approvalID != "" {
		for _, x := range all {
			if x.a.ID == approvalID {
				fmt.Fprintf(&b, "%s: %s", PrefixStatus, p.describe(x, now))
				return textResult(false, "%s", b.String())
			}
		}
		return textResult(false, "%s: no approval %s is known in this session from the last hour.", PrefixStatus, clean(approvalID, 100))
	}
	var waiting []string
	for _, x := range all {
		if p.isWaiting(x, now) {
			waiting = append(waiting, "- "+p.describe(x, now))
		}
	}
	switch len(waiting) {
	case 0:
		fmt.Fprintf(&b, "%s: no request in this session is waiting for an operator's approval.", PrefixStatus)
	case 1:
		fmt.Fprintf(&b, "%s: 1 request in this session is waiting for an operator's approval.\n%s", PrefixStatus, waiting[0])
	default:
		fmt.Fprintf(&b, "%s: %d requests in this session are waiting for an operator's approval.\n%s", PrefixStatus, len(waiting), strings.Join(waiting, "\n"))
	}
	return textResult(false, "%s", b.String())
}

func (p *Proxy) isWaiting(x tracked, now time.Time) bool {
	return x.a.Status == agentrt.ApprovalPending && (x.a.ExpiresAt.IsZero() || !now.After(x.a.ExpiresAt))
}

// describe is one approval in a sentence: the call, how long it has
// waited or what became of it, and what the model should do. An approved
// request is reported executed only when its step executed.
func (p *Proxy) describe(x tracked, now time.Time) string {
	call := fmt.Sprintf("approval %s for %s %s", x.a.ID, clean(x.r.tool, 64), clean(compact(x.a.Request.Args), 200))
	latest := x.st.latest()
	switch {
	case p.isWaiting(x, now) && x.a.Kind == agentrt.InterruptedSideEffect:
		return fmt.Sprintf("%s: OUTCOME UNKNOWN, waiting %s for an operator to decide whether it runs again; an earlier attempt may have taken effect. Nothing further has been executed.",
			call, age(now, x.a.CreatedAt))
	case p.isWaiting(x, now):
		return fmt.Sprintf("%s: PENDING for %s. Nothing has been executed; the operator decides elsewhere. Do not call the tool again until it is approved.",
			call, age(now, x.a.CreatedAt))
	case x.a.Status == agentrt.ApprovalPending:
		return fmt.Sprintf("%s: EXPIRED without a decision. It was not executed.", call)
	case x.a.Status == agentrt.ApprovalRejected:
		note := ""
		if strings.TrimSpace(x.a.Note) != "" {
			note = fmt.Sprintf(" The operator's note: %q.", clean(x.a.Note, 500))
		}
		return fmt.Sprintf("%s: REJECTED by the operator.%s It was not executed; do not ask for it again.", call, note)
	case x.a.Status == agentrt.ApprovalExpired:
		return fmt.Sprintf("%s: EXPIRED. It was not executed.", call)
	}
	// Approved.
	if st := stepOf(x.st, x.a.StepID); st != nil && st.Observation != nil &&
		(st.Observation.Kind == agentrt.ObserveToolResult || st.Observation.Kind == agentrt.ObserveToolError) {
		return fmt.Sprintf("%s: APPROVED and executed.", call)
	}
	switch {
	case latest != nil && latest.ID != x.a.ID:
		return fmt.Sprintf("%s: APPROVED, and then a different approval was asked for the same call (approval %s).", call, latest.ID)
	case x.st.run.Status == agentrt.StatusWaitingForApproval:
		return fmt.Sprintf("%s: APPROVED, awaiting collection. Call %s again with exactly the same arguments to run it and get the result.", call, clean(x.r.tool, 64))
	case !x.st.run.Status.Terminal():
		return fmt.Sprintf("%s: APPROVED and executing now.", call)
	}
	return fmt.Sprintf("%s: APPROVED but not executed: %s.", call, notExecuted(x.st, x.a.StepID))
}

// notExecuted says why an approved request's run ended before its step
// executed.
func notExecuted(st runState, stepID string) string {
	if s := stepOf(st, stepID); s != nil && s.Observation != nil {
		switch s.Observation.Kind {
		case agentrt.ObservePolicyDenied:
			return "the policy denied it when it was collected"
		case agentrt.ObserveInvalidDecision:
			return "its arguments no longer matched the tool's schema when it was collected"
		}
	}
	switch st.run.Reason {
	case agentrt.ReasonOperatorCancelled:
		return "an operator cancelled it"
	case agentrt.ReasonApprovalExpired:
		return "the approval expired before it was collected"
	}
	return "its run ended (" + clean(string(st.run.Reason), 64) + ")"
}

func stepOf(st runState, id string) *agentrt.Step {
	for i := range st.steps {
		if st.steps[i].ID == id {
			return &st.steps[i]
		}
	}
	return nil
}

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

func age(now, since time.Time) string {
	d := now.Sub(since).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}
