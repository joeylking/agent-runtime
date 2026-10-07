package agentrt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Re-checking a run: each policy evaluation it recorded is handed again
// to a policy, rebuilt from the record as the policy saw it, and the
// answer compared with the one recorded. A policy that decides from what
// it is handed alone gives the same answers; one that reads anything else,
// a clock, a file, a service, can answer differently, and that is what a
// re-check under the policy a run ran with finds.

// RecheckOptions are what Recheck takes beyond the run and the policy.
type RecheckOptions struct {
	// Tools, when set, are the tools as registered now: each request is
	// handed the spec its tool's Spec returns now, by name, in place of
	// the one recorded, and its arguments are checked against that spec's
	// schema as a fresh decision's are, so the report says whether today's
	// policy with today's tools would have decided the same. A request
	// naming a tool not among them, or whose arguments its schema now
	// refuses, differs: it would be refused before the policy. Nil hands
	// each request the spec recorded with it.
	Tools []Tool
}

// SpecSource is which spec a re-checked request carried.
type SpecSource string

const (
	// SpecRecorded is the spec recorded with the evaluation, by the hash
	// its step.policy event names, read with Store.ToolSpec.
	SpecRecorded SpecSource = "recorded"
	// SpecCurrent is the spec of the tool RecheckOptions.Tools names.
	SpecCurrent SpecSource = "current"
)

// RecheckResult is what one recorded step.policy came to.
type RecheckResult string

const (
	// RecheckSame is an evaluation the policy answered alike: the same
	// outcome and, for require_approval, the same approval kind and hash.
	RecheckSame RecheckResult = "same"
	// RecheckDifferent is an evaluation it answered otherwise, or failed
	// to answer.
	RecheckDifferent RecheckResult = "different"
	// RecheckNotEvaluation is a step.policy no policy wrote: a
	// reconciliation's or an interrupted side effect's pause. There is
	// nothing to re-check.
	RecheckNotEvaluation RecheckResult = "not_a_policy_evaluation"
	// RecheckNotRecheckable is an evaluation the record cannot rebuild: one
	// recorded before v0.5.0, which kept neither the spec nor the view,
	// one whose record is damaged or larger than a page carries, or one
	// whose policy failed, which recorded no evaluation to rebuild.
	RecheckNotRecheckable RecheckResult = "not_recheckable"
)

// PolicyIdentity compares the identity a run's evaluations recorded with
// the identity of the policy they were re-checked under.
type PolicyIdentity string

const (
	// IdentityUnknown means one side, or both, has no identity: the policy
	// does not implement IdentifiedPolicy, or the evaluation did not record
	// one. Whether the policy changed cannot be told from the record.
	IdentityUnknown PolicyIdentity = "unknown"
	// IdentityChanged means both sides have one and they differ.
	IdentityChanged PolicyIdentity = "changed"
	// IdentityUnchanged means every evaluation recorded the identity the
	// policy has now.
	IdentityUnchanged PolicyIdentity = "unchanged"
)

// RecheckedEvaluation is one recorded step.policy and what re-checking
// it came to.
type RecheckedEvaluation struct {
	// StepIndex, StepID, and Seq name the step and the event; Tool is the
	// tool its decision named. OnResume is an evaluation made when an
	// approved request was resumed, which is a line of its own beside the
	// step's first.
	StepIndex int    `json:"step_index"`
	StepID    string `json:"step_id"`
	Seq       int64  `json:"seq"`
	Tool      string `json:"tool"`
	OnResume  bool   `json:"on_resume,omitempty"`
	// Result is what it came to.
	Result RecheckResult `json:"result"`
	// Recorded is the decision recorded; Rechecked is the policy's answer
	// now, nil when it was not asked or failed.
	Recorded  PolicyDecision  `json:"recorded"`
	Rechecked *PolicyDecision `json:"rechecked,omitempty"`
	// RecordedHash and RecheckedHash are the hashes of the approvals the
	// two require_approval decisions ask for, over their kind,
	// capability, and presentation and the request each was made for:
	// the recorded request, and the request re-checked, which carries the
	// current spec under SpecCurrent.
	RecordedHash  string `json:"recorded_hash,omitempty"`
	RecheckedHash string `json:"rechecked_hash,omitempty"`
	// Detail says what differs, and why an evaluation was not re-checked.
	// The policy's reason is never a difference: it is in Rechecked.
	Detail string `json:"detail,omitempty"`
	// SpecSource is the spec the re-checked request carried, empty when
	// the policy was not asked.
	SpecSource SpecSource `json:"spec_source,omitempty"`
	// PolicyID is the identity the evaluation recorded, empty for none.
	PolicyID string `json:"policy_id"`
}

// RecheckReport is what Recheck found for one run.
type RecheckReport struct {
	RunID     string    `json:"run_id"`
	RunStatus RunStatus `json:"run_status"`
	// PolicyID is the identity of the policy re-checked under, empty when
	// it does not implement IdentifiedPolicy, and Identity compares it
	// with the identities the evaluations recorded.
	PolicyID string         `json:"policy_id"`
	Identity PolicyIdentity `json:"identity"`
	// SpecSource is SpecCurrent when RecheckOptions.Tools was set.
	SpecSource SpecSource `json:"spec_source"`
	// Notes say what holds for the whole run: one not terminal, re-checked
	// up to its current state, and one recorded before v0.5.0.
	Notes []string `json:"notes,omitempty"`
	// Evaluations are the run's step.policy events in the order they were
	// written.
	Evaluations []RecheckedEvaluation `json:"evaluations"`
}

// Same reports whether no evaluation re-checked differs. An evaluation
// that was not re-checked, because no policy made it or the record cannot
// rebuild it, is not a difference, so a report that re-checked nothing is
// the same: Rechecked says how many were, and Complete whether any was
// left out. A gate wants all three.
func (r RecheckReport) Same() bool { return r.Differences() == 0 }

// Complete reports whether every evaluation listed was re-checked or is
// not a policy evaluation: none is RecheckNotRecheckable. It does not say
// that anything was re-checked; a run with no evaluation is complete.
func (r RecheckReport) Complete() bool { return r.count(RecheckNotRecheckable) == 0 }

// NotRecheckable is how many evaluations the record could not rebuild.
func (r RecheckReport) NotRecheckable() int { return r.count(RecheckNotRecheckable) }

// Differences is how many evaluations differ.
func (r RecheckReport) Differences() int { return r.count(RecheckDifferent) }

// Rechecked is how many evaluations the policy was asked again: those the
// same and those different.
func (r RecheckReport) Rechecked() int { return r.count(RecheckSame) + r.count(RecheckDifferent) }

func (r RecheckReport) count(res RecheckResult) int {
	n := 0
	for _, e := range r.Evaluations {
		if e.Result == res {
			n++
		}
	}
	return n
}

// MarshalJSON writes the report's fields with same, differences,
// rechecked, not_recheckable, and complete beside them, so a reader of
// the JSON need not count.
func (r RecheckReport) MarshalJSON() ([]byte, error) {
	type fields RecheckReport
	evals := r.Evaluations
	if evals == nil {
		evals = []RecheckedEvaluation{}
	}
	f := fields(r)
	f.Evaluations = evals
	return json.Marshal(struct {
		fields
		Same           bool `json:"same"`
		Differences    int  `json:"differences"`
		Rechecked      int  `json:"rechecked"`
		NotRecheckable int  `json:"not_recheckable"`
		Complete       bool `json:"complete"`
	}{f, r.Same(), r.Differences(), r.Rechecked(), r.NotRecheckable(), r.Complete()})
}

// recheckPage is how many rows each read of a run's record takes, and
// recheckApprovals how many of a run's approvals Recheck reads whole: an
// evaluation whose view counted more is not re-checkable.
const (
	recheckPage       = 128
	recheckApprovals  = 1024
	policyErrorPrefix = "policy error: "
)

// Recheck replays each policy evaluation a run recorded through policy,
// in the order they were written, and reports, evaluation by evaluation,
// whether the policy decides the same now. An evaluation is a step.policy
// event carrying the view of the run its policy saw; a step that was
// resumed after an approval has one for the resume as well. For each it
// rebuilds what the policy was handed:
//
//   - the request: the run and step ids, the arguments as the step's
//     decision recorded them, and the spec the event names (Store.ToolSpec),
//     or the current spec when RecheckOptions.Tools is set, the arguments
//     and the schema in PolicyJSON form, made from the stored bytes, which
//     is the form the loop handed the policy from the agent's;
//   - the run: its id, goal, limits, and creation time, which is its start
//     time, from run.created, and its status, step count, model calls,
//     tokens, estimated cost, and active time from the event's view; the
//     reason, detail, finish time, and result are empty, as they are while
//     a policy can be asked;
//   - the steps: the run's first ones, as many as the view counted, each
//     as stored now. A step before the evaluated one is finished before
//     the policy is asked and never written again, so it is what the
//     policy saw;
//   - the approvals: the run's first ones, as many as the view counted,
//     each read whole and as stored but with the status it had when the
//     event was written: the last approval.decided before the event, by seq, or
//     pending, with no decision time, decider, or note, when there was
//     none. An approval that has no approval.decided at all is judged by
//     its stored decided_at against the event's time instead.
//
// The policy's answer is compared with the recorded decision: the outcome
// and, for require_approval, the approval kind and the hash of the
// approval it asks for, computed as the runtime computes it, so a changed
// capability or presentation differs although the outcome does not. The
// reason is never compared. A policy error, or a decision whose capability
// or presentation is not usable JSON, is a difference, unless the recorded
// decision was refused as invalid for the same reason, with the same
// outcome and kind: that is the same, with the reason in Detail.
//
// A step.policy written by a reconciliation's or an interrupted side
// effect's pause is reported as not a policy evaluation, and one written
// before v0.5.0, which recorded neither spec nor view, as not
// re-checkable, as is one whose record the page reads cut: a step.policy
// event or a step longer than MaxPageText, which an approval's large
// capability or presentation in the step.policy can make. A policy that
// failed recorded no evaluation: its step is listed as not re-checkable,
// "policy failed; no evaluation recorded", at the seq of its step.failed.
// A gate's Allowed verdict that was never executed, an approved request
// Attach let through whose grant then expired among them, recorded no
// evaluation either, and leaves nothing to list beyond the step. A run
// that is not terminal is re-checked up to its current state, and a note
// says so. Recheck writes nothing and reads no clock: it reads the run, its
// events and steps a page at a time, each text column capped at
// MaxPageText; the approvals the views counted whole, as GetApproval reads
// one, at most 1024 of them, an evaluation whose view counted more being
// not re-checkable; and each spec whole. It returns ErrNotFound for a run
// the store does not have, and an error for a policy whose identity
// NewDriver would refuse.
//
// What it shows is limited by what the policy reads: a re-check under the
// policy the run ran with differs only where that policy decides from
// something other than what it is handed. It does not show that the
// record is unaltered; Store.VerifyEvents does, up to a head kept
// elsewhere.
func Recheck(ctx context.Context, store *Store, runID string, policy Policy, opts RecheckOptions) (RecheckReport, error) {
	if store == nil {
		return RecheckReport{}, errors.New("agentrt: recheck: store is nil")
	}
	if policy == nil {
		return RecheckReport{}, errors.New("agentrt: recheck: policy is nil")
	}
	given, err := policyID(policy)
	if err != nil {
		return RecheckReport{}, err
	}
	run, err := store.GetRunCapped(ctx, runID)
	if err != nil {
		return RecheckReport{}, err
	}
	rc := &rechecker{store: store, policy: policy, runID: runID, specs: map[string]specRead{}}
	rep := RecheckReport{RunID: runID, RunStatus: run.Status, PolicyID: given, SpecSource: SpecRecorded}
	if opts.Tools != nil {
		rep.SpecSource = SpecCurrent
		rc.current = map[string]Tool{}
		for _, t := range opts.Tools {
			if t == nil {
				return RecheckReport{}, errors.New("agentrt: recheck: a current tool is nil")
			}
			name := t.Spec().Name
			if _, dup := rc.current[name]; dup {
				return RecheckReport{}, fmt.Errorf("agentrt: recheck: current tool %q is given twice", name)
			}
			rc.current[name] = t
		}
	}
	if err := rc.read(ctx); err != nil {
		return RecheckReport{}, err
	}
	for _, pe := range rc.evaluations {
		ev, err := rc.recheck(ctx, pe)
		if err != nil {
			return RecheckReport{}, err
		}
		rep.Evaluations = append(rep.Evaluations, ev)
	}
	rep.Identity = identityOf(given, rep.Evaluations)
	if !run.Status.Terminal() {
		rep.Notes = append(rep.Notes, fmt.Sprintf("the run is %s: re-checked up to its current state", run.Status))
	}
	if rc.created.found && !rc.created.tools {
		rep.Notes = append(rep.Notes, "recorded before v0.5.0: its decisions recorded then cannot be re-checked")
	}
	if len(rep.Evaluations) == 0 {
		rep.Notes = append(rep.Notes, "no policy evaluation is recorded")
	}
	return rep, nil
}

// identityOf compares the identity given with those the evaluations the
// policy was asked about recorded.
func identityOf(given string, evals []RecheckedEvaluation) PolicyIdentity {
	asked := false
	changed := false
	for _, e := range evals {
		if e.Result != RecheckSame && e.Result != RecheckDifferent {
			continue
		}
		asked = true
		if e.PolicyID == "" || given == "" {
			return IdentityUnknown
		}
		if e.PolicyID != given {
			changed = true
		}
	}
	switch {
	case !asked:
		return IdentityUnknown
	case changed:
		return IdentityChanged
	}
	return IdentityUnchanged
}

// rechecker is one Recheck's reading of its run.
type rechecker struct {
	store   *Store
	policy  Policy
	runID   string
	current map[string]Tool

	created struct {
		found, tools, cut bool
		at                time.Time
		goal              string
		limits            Limits
	}
	steps     []Step
	byID      map[string]int
	approvals []Approval
	// approvalCount is how many approvals the run has; approvals holds
	// the first of them, as many as the views counted, at most
	// recheckApprovals.
	approvalCount int
	// decided is each approval's approval.decided events, in seq order.
	decided     map[string][]decidedEvent
	evaluations []policyEvent
	specs       map[string]specRead
	// prev is the run's event before the one being read, and evaluated
	// the steps a step.policy has been read for.
	prev      Event
	evaluated map[string]bool
}

type decidedEvent struct {
	seq    int64
	status ApprovalStatus
}

// policyEvent is a step.policy event as Recheck reads it, or the
// step.failed of a policy error, which wrote none.
type policyEvent struct {
	ev  Event
	rec recordedPolicy
	bad string
	// invalid is why the runtime refused the recorded decision, from the
	// step.failed written with it, empty when it did not.
	invalid string
	// failed is the error of a policy that failed and recorded no
	// evaluation, and failedAgain whether an earlier evaluation of the
	// step was read, so this one was on resume.
	failed      string
	isFailure   bool
	failedAgain bool
}

// recordedPolicy is a step.policy payload: policyRecord as decoded, and the
// marker a page read leaves on one it cut.
type recordedPolicy struct {
	PolicyDecision
	PolicyID  string      `json:"policy_id"`
	SpecHash  string      `json:"spec_hash"`
	View      *policyView `json:"view"`
	Truncated bool        `json:"truncated"`
}

// specRead is a spec read by its hash, or why it could not be.
type specRead struct {
	spec    ToolSpec
	problem string
}

// read reads the run's events, steps, and approvals a page at a time.
func (rc *rechecker) read(ctx context.Context) error {
	var seq int64
	for {
		page, err := rc.store.ListEventsAfter(ctx, rc.runID, seq, recheckPage)
		if err != nil {
			return err
		}
		for _, e := range page {
			rc.event(e)
			seq = e.Seq
		}
		if len(page) < recheckPage {
			break
		}
	}
	rc.byID = map[string]int{}
	for offset := 0; ; offset += recheckPage {
		page, total, err := rc.store.ListStepsPage(ctx, rc.runID, recheckPage, offset)
		if err != nil {
			return err
		}
		for _, st := range page {
			rc.byID[st.ID] = len(rc.steps)
			rc.steps = append(rc.steps, st)
		}
		if len(page) == 0 || offset+len(page) >= total {
			break
		}
	}
	// The approvals the views counted are read whole, as GetApproval reads
	// one: a policy saw each whole, and a decision is bound to all of it.
	n, err := rc.store.count(ctx, `SELECT COUNT(*) FROM approvals WHERE run_id = ?`, rc.runID)
	if err != nil {
		return err
	}
	rc.approvalCount = n
	need := 0
	for _, pe := range rc.evaluations {
		if v := pe.rec.View; v != nil && v.Approvals > need {
			need = v.Approvals
		}
	}
	if need = min(need, n, recheckApprovals); need > 0 {
		if rc.approvals, err = rc.store.firstApprovals(ctx, rc.runID, need); err != nil {
			return err
		}
	}
	return nil
}

// event keeps what Recheck needs of one event.
func (rc *rechecker) event(e Event) {
	prev := rc.prev
	rc.prev = Event{Seq: e.Seq, StepID: e.StepID, Type: e.Type}
	switch e.Type {
	case EventRunCreated:
		var c struct {
			Goal      string           `json:"goal"`
			Limits    Limits           `json:"limits"`
			Tools     *json.RawMessage `json:"tools"`
			Truncated bool             `json:"truncated"`
		}
		err := json.Unmarshal(e.Payload, &c)
		rc.created.found, rc.created.at = true, e.At
		rc.created.cut = err != nil || c.Truncated
		rc.created.goal, rc.created.limits, rc.created.tools = c.Goal, c.Limits, c.Tools != nil
	case EventApprovalDecided:
		var d struct {
			ID     string         `json:"approval_id"`
			Status ApprovalStatus `json:"status"`
		}
		if json.Unmarshal(e.Payload, &d) == nil && d.ID != "" {
			if rc.decided == nil {
				rc.decided = map[string][]decidedEvent{}
			}
			rc.decided[d.ID] = append(rc.decided[d.ID], decidedEvent{seq: e.Seq, status: d.Status})
		}
	case EventStepPolicy:
		pe := policyEvent{ev: e}
		if err := json.Unmarshal(e.Payload, &pe.rec); err != nil {
			pe.bad = "its step.policy does not decode: " + err.Error()
		} else if pe.rec.Truncated {
			pe.bad = "its step.policy is longer than MaxPageText"
		}
		e.Payload = nil
		pe.ev = e
		rc.evaluations = append(rc.evaluations, pe)
		if rc.evaluated == nil {
			rc.evaluated = map[string]bool{}
		}
		rc.evaluated[e.StepID] = true
	case EventStepFailed:
		// A policy error fails the step with this detail. A decision the
		// runtime refused wrote its step.policy just before, in the same
		// transaction; a policy that failed wrote none.
		var f struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(e.Payload, &f) != nil || !strings.HasPrefix(f.Detail, policyErrorPrefix) {
			return
		}
		why := strings.TrimPrefix(f.Detail, policyErrorPrefix)
		if prev.Type == EventStepPolicy && prev.StepID == e.StepID && len(rc.evaluations) > 0 {
			rc.evaluations[len(rc.evaluations)-1].invalid = why
			return
		}
		e.Payload = nil
		rc.evaluations = append(rc.evaluations, policyEvent{ev: e, failed: why, isFailure: true, failedAgain: rc.evaluated[e.StepID]})
	}
}

// recheck re-checks one recorded step.policy.
func (rc *rechecker) recheck(ctx context.Context, pe policyEvent) (RecheckedEvaluation, error) {
	rec := pe.rec
	out := RecheckedEvaluation{StepID: pe.ev.StepID, Seq: pe.ev.Seq, StepIndex: -1, Recorded: rec.PolicyDecision, PolicyID: rec.PolicyID}
	out.Recorded.Capability, out.Recorded.Presentation = nonEmpty(rec.Capability), nonEmpty(rec.Presentation)
	not := func(res RecheckResult, detail string) (RecheckedEvaluation, error) {
		out.Result, out.Detail = res, detail
		return out, nil
	}
	cannot := func(why string) (RecheckedEvaluation, error) {
		return not(RecheckNotRecheckable, "not re-checkable: "+why)
	}
	i, ok := rc.byID[pe.ev.StepID]
	var step Step
	if ok {
		step = rc.steps[i]
		out.StepIndex = step.Index
		if step.Decision != nil {
			out.Tool = step.Decision.Tool
		}
	}
	if rec.View != nil {
		out.OnResume = rec.View.Status == StatusWaitingForApproval
	}
	switch {
	case pe.isFailure:
		out.OnResume, out.PolicyID = pe.failedAgain, step.PolicyID
		return not(RecheckNotRecheckable, "policy failed; no evaluation recorded: "+pe.failed)
	case pe.bad != "":
		return cannot(pe.bad)
	case rec.View == nil && rec.SpecHash == "":
		return cannot("recorded before v0.5.0")
	case rec.View == nil && rec.Kind == InterruptedSideEffect:
		return not(RecheckNotEvaluation, "not a policy evaluation: an interrupted side effect's pause")
	case rec.View == nil:
		return not(RecheckNotEvaluation, "not a policy evaluation: a reconciliation's pause")
	case rec.SpecHash == "":
		return cannot("no spec is recorded")
	case !ok:
		return cannot("its step is not recorded")
	case step.DecodeError != "":
		return cannot(fmt.Sprintf("step %d: %s", step.Index, step.DecodeError))
	case step.Decision == nil || step.Decision.Kind != DecideToolCall:
		return cannot(fmt.Sprintf("step %d recorded no tool call", step.Index))
	case !rc.created.found || rc.created.cut:
		return cannot("its run.created is missing or longer than MaxPageText")
	}
	v := rec.View
	if v.Steps < 0 || v.Steps > len(rc.steps) || v.Approvals < 0 || v.Approvals > rc.approvalCount {
		return cannot(fmt.Sprintf("the view counted %d steps and %d approvals; the run has %d and %d", v.Steps, v.Approvals, len(rc.steps), rc.approvalCount))
	}
	if v.Approvals > len(rc.approvals) {
		return cannot(fmt.Sprintf("the view counted %d approvals, more than the %d Recheck reads whole", v.Approvals, recheckApprovals))
	}
	view := RunView{Run: Run{ID: rc.runID, Goal: rc.created.goal, Status: v.Status, Limits: rc.created.limits, StepCount: v.StepCount,
		CreatedAt: rc.created.at, StartedAt: rc.created.at, ModelCalls: v.ModelCalls,
		Usage:         Usage{InputTokens: v.InputTokens, OutputTokens: v.OutputTokens, CachedInputTokens: v.CachedInputTokens},
		EstimatedCost: v.EstimatedCost, ActiveTime: time.Duration(v.ActiveMS) * time.Millisecond}}
	if v.Steps > 0 {
		view.Steps = cloneSteps(rc.steps[:v.Steps])
	}
	for _, st := range view.Steps {
		if st.DecodeError != "" {
			return cannot(fmt.Sprintf("step %d: %s", st.Index, st.DecodeError))
		}
	}
	if v.Approvals > 0 {
		view.Approvals = cloneApprovals(rc.approvals[:v.Approvals])
	}
	for j := range view.Approvals {
		a := &view.Approvals[j]
		if a.DecodeError != "" {
			return cannot(fmt.Sprintf("approval %s: %s", a.ID, a.DecodeError))
		}
		rc.statusAt(a, pe.ev)
	}

	recorded, err := rc.spec(ctx, rec.SpecHash)
	if err != nil {
		return out, err
	}
	if recorded.problem != "" {
		return cannot(recorded.problem)
	}
	args := orEmptyObject(cloneBytes(step.Decision.Args))
	recordedReq := ToolRequest{RunID: rc.runID, StepID: step.ID, Spec: recorded.spec, Args: args}
	req := recordedReq
	out.SpecSource = SpecRecorded
	if rc.current != nil {
		out.SpecSource = SpecCurrent
		t, ok := rc.current[step.Decision.Tool]
		if !ok {
			out.Result, out.Detail = RecheckDifferent, fmt.Sprintf("tool %q is not among the current tools: the request would be refused before the policy", step.Decision.Tool)
			return out, nil
		}
		req.Spec = t.Spec()
		req.Spec.InputSchema = cloneBytes(req.Spec.InputSchema)
		if serr := CheckArgs(req.Spec, args); serr != nil {
			out.Result, out.Detail = RecheckDifferent, "the arguments fail the current tool's schema: "+serr.Error()
			return out, nil
		}
	}
	// The policy is handed what the loop handed it: the arguments and the
	// schema in PolicyJSON form, made here from the stored, escaped bytes.
	handed, herr := policyRequest(req)
	if herr != nil {
		return cannot("the request does not encode for the policy: " + herr.Error())
	}

	pd, perr := rc.policy.Evaluate(ctx, handed, view)
	if cerr := ctx.Err(); cerr != nil {
		return out, cerr
	}
	if perr != nil {
		out.Result, out.Detail = RecheckDifferent, "policy failed: "+perr.Error()
		return out, nil
	}
	pd.Capability, pd.Presentation = nonEmpty(pd.Capability), nonEmpty(pd.Presentation)
	out.Rechecked = &pd
	cerr := checkPolicy(pd)
	switch {
	case cerr != nil && pe.invalid != "" && cerr.Error() == pe.invalid && pd.Outcome == rec.Outcome && pd.Kind == rec.Kind:
		// The runtime refused the recorded decision for this reason, and
		// would refuse this one for the same.
		out.Result, out.Detail = RecheckSame, "an invalid decision, refused as recorded: "+cerr.Error()
		return out, nil
	case cerr != nil:
		out.Result, out.Detail = RecheckDifferent, "policy returned an invalid decision: "+cerr.Error()
		return out, nil
	case pe.invalid != "":
		out.Result, out.Detail = RecheckDifferent, "the recorded decision was refused as invalid ("+pe.invalid+"); the policy now returns a valid one"
		return out, nil
	}
	out.Result = RecheckSame
	if rec.Outcome != pd.Outcome {
		out.Result = RecheckDifferent
	}
	if rec.Outcome == RequireApproval {
		if out.RecordedHash, err = approvalHash(rec.Kind, rec.Capability, rec.Presentation, recordedReq); err != nil {
			return cannot("the recorded approval does not hash: " + err.Error())
		}
	}
	if pd.Outcome == RequireApproval {
		if out.RecheckedHash, err = approvalHash(pd.Kind, pd.Capability, pd.Presentation, req); err != nil {
			out.Result, out.Detail = RecheckDifferent, "policy returned an invalid decision: "+err.Error()
			return out, nil
		}
	}
	if out.Result == RecheckSame && rec.Outcome == RequireApproval {
		switch {
		case rec.Kind != pd.Kind:
			out.Result, out.Detail = RecheckDifferent, fmt.Sprintf("asks for approval kind %q, not %q", pd.Kind, rec.Kind)
		case out.RecordedHash != out.RecheckedHash:
			out.Result, out.Detail = RecheckDifferent, "asks for another approval: its capability, presentation, or tool spec changed"
		}
	}
	return out, nil
}

// nonEmpty is raw, or nil when it holds nothing, so a decision read back
// and one a policy returned compare and print alike.
func nonEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// statusAt sets a's status to the one it had when e was written: the
// status of its last approval.decided before e, or pending with no
// decision when there was none. An approval with no approval.decided at
// all, which the runtime always writes with a decision, is judged by its
// stored decided_at against e's time.
func (rc *rechecker) statusAt(a *Approval, e Event) {
	status := ApprovalPending
	if events, ok := rc.decided[a.ID]; ok {
		for _, d := range events {
			if d.seq < e.Seq {
				status = d.status
			}
		}
	} else if a.Status != ApprovalPending && !a.DecidedAt.IsZero() && !a.DecidedAt.After(e.At) {
		status = a.Status
	}
	a.Status = status
	if status == ApprovalPending {
		a.DecidedAt, a.DecidedBy, a.Note = time.Time{}, "", ""
	}
}

// spec reads a spec by its hash once per Recheck: whole, as ToolSpec reads
// it, with a spec that is missing or no longer hashes reported rather than
// returned as an error.
func (rc *rechecker) spec(ctx context.Context, hash string) (specRead, error) {
	if s, ok := rc.specs[hash]; ok {
		return s, nil
	}
	var raw string
	var out specRead
	err := rc.store.db.QueryRowContext(ctx, `SELECT spec_json FROM tool_specs WHERE hash = ?`, hash).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		out.problem = fmt.Sprintf("spec %s is not stored", hash)
	case err != nil:
		return specRead{}, err
	default:
		if derr := json.Unmarshal([]byte(raw), &out.spec); derr != nil {
			out.problem = fmt.Sprintf("spec %s does not decode: %v", hash, derr)
		} else if e, herr := newSpecEntry(out.spec); herr != nil || e.hash != hash {
			out.problem = fmt.Sprintf("spec %s does not match its hash", hash)
		}
	}
	rc.specs[hash] = out
	return out, nil
}
