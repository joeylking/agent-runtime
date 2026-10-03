# When the runtime does not know, a person decides

Processes die. A deploy restarts them, the kernel kills them for memory, a
laptop lid closes, the power goes. An agent built on agent-runtime may
have been in the middle of a tool call that charges a card, pushes a
branch, or sends a message. When the program starts again, something has
to decide what happens to that call. This essay is about how the runtime
makes that decision: what it can know, what it cannot, and who decides
when it cannot.

## What a crash looks like from the database

Everything the runtime knows is in one SQLite file. A run is a row, each
step of the run is a row, and an audit log of events is written in the
same transaction as each change. Before a tool is called, the runtime
commits the step as executing together with a `step.tool_started` event,
and every commit is synced to disk
([the previous essay](04-the-tables-are-the-state.md) explains why).

So a process that died mid-step leaves this behind: a run whose status is
still `RUNNING`, a step still `deciding` or `executing`, an event log
that stops at the last commit, and a lease on the run that nobody is
renewing. A step still `executing` means the tool may have run. The
record cannot say more than that. A crash just after `step.tool_started`
and a crash just after the tool did its work, before its result was
recorded, leave identical rows. The runtime's own test kit documents
exactly that ([`testkit/testkit.go`](../../testkit/testkit.go)).

## The lease: telling dead from alive

A run found `RUNNING` is either being executed right now or was left
behind by a process that died. Before v0.3 nothing in the row told the two
apart, so a second process could take over a run whose loop was still
alive and enter the same step while the first one's tool call was still
running. [ADR 5](../decisions/0005-run-lease.md) adds a lease: an owner id
and an expiry on the run's row
([`lease.go`](../../lease.go)).

- The lease is taken in the transaction that moves the run to `RUNNING`
  and released in the one that pauses or finishes it.
- A heartbeat renews it on the wall clock every third of its length, 30
  seconds by default, whatever the loop is doing, so a long tool call
  keeps it ([`TestLease_LongToolCallKeepsTheLease`](../../lease_test.go)).
  On a file database it renews on a connection of its own, so a consumer
  holding the database connection cannot starve it
  ([`TestLease_HeldConnectionDoesNotStarveTheHeartbeat`](../../security_test.go)).
- Every write the loop makes requires the lease. The writes that start a
  side effect, a tool start or a model request, also require it unexpired
  by the wall clock, so a loop that stalled past its expiry starts nothing
  even if nobody has taken the run
  ([`TestLease_LapsedLeaseStartsNoSideEffect`](../../security_test.go)).
- A loop that loses its lease stops before its next tool call or model
  request and returns `ErrLeaseLost`
  ([`TestLease_LoopThatLosesItsLeaseStopsBeforeTheNextToolCall`](../../lease_test.go),
  [`TestLease_NoModelCallAfterTheLeaseIsLost`](../../lease_test.go)).
- `Resume` of a run whose lease is live and someone else's returns
  `ErrRunLeased` and changes nothing
  ([`TestLease_ResumeOfALiveRunIsRefused`](../../lease_test.go)).
- The stored owner is the configured name plus a random suffix per
  driver, so two replicas configured alike never share a lease
  ([`TestLease_SameLeaseOwnerNameIsTwoOwners`](../../security_test.go)).

Once the lease has expired, `Resume` takes the run over. In one
transaction it takes the lease, records `lease.taken_over`, and marks the
step in flight as interrupted. The dead owner's late result, if its tool
ever returns, is not recorded
([`TestLease_ExpiredLeaseIsTakenOverAndReconciledOnce`](../../lease_test.go)).

## Never twice on its own

What happens next is the interruption contract of
[ADR 6](../decisions/0006-interrupted-side-effects.md). The runtime never
runs an interrupted step again by itself. That was always true, but it
was not enough. Before v0.3, a resumed run showed the agent a step with no
result, and the agent, reasonably, asked for the same call again in a new
step. The effect was the same as a rerun: a payment, a push, or a message
sent twice, with nothing in the record to say the second was a repeat.

Now, when the consumer has supplied no reconciliation, and the step was
executing a tool that is not read only, the run does not continue. It
pauses on that exact request through the ordinary approval mechanism, as
an approval of kind `interrupted_side_effect`. Its presentation says the
call started and the process stopped before its outcome was recorded, so
whether it took effect is unknown. Approving and resuming runs the same
request once more, on the same step, after the same schema check and
policy re-evaluation as any approval. Rejecting ends the run with the tool
not called again
([`resume.go`](../../resume.go),
[`TestResume_InterruptedSideEffectWaitsForAnOperator`](../../security_test.go)).
A step that was executing a read-only tool, or that was still deciding,
continues as before, because running a read again changes nothing
([`TestResume_InterruptedReadOnlyOrDecidingStepContinues`](../../security_test.go)).
A step whose tool is no longer registered, or whose arguments no longer
pass its schema, cannot wait on an approval, so the run fails as
`reconcile_conflict` rather than wait on nothing.

The ADR considered the usual alternative, at-least-once delivery with a
note that every tool must be idempotent, and refused it. Idempotency is a
property of the outside system, not of the runtime or most consumers. A
payment API without idempotency keys, a git push, or an email cannot be
made safe by documentation.

## What reconciliation is for

The runtime cannot tell whether a call took effect. The consumer often
can, from its own records or the outside world. `Config.Reconcile` is
where it says so: given the run as recorded, it returns continue, complete
with a recovered result, wait for an approval it supplies, or fail as a
conflict ([`TestResume_ReconcileOutcomes`](../../interrupted_test.go),
[`TestResume_InterruptedStepIsReconciledAndContinued`](../../interrupted_test.go)).

repo-steward's reconciliation settles an interrupted publication against
the remote, and never re-creates a pull request without first looking for
one carrying its marker
([`internal/publish/publish.go`](https://github.com/joeylking/repo-steward/blob/main/internal/publish/publish.go),
wired in [`internal/steward/agent.go`](https://github.com/joeylking/repo-steward/blob/main/internal/steward/agent.go)).
casework, which is private, gives each proposed change a stable operation
id and looks the operation up in the target application before ever
dispatching it again (`internal/tools/act.go` in casework).

## What the lease cannot do

- **An older binary.** A version from before the lease ignores it
  completely and would resume a run a current process is executing.
  Nothing in the file can stop it, so every older process must be stopped
  before a current one first opens the database
  ([`CHANGELOG.md`](../../CHANGELOG.md)).
- **Clock skew.** Lease expiry compares wall clocks. Processes sharing a
  file on different machines need clocks that agree to well within the
  lease's length, and SQLite on a network filesystem is unsupported.
- **A call already running.** A tool in flight cannot be recalled. The
  lease only stops the next one.
- **Instant takeover.** A process that dies holding a lease blocks
  `Resume` of its run for up to one lease length, even for the same
  program restarted, because a name is not proof the old process is gone.

## Crashing on purpose

A contract like this is only believable if it is tested at every point a
process can die. The `testkit` package does that for consumers. At a
chosen point it parks the goroutine, Go's lightweight thread, executing the run and closes its
database handle, so the heartbeat stops and the lease is never released,
which is exactly what a dead process leaves. It then resumes in a fresh
driver on a fresh open of the file, which is refused until the short
lease expires, as a restarted process would be. It asserts that no step
re-executes on its own, that an approved request executes at most once,
that every tool start is finished or interrupted, that nothing is left in
flight, and that every requested crash was reached
([`testkit/testkit.go`](../../testkit/testkit.go),
[`TestRun_SideEffectLostToACrashRunsAgainOnlyWhenApproved`](../../testkit/testkit_test.go),
[`TestRun_ApprovedRequestSurvivesACrashAfterResume`](../../testkit/testkit_test.go),
[`TestRun_CrashAfterTakeoverStillWaitsForTheOperator`](../../testkit/testkit_test.go)).

The crash points are after `step.started`, after `step.decided`, after
`step.tool_started`, after the tool's effect and before its record, after
`step.tool_finished`, after `run.resumed`, after `step.interrupted`, and
at every pause. casework's crash-recovery tests run on this harness
(`internal/executor/kit_test.go` in casework).

## What this does not claim

- It is not exactly-once delivery to the outside world. An approved
  re-run is a second execution of a call that may already have taken
  effect. The approval says so, and the choice is the operator's.
- The read-only exemption trusts the tool's declared kind of effect,
  which the runtime does not verify.
- The test kit cannot crash inside one transaction, between a tool's own
  journal writes, or during a model request, because the public interface
  reaches none of them ([`docs/status.md`](../status.md)).
- A process that dies during a model request leaves that attempt recorded
  but not charged to the run's totals.
