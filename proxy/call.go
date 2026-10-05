package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// action is what deciding a call came to: an answer, a request to execute
// outside the lock, an approval to hold for or answer pending on, or a
// race lost, to decide again.
type action struct {
	result  *sdk.CallToolResult
	exec    *execution
	pending *agentrt.Approval
	retry   bool
}

// execution is an allowed request and the session that runs it.
type execution struct {
	s        *agentrt.Session
	st       *agentrt.OpenStep
	t        *tool
	approval *agentrt.Approval
}

func answer(r *sdk.CallToolResult) action { return action{result: r} }

// The proxy's closing decisions. Each run holds one call, so the proxy
// ends a run its tool call did not complete with a fail decision of
// origin operator: the proxy decided it, on the operator's configuration,
// and the model did not.
const (
	closeToolError  = "one call per run: the tool call returned an error"
	closeDenied     = "one call per run: the request was denied"
	closeInvalid    = "one call per run: the request was invalid"
	closeRecovered  = "one call per run: taken over after an interruption, with nothing left in it to execute"
	closeUnrecorded = "one call per run: the tool call executed and its result could not be recorded"
)

// handle answers one call to an upstream tool. Gate and store work runs on
// a context the host cannot cancel: a host that cancels a call, or times
// out, while its tool runs does not stop the tool, whose outcome is
// recorded and given to an identical call later. Only the hold for an
// approval ends with the host's context.
func (p *Proxy) handle(ctx context.Context, ss *sdk.ServerSession, token any, t *tool, args json.RawMessage) *sdk.CallToolResult {
	wctx := context.WithoutCancel(ctx)
	key, valid := requestKey(t.name, args)
	held := false
	for tries := 0; ; tries++ {
		p.mu.Lock()
		a := p.decide(wctx, t, key, valid, args)
		p.mu.Unlock()
		switch {
		case a.exec != nil:
			return p.execute(wctx, a.exec)
		case a.pending != nil:
			if !held && p.hold > 0 && ctx.Err() == nil {
				held = true
				if p.wait(ctx, ss, token, *a.pending) && ctx.Err() == nil {
					continue
				}
			}
			return pendingAnswer(*a.pending)
		case a.retry:
			if tries < 5 {
				continue
			}
			return inProgressText()
		default:
			return a.result
		}
	}
}

// pendingAnswer is what a call waiting on an approval is told: an
// interruption's own text when the approval asks whether an attempt whose
// outcome is unknown runs again, and the pending text otherwise.
func pendingAnswer(a agentrt.Approval) *sdk.CallToolResult {
	if a.Kind != agentrt.InterruptedSideEffect {
		return pendingText(a.ID)
	}
	var c capability
	if json.Unmarshal(a.Capability, &c) == nil && c.Interrupted != nil && c.Interrupted.Ended != endedCutOff {
		return unknownText(c.Interrupted.how(), "", a.ID)
	}
	return interruptedText(a.ID)
}

// runState is a run as the proxy reads it: the run, its steps and
// approvals, and its lease. exists is false for a run never begun.
type runState struct {
	exists    bool
	run       agentrt.Run
	steps     []agentrt.Step
	approvals []agentrt.Approval
	owner     string
	until     time.Time
}

func (st runState) latest() *agentrt.Approval {
	if len(st.approvals) == 0 {
		return nil
	}
	return &st.approvals[len(st.approvals)-1]
}

func (p *Proxy) inspect(ctx context.Context, runID string) (runState, error) {
	run, err := p.store.GetRun(ctx, runID)
	if errors.Is(err, agentrt.ErrNotFound) {
		return runState{}, nil
	}
	if err != nil {
		return runState{}, err
	}
	st := runState{exists: true, run: run}
	if st.steps, err = p.store.ListSteps(ctx, runID); err != nil {
		return runState{}, err
	}
	if st.approvals, err = p.store.ListApprovals(ctx, runID); err != nil {
		return runState{}, err
	}
	if st.owner, st.until, err = p.store.Lease(ctx, runID); err != nil {
		return runState{}, err
	}
	return st, nil
}

// leased reports a lease another call holds and has not let expire. Lease
// times are the wall clock's, never the configured clock's.
func (st runState) leased() bool {
	return st.owner != "" && st.until.After(time.Now())
}

func (p *Proxy) unavailable(err error, what string) action {
	p.logf("error: %v", err)
	return answer(unavailableText(what))
}

const notRead = "the gate could not read its records, and nothing was executed"

// decide applies the rules to one call, in the order docs/proxy.md gives
// them, each a step that answers the call or passes it to the next. It
// holds p.mu, so within this process nothing else reads or changes the
// index or a run of this session between the steps.
func (p *Proxy) decide(ctx context.Context, t *tool, key string, valid bool, args json.RawMessage) action {
	now := p.now()
	// Arguments the runtime refuses as JSON have no key and match no
	// other call; no rule can apply, and the gate records them invalid.
	if !valid {
		return p.begin(ctx, t, "", args, nil, false, nil, agentrt.OriginModel, now)
	}
	open, recent, err := p.idx.forKey(ctx, p.session, key, now.Add(-t.window))
	if err != nil {
		return p.unavailable(err, notRead)
	}
	// Rules 1 to 3 need the request's own run, behind the key's open row,
	// unfinished. A row whose run never began is taken over by beginning
	// that run (reuse); one whose run finished passes.
	reuse := false
	if open != nil {
		st, err := p.inspect(ctx, open.runID)
		if err != nil {
			return p.unavailable(err, notRead)
		}
		switch {
		case !st.exists:
			reuse = true
		case !st.run.Status.Terminal():
			if a, done := p.unfinished(ctx, t, open, st, now); done {
				return a
			}
			// It finished here, by an approval's expiry or the end of a
			// recovered run. It must have: unless a write that would have
			// ended it failed, in which case nothing new starts.
			if st, err = p.inspect(ctx, open.runID); err != nil {
				return p.unavailable(err, notRead)
			}
			if !st.run.Status.Terminal() {
				return answer(inProgressText())
			}
		}
	}
	// Rules 4 and 5 need a finished run of the request within the window.
	if a, done, err := p.refusal(ctx, t, recent, now); err != nil {
		return p.unavailable(err, notRead)
	} else if done {
		return a
	}
	// Rule 6 needs a tool that is not read-only whose last attempt at this
	// request, at any age, has an unknown outcome: the new run asks an
	// operator.
	var prior *attempt
	if t.class != agentrt.ReadOnly {
		if prior, err = p.unresolved(ctx, key, now); err != nil {
			return p.unavailable(err, notRead)
		}
	}
	// The session's rules need a call that is not read-only or that needs
	// an approval.
	if a, done := p.sessionRules(ctx, t, key, prior != nil, now); done {
		return a
	}
	// Otherwise a new run begins.
	return p.begin(ctx, t, key, args, open, reuse, prior, agentrt.OriginModel, now)
}

// unfinished applies rules 1 to 3 to the request's own run that has not
// finished. done is false when the run finished here, by an approval's
// expiry or the end of a recovered run, for the refusals to judge.
func (p *Proxy) unfinished(ctx context.Context, t *tool, r *row, st runState, now time.Time) (action, bool) {
	if st.run.Status != agentrt.StatusWaitingForApproval {
		// Rule 2: another call executes it under a live lease. Rule 3: the
		// call that ran it is gone.
		if st.leased() {
			return answer(inProgressText()), true
		}
		return p.recover(ctx, t, r)
	}
	// Rule 1: the run waits on an approval of this request.
	a := st.latest()
	if a == nil {
		return answer(inProgressText()), true
	}
	switch a.Status {
	case agentrt.ApprovalPending:
		if a.ExpiresAt.IsZero() || !now.After(a.ExpiresAt) {
			return action{pending: a}, true
		}
		// Attach expires it and cancels the run. Its error is ignored: the
		// run is read again before anything else, and a run still waiting
		// answers in progress.
		if s, _, err := p.gate.Attach(ctx, r.runID); err == nil {
			s.Close()
		}
		return action{}, false
	case agentrt.ApprovalApproved:
		return p.collect(ctx, t, r)
	}
	return answer(inProgressText()), true
}

// collect attaches to a run whose approval was granted: policy is
// evaluated again and the grant's age judged, and what executes is the
// recorded request, never the bytes of this call.
func (p *Proxy) collect(ctx context.Context, t *tool, r *row) (action, bool) {
	s, v, err := p.gate.Attach(ctx, r.runID)
	if err != nil {
		var leased agentrt.ErrRunLeased
		switch {
		case errors.As(err, &leased):
			return answer(inProgressText()), true
		case errors.Is(err, agentrt.ErrRunState):
			return action{retry: true}, true
		}
		return p.unavailable(err, "the gate could not collect the approved request, and nothing was executed"), true
	}
	switch v.Outcome {
	case agentrt.VerdictAllowed:
		var a *agentrt.Approval
		if approvals, err := p.store.ListApprovals(ctx, r.runID); err == nil && len(approvals) > 0 {
			a = &approvals[len(approvals)-1]
		}
		return action{exec: &execution{s: s, st: s.Current(), t: t, approval: a}}, true
	case agentrt.VerdictPending:
		p.operatorHint(v.Approval, t)
		return action{pending: v.Approval}, true
	case agentrt.VerdictDenied:
		return p.denied(ctx, s, v), true
	case agentrt.VerdictEnded:
		return action{}, false
	}
	s.Close()
	return action{retry: true}, true
}

// recover attaches to a run left running by a call that is gone, whose
// lease has expired: a tool that is not read-only cut off while executing
// pauses for an operator, and the call is told so; anything else, a read
// cut off, a call cut off before it executed, or one cut off after its
// outcome was recorded, leaves nothing to execute, and the run is ended.
func (p *Proxy) recover(ctx context.Context, t *tool, r *row) (action, bool) {
	s, v, err := p.gate.Attach(ctx, r.runID)
	if err != nil {
		var leased agentrt.ErrRunLeased
		switch {
		case errors.As(err, &leased):
			return answer(inProgressText()), true
		case errors.Is(err, agentrt.ErrRunState):
			return action{retry: true}, true
		}
		return p.unavailable(err, "the gate could not recover an earlier attempt of this request, and nothing was executed"), true
	}
	switch v.Outcome {
	case agentrt.VerdictPending:
		p.operatorHint(v.Approval, t)
		p.logf("run %s: a call to %s was cut off while executing; its outcome is unknown", r.runID, t.name)
		return action{pending: v.Approval}, true
	case agentrt.VerdictEnded:
		return action{}, false
	}
	p.finish(ctx, s, closeRecovered)
	return action{}, false
}

// refusal applies rules 4 and 5 to the request's finished runs within the
// window, newest first, and settles each one it reads. The newest that
// decides anything decides: one rejected, cancelled, or expired refuses
// the call, except one expired on a question about an unknown outcome,
// which rule 6 asks again; for a tool that is not read-only, one whose
// tool executed with a known outcome, or whose result went unrecorded,
// refuses it as a duplicate, and one whose outcome is unknown refuses
// nothing, because rule 6 asks an operator instead. A run
// whose tool never started, denied or invalid, decides nothing.
func (p *Proxy) refusal(ctx context.Context, t *tool, recent []row, now time.Time) (action, bool, error) {
	if t.window <= 0 {
		return action{}, false, nil
	}
	for i := range recent {
		r := &recent[i]
		st, err := p.inspect(ctx, r.runID)
		if err != nil {
			return action{}, false, err
		}
		if !st.exists || !st.run.Status.Terminal() {
			continue
		}
		o, _ := p.lastAttempt(st)
		p.settle(ctx, r, o, now)
		end := st.run.FinishedAt
		if end.IsZero() || end.Before(now.Add(-t.window)) {
			continue
		}
		until := end.Add(t.window)
		a := st.latest()
		switch st.run.Reason {
		case agentrt.ReasonApprovalRejected:
			if a != nil {
				return answer(rejectedText(a.ID, a.Note, until)), true, nil
			}
		case agentrt.ReasonApprovalExpired:
			// An expiry resolves no unknown outcome: rule 6 asks again.
			if o == outcomeUnknown {
				return action{}, false, nil
			}
			if a != nil {
				return answer(expiredText(a.ID, until)), true, nil
			}
		case agentrt.ReasonOperatorCancelled:
			return answer(cancelledText(st.run.ReasonDetail, until)), true, nil
		}
		if t.class == agentrt.ReadOnly {
			continue
		}
		switch o {
		case outcomeKnown:
			if s := executedStep(st); s != nil {
				return answer(duplicateText(end, t.window, knownOutcome(*s))), true, nil
			}
		case outcomeUnknown:
			return action{}, false, nil
		}
	}
	return action{}, false, nil
}

// settle records a terminal run's outcome in its row. An error is logged
// and otherwise ignored: the row stays unsettled, so every scan reads the
// run itself, as it would have before.
func (p *Proxy) settle(ctx context.Context, r *row, o outcome, now time.Time) {
	if r.outcome != outcomeUnseen {
		return
	}
	if err := p.idx.settle(ctx, r.id, o, now); err != nil {
		p.logf("error: %v", err)
	}
}

// settleRun settles a run's row once the run is terminal.
func (p *Proxy) settleRun(ctx context.Context, runID string) {
	r, err := p.idx.byRun(ctx, runID)
	if err != nil {
		p.logf("error: %v", err)
		return
	}
	st, err := p.inspect(ctx, runID)
	if err != nil || !st.exists || !st.run.Status.Terminal() {
		return
	}
	o, _ := p.lastAttempt(st)
	p.settle(ctx, r, o, p.now())
}

// unresolved applies rule 6: the last attempt at this request, in its
// newest run that made one, whatever its age, when its outcome is unknown.
// An attempt that executed with a known outcome since resolves it; an
// operator's rejection of a re-run does not, so the identical call asks
// an operator again rather than run on the policy alone.
func (p *Proxy) unresolved(ctx context.Context, key string, now time.Time) (*attempt, error) {
	rows, err := p.idx.attempts(ctx, p.session, key)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		r := &rows[i]
		if r.outcome == outcomeKnown {
			return nil, nil
		}
		st, err := p.inspect(ctx, r.runID)
		if err != nil {
			return nil, err
		}
		if !st.exists || !st.run.Status.Terminal() {
			// Only the key's open row can be unfinished, and rules 1 to 3
			// have dealt with it.
			continue
		}
		o, at := p.lastAttempt(st)
		p.settle(ctx, r, o, now)
		switch o {
		case outcomeKnown:
			return nil, nil
		case outcomeUnknown:
			return at, nil
		}
	}
	return nil, nil
}

// sessionRules blocks a new call to a tool that is not read-only while
// another request to the same tool has an unknown outcome no operator has
// resolved: an interruption approval of it pending, or approved and not
// yet collected, blocks the call; an interruption approval past its expiry
// is expired here, and with any other unknown outcome left with nothing
// waiting it is asked about again (unknownBlock). It blocks a new call
// that needs approval while the session's approvals are at the cap. A run
// of the same tool left running past its lease is recovered first. Rows
// read on the way are settled: a finished run's, and one whose run never
// began.
func (p *Proxy) sessionRules(ctx context.Context, t *tool, key string, rerun bool, now time.Time) (action, bool) {
	outcome, _ := p.decider.decide(t)
	needsApproval := outcome == agentrt.RequireApproval || rerun && outcome != agentrt.Deny
	if t.class == agentrt.ReadOnly && !needsApproval {
		return action{}, false
	}
	rows, err := p.idx.unsettled(ctx, p.session)
	if err != nil {
		return p.unavailable(err, notRead), true
	}
	pending := 0
	for i := range rows {
		r := &rows[i]
		if r.key == key {
			continue
		}
		st, err := p.inspect(ctx, r.runID)
		if err != nil {
			return p.unavailable(err, notRead), true
		}
		if !st.exists {
			// The call that claimed it is gone, or about to begin its run;
			// if it does, it unsettles the row (begin).
			if err := p.idx.settleEmpty(ctx, r.id, now); err != nil {
				p.logf("error: %v", err)
			}
			continue
		}
		if st.run.Status.Terminal() {
			o, _ := p.lastAttempt(st)
			p.settle(ctx, r, o, now)
			continue
		}
		sameTool := t.class != agentrt.ReadOnly && r.tool == t.name
		if st.run.Status == agentrt.StatusWaitingForApproval {
			a := st.latest()
			if a == nil {
				continue
			}
			live := a.Status == agentrt.ApprovalPending && (a.ExpiresAt.IsZero() || !now.After(a.ExpiresAt)) ||
				a.Status == agentrt.ApprovalApproved && (st.run.Limits.GrantTTL <= 0 || !now.After(a.DecidedAt.Add(st.run.Limits.GrantTTL)))
			if sameTool && a.Kind == agentrt.InterruptedSideEffect {
				if live {
					return answer(blockedInterruptedText(t.name, a.ID)), true
				}
				// Expired: Attach expires it and ends the run, and an expiry
				// resolves nothing, so unknownBlock asks again below.
				if s, _, err := p.gate.Attach(ctx, r.runID); err == nil {
					s.Close()
				}
				continue
			}
			if live && a.Status == agentrt.ApprovalPending {
				pending++
			}
			continue
		}
		if sameTool && !st.leased() {
			if a, done := p.recover(ctx, t, r); done {
				if a.pending != nil {
					return answer(blockedInterruptedText(t.name, a.pending.ID)), true
				}
				if a.retry || a.result != nil {
					return answer(inProgressText()), true
				}
			}
		}
	}
	if t.class != agentrt.ReadOnly {
		if a, done := p.unknownBlock(ctx, t, key, now); done {
			return a, true
		}
	}
	if needsApproval && pending >= p.maxPending {
		return answer(blockedPendingText(p.maxPending)), true
	}
	return action{}, false
}

// unknownBlock blocks a call to a tool that is not read-only while another
// request to the tool has a last attempt whose outcome is unknown, in a
// finished run, and no operator has resolved it: no attempt since has a
// known outcome, and its run did not end on an operator's rejection or
// cancellation. Such a request has nothing waiting for an operator, its
// re-run having met the run's limit, its run recovered after a crash, or
// its approval expired, so it is asked about again, in a fresh run, and
// the call is blocked on that approval: nothing is blocked with nothing
// for an operator to act on. A tool the policy denies is not blocked: the
// call is denied anyway.
func (p *Proxy) unknownBlock(ctx context.Context, t *tool, key string, now time.Time) (action, bool) {
	if o, _ := p.decider.decide(t); o == agentrt.Deny {
		return action{}, false
	}
	rows, err := p.idx.unknownForTool(ctx, p.session, t.name, key)
	if err != nil {
		return p.unavailable(err, notRead), true
	}
	for i := range rows {
		r := &rows[i]
		st, err := p.inspect(ctx, r.runID)
		if err != nil {
			return p.unavailable(err, notRead), true
		}
		// A run not finished is in flight or waiting, which the scan of
		// unsettled rows has judged.
		if !st.exists || !st.run.Status.Terminal() {
			continue
		}
		o, at := p.lastAttempt(st)
		p.settle(ctx, r, o, now)
		if o != outcomeUnknown || st.run.Reason == agentrt.ReasonApprovalRejected || st.run.Reason == agentrt.ReasonOperatorCancelled {
			continue
		}
		a := p.reopen(ctx, t, r.key, lastArgs(st), at, now)
		if a == nil {
			return answer(unavailableText("the gate could not ask an operator about an earlier call to this tool whose outcome is unknown, and nothing was executed")), true
		}
		return answer(blockedInterruptedText(t.name, a.ID)), true
	}
	return action{}, false
}

// reopen asks an operator, in a fresh run of the request, whether a
// request whose last attempt's outcome is unknown runs again, when nothing
// about it waits: the proxy proposes the recorded request with origin
// operator, and the policy asks with an interruption approval naming the
// attempt (rerunOf). It returns that approval, or nil when none could be
// asked for, which is logged.
func (p *Proxy) reopen(ctx context.Context, t *tool, key string, args json.RawMessage, at *attempt, now time.Time) *agentrt.Approval {
	open, _, err := p.idx.forKey(ctx, p.session, key, now)
	if err != nil {
		p.logf("error: %v", err)
		return nil
	}
	reuse := false
	if open != nil {
		st, err := p.inspect(ctx, open.runID)
		if err != nil {
			p.logf("error: %v", err)
			return nil
		}
		switch {
		case !st.exists:
			reuse = true
		case !st.run.Status.Terminal():
			p.logf("error: run %s: asking again about an unknown outcome: the request's run %s has not finished", at.RunID, open.runID)
			return nil
		}
	}
	a := p.begin(ctx, t, key, args, open, reuse, at, agentrt.OriginOperator, now)
	if a.exec != nil {
		// The policy asks for an interruption short of a denial, so this
		// is never reached; the step is left unexecuted.
		a.exec.s.Close()
	}
	if a.pending == nil {
		p.logf("error: run %s: asking again about an unknown outcome: no approval was opened", at.RunID)
	}
	return a.pending
}

// lastArgs are the arguments of a run's last tool call, as recorded.
func lastArgs(st runState) json.RawMessage {
	for i := len(st.steps) - 1; i >= 0; i-- {
		if d := st.steps[i].Decision; d != nil && d.Kind == agentrt.DecideToolCall {
			return orEmpty(d.Args)
		}
	}
	return json.RawMessage("{}")
}

// begin opens the request's run and proposes the call. A new run claims a
// row for the key first, closing the open row of a finished run; a row
// whose run never began is reused, its run begun under its id, and of two
// calls that try, the store lets one begin it and the other decides
// again. Arguments the runtime refuses as JSON (key "") get a row of
// their own that no call matches. prior, when set, is the attempt whose
// outcome is unknown that this run re-runs (rule 6). origin is the
// decision's: the model's call, or the proxy's own question (reopen).
func (p *Proxy) begin(ctx context.Context, t *tool, key string, args json.RawMessage, open *row, reuse bool, prior *attempt, origin agentrt.DecisionOrigin, now time.Time) action {
	const notStarted = "the gate could not start this request, and nothing was executed"
	runID := p.session + "." + newSuffix()
	if reuse {
		runID = open.runID
	} else {
		prev := open
		if key == "" {
			key, prev = "invalid:"+runID, nil
		}
		if err := p.idx.claim(ctx, p.session, key, t.name, t.class, runID, prev, now); err != nil {
			if errors.Is(err, errRace) {
				return action{retry: true}
			}
			return p.unavailable(err, "the gate could not record this request, and nothing was executed")
		}
	}
	s, err := p.gate.Begin(ctx, runID, "call "+t.name, p.limits)
	if err != nil {
		if _, gerr := p.store.GetRun(ctx, runID); gerr == nil {
			// Another call began this row's run first.
			return action{retry: true}
		}
		return p.unavailable(err, notStarted)
	}
	// A scan may have settled the row while its run had not begun.
	if err := p.idx.unsettle(ctx, runID); err != nil {
		// The run has no step yet; the next identical call ends it.
		s.Close()
		return p.unavailable(err, notStarted)
	}
	st, err := s.Step(ctx)
	if err != nil || st == nil {
		s.Close()
		return p.unavailable(fmt.Errorf("run %s: no step: %v", runID, err), notStarted)
	}
	if prior != nil {
		p.setRerun(runID, prior)
		defer p.setRerun(runID, nil)
	}
	v, err := st.Propose(ctx, agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: t.name, Args: args, Origin: origin})
	if err != nil {
		s.Close()
		return p.unavailable(err, "the gate could not record this request, and nothing was executed")
	}
	return p.verdict(ctx, s, st, t, v)
}

// verdict acts on what the gate answered a proposal: an allowed request
// is executed, a pending one held for, and a denied or invalid one ended.
func (p *Proxy) verdict(ctx context.Context, s *agentrt.Session, st *agentrt.OpenStep, t *tool, v agentrt.Verdict) action {
	switch v.Outcome {
	case agentrt.VerdictAllowed:
		return action{exec: &execution{s: s, st: st, t: t}}
	case agentrt.VerdictPending:
		p.operatorHint(v.Approval, t)
		return action{pending: v.Approval}
	case agentrt.VerdictDenied:
		return p.denied(ctx, s, v)
	}
	// Ended: the proxy's policy never aborts, so this is a run a limit or
	// an operator ended.
	s.Close()
	p.settleRun(ctx, v.Run.ID)
	return answer(deniedText(v.Run.ReasonDetail))
}

// setRerun names, or with nil forgets, the attempt a run's proposal
// re-runs, for the policy (rerunOf).
func (p *Proxy) setRerun(runID string, at *attempt) {
	p.reruns.Lock()
	defer p.reruns.Unlock()
	if at == nil {
		delete(p.reruns.m, runID)
		return
	}
	p.reruns.m[runID] = at
}

// denied ends the run of a call the policy denied or the runtime found
// invalid, and says which.
func (p *Proxy) denied(ctx context.Context, s *agentrt.Session, v agentrt.Verdict) action {
	var c struct {
		Reason string `json:"reason"`
		Error  string `json:"error"`
	}
	if v.Observation != nil {
		json.Unmarshal(v.Observation.Content, &c)
	}
	runID := s.Run().ID
	defer p.settleRun(ctx, runID)
	if v.Observation != nil && v.Observation.Kind == agentrt.ObserveInvalidDecision {
		p.finish(ctx, s, closeInvalid)
		return answer(invalidText(c.Error))
	}
	p.finish(ctx, s, closeDenied)
	return answer(deniedText(c.Reason))
}

// finish ends a run whose call is over and closes its session: a run its
// tool call completed is already over, and any other is ended with the
// proxy's fail decision.
func (p *Proxy) finish(ctx context.Context, s *agentrt.Session, why string) {
	defer s.Close()
	if s.Done() {
		return
	}
	st, err := s.Step(ctx)
	if err != nil || st == nil {
		return
	}
	if _, err := st.Propose(ctx, agentrt.Decision{Kind: agentrt.DecideFail, Message: why, Origin: agentrt.OriginOperator}); err != nil {
		p.logf("run %s: closing: %v", s.Run().ID, err)
	}
}

// execute runs an allowed request through the gate and answers with what
// the gate recorded. A call to a tool that is not read-only whose outcome
// is unknown is not ended: in the same run the proxy proposes it again,
// which waits for an operator (rerun).
func (p *Proxy) execute(ctx context.Context, e *execution) *sdk.CallToolResult {
	obs, err := e.st.Execute(ctx)
	runID := e.s.Run().ID
	if err != nil && obs.Kind == "" {
		e.s.Close()
		switch {
		case errors.Is(err, agentrt.ErrApprovalExpired):
			id := ""
			if e.approval != nil {
				id = e.approval.ID
			}
			p.settleRun(ctx, runID)
			return expiredText(id, p.now().Add(e.t.window))
		case errors.Is(err, agentrt.ErrRunState), errors.Is(err, agentrt.ErrLeaseLost):
			return inProgressText()
		}
		p.logf("error: %v", err)
		return unavailableText("the gate could not start this request, and nothing was executed")
	}
	if err != nil {
		p.logf("run %s: %v", runID, err)
	}
	if obs.Kind == agentrt.ObserveToolError {
		if ended, unknown := p.unknownEnding(e.t, obs); unknown {
			return p.rerun(ctx, e, ended, obs)
		}
		if unrecorded(e.t, obs) {
			p.finish(ctx, e.s, closeUnrecorded)
			p.settleRun(ctx, runID)
			return unrecordedText(errorText(obs.Content), e.t.window)
		}
		p.finish(ctx, e.s, closeToolError)
	} else {
		e.s.Close()
	}
	p.settleRun(ctx, runID)
	return resultOf(obs)
}

// unrecorded reports a call to a tool that is not read-only whose server
// answered with content the runtime refused (invalid_content): not usable
// JSON, nested too deep, or a number past its bound. The server answered,
// so the call executed, and it is not a failure the model may retry: it
// counts as executed, for the duplicate rule as for every other, and the
// model is told its result could not be recorded. It is not an unknown
// outcome either: the server is known to have answered, so asking an
// operator whether to run it again would invite the repeat the duplicate
// rule exists to stop.
func unrecorded(t *tool, obs agentrt.Observation) bool {
	return t.class != agentrt.ReadOnly && obs.Kind == agentrt.ObserveToolError && failureOf(obs) == "invalid_content"
}

func failureOf(obs agentrt.Observation) string {
	var c struct {
		Failure string `json:"failure"`
	}
	json.Unmarshal(obs.Content, &c)
	return c.Failure
}

// rerun follows a call to a tool that is not read-only whose outcome is
// unknown: the server may have acted. The run is not ended: the proxy
// proposes the recorded request again in it, with origin operator, and
// the policy asks an operator whether it runs again (rerunOf), so the
// operator sees it at once, the tool is blocked until it is decided, and
// the model's identical call collects it. The model is told the outcome
// is unknown. If the run's limit ends it first, which a re-run whose
// outcome is unknown again meets, the question is asked in a fresh run of
// the request (reopen); if that fails too, the next call to the tool asks
// it (unknownBlock, rule 6).
func (p *Proxy) rerun(ctx context.Context, e *execution, ended string, obs agentrt.Observation) *sdk.CallToolResult {
	runID, stepID := e.s.Run().ID, e.st.ID()
	how := attempt{Ended: ended}.how()
	text := errorText(obs.Content)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logf("run %s: a call to %s %s; its outcome is unknown", runID, e.t.name, how)
	steps, err := p.store.ListSteps(ctx, runID)
	var executed *agentrt.Step
	for i := range steps {
		if steps[i].ID == stepID {
			executed = &steps[i]
		}
	}
	if err != nil || executed == nil || executed.Decision == nil {
		p.logf("error: run %s: reading the attempt: %v", runID, err)
		e.s.Close()
		return unknownText(how, text, "")
	}
	at := &attempt{RunID: runID, StepID: stepID, StartedAt: stamp3339(executed.StartedAt), Ended: ended}
	st, err := e.s.Step(ctx)
	if err != nil || st == nil {
		// The run's limit ended it: the question is asked in a fresh run.
		e.s.Close()
		p.settleRun(ctx, runID)
		if a := p.reopenRun(ctx, e.t, executed.Decision.Args, at); a != nil {
			return unknownText(how, text, a.ID)
		}
		p.logf("error: run %s: no step for the re-run: %v", runID, err)
		return unknownText(how, text, "")
	}
	p.setRerun(runID, at)
	defer p.setRerun(runID, nil)
	v, err := st.Propose(ctx, agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: e.t.name, Args: orEmpty(executed.Decision.Args), Origin: agentrt.OriginOperator})
	if err != nil {
		p.logf("error: run %s: proposing the re-run: %v", runID, err)
		e.s.Close()
		return unknownText(how, text, "")
	}
	if v.Outcome == agentrt.VerdictPending {
		p.operatorHint(v.Approval, e.t)
		return unknownText(how, text, v.Approval.ID)
	}
	// Denied by the policy now: nothing waits. Ended by a limit: the
	// question is asked in a fresh run.
	if v.Outcome == agentrt.VerdictDenied {
		p.denied(ctx, e.s, v)
		return unknownText(how, text, "")
	}
	e.s.Close()
	p.settleRun(ctx, runID)
	if a := p.reopenRun(ctx, e.t, executed.Decision.Args, at); a != nil {
		return unknownText(how, text, a.ID)
	}
	return unknownText(how, text, "")
}

// reopenRun is reopen for the request a run's attempt made, keyed from its
// recorded arguments.
func (p *Proxy) reopenRun(ctx context.Context, t *tool, args json.RawMessage, at *attempt) *agentrt.Approval {
	args = orEmpty(args)
	key, valid := requestKey(t.name, args)
	if !valid {
		return nil
	}
	return p.reopen(ctx, t, key, args, at, p.now())
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

// knownError matches the recorded error of a call the server answered, or
// one the mcp package refused before sending, for one tool: an isError
// result is recorded as the server and tool, the content's size, and the
// server's text; a refusal names the registered tool. Any other error of
// a call that was sent, a transport or protocol failure, may have followed
// the server acting, and is not known.
func knownError(t *tool) *regexp.Regexp {
	server := regexp.QuoteMeta(t.server + "/" + t.remote)
	name := regexp.QuoteMeta(t.name)
	return regexp.MustCompile(`^(?:` + server + `: \d+ bytes(?:, truncated)?(?:, structured content of \d+ bytes dropped for the text)?, error: ` +
		`|` + name + `: (?:arguments are not a JSON object|parameter ".*" is (?:denied|fixed) by the operator|the server asked for more input))`)
}

// unknownEnding reports whether a recorded tool error of a tool that is
// not read-only leaves the call's outcome unknown, and how it ended: a
// timeout, or an error that is not one knownError matches. Content the
// server returned that the runtime refused (invalid_content) is an answer.
func (p *Proxy) unknownEnding(t *tool, obs agentrt.Observation) (string, bool) {
	if t.class == agentrt.ReadOnly || obs.Kind != agentrt.ObserveToolError {
		return "", false
	}
	var c struct {
		Error   string `json:"error"`
		Failure string `json:"failure"`
	}
	if json.Unmarshal(obs.Content, &c) != nil {
		return endedNoAnswer, true
	}
	switch c.Failure {
	case "timeout":
		return endedTimeout, true
	case "invalid_content":
		return "", false
	}
	if t.known.MatchString(c.Error) {
		return "", false
	}
	return endedNoAnswer, true
}

// lastAttempt is what a finished run's last attempt at its tool came to,
// and, when unknown, the attempt to name: a result or an error the server
// answered is known; a step cut off while executing, or a tool error that
// leaves the outcome unknown, is unknown; and so is a re-run waiting on, or
// rejected or expired on, an interruption approval, which names the attempt
// that approval was about. A step that never executed, denied or invalid
// or never approved, or cut off before its tool started, is passed over.
func (p *Proxy) lastAttempt(st runState) (outcome, *attempt) {
	for i := len(st.steps) - 1; i >= 0; i-- {
		s := st.steps[i]
		if s.Decision == nil || s.Decision.Kind != agentrt.DecideToolCall {
			continue
		}
		t := p.tools[s.Decision.Tool]
		at := &attempt{RunID: st.run.ID, StepID: s.ID, StartedAt: stamp3339(s.StartedAt), Ended: endedCutOff}
		if interruptedExecuting(s) {
			return outcomeUnknown, at
		}
		if interruptedBeforeExecuting(s) {
			// Cut off before its tool started: nothing ran.
			continue
		}
		if o := s.Observation; o != nil {
			switch o.Kind {
			case agentrt.ObserveToolResult:
				return outcomeKnown, nil
			case agentrt.ObserveInterrupted:
				return outcomeUnknown, at
			case agentrt.ObserveToolError:
				if t == nil {
					// A tool no longer offered: nothing tells its answers apart.
					at.Ended = endedNoAnswer
					return outcomeUnknown, at
				}
				if ended, unknown := p.unknownEnding(t, *o); unknown {
					at.Ended = ended
					return outcomeUnknown, at
				}
				return outcomeKnown, nil
			}
		}
		if a := latestFor(st.approvals, s.ID); a != nil && a.Kind == agentrt.InterruptedSideEffect {
			var c capability
			if json.Unmarshal(a.Capability, &c) == nil && c.Interrupted != nil {
				return outcomeUnknown, c.Interrupted
			}
			return outcomeUnknown, at
		}
	}
	return outcomeNone, nil
}

// executedStep is the run's last tool step that executed, for the
// duplicate rule's answer.
func executedStep(st runState) *agentrt.Step {
	for i := len(st.steps) - 1; i >= 0; i-- {
		if o := st.steps[i].Observation; o != nil && (o.Kind == agentrt.ObserveToolResult || o.Kind == agentrt.ObserveToolError) {
			return &st.steps[i]
		}
	}
	return nil
}

// knownOutcome is what came of a step that executed, as the model is told
// it in prose: escaped and bounded, since the server chose it.
func knownOutcome(st agentrt.Step) string {
	switch {
	case st.Observation.Kind == agentrt.ObserveToolResult:
		return "it succeeded, with this result: " + clean(resultText(st.Observation.Content), maxResult)
	case failureOf(*st.Observation) == "invalid_content":
		return "the server answered, but its result could not be recorded: " + clean(errorText(st.Observation.Content), 500)
	}
	return "it failed, with this error: " + clean(errorText(st.Observation.Content), maxResult)
}

// resultOf maps a recorded observation to a tool result: the mcp
// package's recording, structured content when it is an object and text
// otherwise, passed back as data as the server sent it; and an error
// result carrying the recorded error text, escaped, since it is prose the
// proxy writes.
func resultOf(obs agentrt.Observation) *sdk.CallToolResult {
	switch obs.Kind {
	case agentrt.ObserveToolResult:
		res := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: resultText(obs.Content)}}}
		if isObject(obs.Content) {
			res.StructuredContent = json.RawMessage(obs.Content)
		}
		return res
	case agentrt.ObserveToolError:
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: clean(errorText(obs.Content), maxResult)}}}
	}
	return unavailableText("the call's outcome was not recorded")
}

// resultText is a recorded result as text: a JSON string as its contents,
// anything else as its JSON.
func resultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	return string(content)
}

func errorText(content json.RawMessage) string {
	var c struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(content, &c) == nil && c.Error != "" {
		return c.Error
	}
	return string(content)
}

func isObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}

// wait holds a call for up to the hold for its approval to be decided,
// polling the store, and sends the host progress notifications when the
// call carried a progress token. It reports whether the approval was
// decided, or its expiry passed, so the call should be decided again.
func (p *Proxy) wait(ctx context.Context, ss *sdk.ServerSession, token any, a agentrt.Approval) bool {
	start := time.Now()
	deadline := time.NewTimer(p.hold)
	defer deadline.Stop()
	tick := time.NewTicker(p.poll)
	defer tick.Stop()
	lastProgress := start
	for {
		select {
		case <-ctx.Done():
			return false
		case <-p.stopping:
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
		cur, err := p.store.GetApproval(context.WithoutCancel(ctx), a.RunID, a.ID)
		if err == nil && (cur.Status != agentrt.ApprovalPending || !cur.ExpiresAt.IsZero() && p.now().After(cur.ExpiresAt)) {
			return true
		}
		if token != nil && ss != nil && time.Since(lastProgress) >= time.Second {
			lastProgress = time.Now()
			ss.NotifyProgress(ctx, &sdk.ProgressNotificationParams{
				ProgressToken: token,
				Progress:      time.Since(start).Seconds(),
				Total:         p.hold.Seconds(),
				Message:       "waiting for an operator's approval",
			})
		}
	}
}
