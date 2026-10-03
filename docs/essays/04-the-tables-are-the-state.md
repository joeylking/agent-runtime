# The tables are the state, the events are the explanation

An agent that touches a real system has to be able to answer two questions
afterwards: what did it do, and who allowed it. The usual answer is an
audit log. This essay is about how agent-runtime keeps its audit log
honest, and about the narrower question of what such a log can prove.

Some terms first. A run is one execution of an agent toward a goal. A
step is one decision by the agent and what came of it: a tool call that
was allowed, denied, or paused for a person's approval, and its result.
An event is one line of the audit log, such as `step.tool_started` or
`approval.decided`.

## Two common designs, and the one chosen

The first common design writes the log beside the state. The program
updates its records, then appends a log line, or the other way round.
Between the two writes there is a window. A crash in that window leaves
a state the log does not explain, or a log line for a change that never
happened. Over time the log drifts from the truth, and nobody can say
which one to believe.

The second design is event sourcing. The events are the only truth, and
the current state is rebuilt by replaying them. Nothing can drift,
because there is only one record. The price is that every change to an
event's shape needs a migration of old events or code to upgrade them as
they are read, and the current state of a run is the result of a replay
rather than a query.

[ADR 2](../decisions/0002-persisted-state-not-event-sourcing.md) takes
neither. The current state lives in ordinary tables: `runs`, `steps`,
`approvals`, and `model_calls`. The events are appended in the same
transaction as the state change they describe, and the runtime never reads
them to decide what to do. The ADR's reasons are practical. A run is
resumed a few times at most and never needs replaying into alternative
histories. Table migrations are simpler and safer than versioned event
payloads. The current state is one query away.

## One transaction per transition

Every state change the runtime makes is one SQLite transaction that
updates the rows and appends the events explaining them
([`store.go`](../../store.go)). It commits whole or not at all. A panic
inside it rolls it back
([`TestStore_PanicInTransactionRollsBack`](../../store_internal_test.go)).
An observer, such as a trace printer, sees the events only after the
commit. The basic test checks the exact sequence of events for a whole
run, and that the observer saw precisely the events the database holds
([`TestDriver_CompletesAndPersists`](../../driver_test.go)).

A step that ends the run is written in the run's own transaction, so a
crash cannot leave a finished final step under a run that still looks
active ([`TestDriver_TerminalToolCommitsWithTheRun`](../../integrity_test.go)).
Status changes are compare-and-set inside the transaction: a write
succeeds only if the run is still in the state the writer expects. So two
processes resuming one approved run execute its action once
([`TestResume_ConcurrentResumeExecutesOnce`](../../integrity_test.go)),
and an operator's cancel is never overwritten by a loop that has not yet
noticed it
([`TestCancel_LiveLoopStopsAtItsNextWrite`](../../integrity_test.go)).

## Why the log cannot drift from the state

There is no window, because there is no second write. The step's new
status and the event saying so are one commit. In particular, the record
that a tool started, the step marked as executing together with
`step.tool_started`, commits before the tool is called. So the runtime
cannot act without the event that says it did.

That guarantee is only as good as the disk. In SQLite's write-ahead mode,
the ordinary sync level can lose the last commits on power loss, and the
last commit may be the `step.tool_started` of a tool that ran. Losing it
would let the next process treat a tool that ran as one that never
started. So every commit is synced. That costs one `fsync` per
transaction and four per step, about 20 microseconds each on the
benchmark machine and milliseconds on a disk that waits for the medium,
and the cost was taken on purpose ([`docs/performance.md`](../performance.md)).

The ADR's tradeoff is two writes per transition, rows and events, kept in
step by construction rather than by care.

## The late `tool_finished`

One case shows why the split between tables and events earns its keep. An
operator can cancel a run at any time, including while a tool is running.
The cancel ends the run as `operator_cancelled` and marks the step in
flight as failed. It cannot recall the tool, which may be halfway through
moving money.

When the tool returns, the loop's write fails, because the run is no
longer running. The outcome is not thrown away. The runtime appends a
`step.tool_finished` event marked `late`, carrying the tool's result,
after the run's `run.finished`. The run stays cancelled and the step keeps
what the cancel wrote
([`TestCancel_ToolOutcomeIsAppendedLate`](../../security_test.go)). In the
test, a transfer tool cancels its own run and then reports that it moved
100; the last event says so.

The tables still give the state, which is cancelled. The events give the
explanation, which includes the fact that the transfer completed after
the cancel. A design with only one of the two would have to lie about one
of them.

## Reading without writing

Because every event commits with its state, the events table is complete,
and something outside the consumer's process can follow it with no other
input. The `export` package does this. A follower opens the database
read-only and reads events in pages after a cursor, the event's sequence
number, which SQLite assigns in commit order across every run
([`export/export.go`](../../export/export.go),
[`TestFollower_DeliversEveryRunInCommitOrder`](../../export/export_test.go)).
It never writes, and the test checks that the file is untouched
([`TestFollower_ReadOnlyLeavesTheFileUntouched`](../../export/export_test.go)).
Every text column is capped, so a huge table or one crafted row cannot
exhaust the follower's memory
([`TestFollower_PagesALargeTable`](../../export/export_test.go)).

The OpenTelemetry exporter built on it maps every event type the runtime
emits, checked against fixtures that run real drivers through a pause, a
crash, a cancel with a late tool result, and a retried model call
([`TestFixtures_EmitEveryEventType`](../../export/otel/otel_test.go),
[`TestExporter_CancelledRunAndLateTool`](../../export/otel/otel_test.go)).
The consumer writes no code for any of it.

Inside the runtime, the events are written and never read for control
flow. The readers are all outside the loop: the operator command, the
exporter, the test kit, and the approval channel, which reads the reason
the policy gave when it paused the run.

## What the record does and does not prove

The record shows the runtime's own sequence: what the agent asked for, in
its exact bytes even when they were malformed; what the policy answered
and why; which approval, by hash, was decided, when, and under what name;
when each tool started and finished and what it returned; and what each
model attempt was charged. Because the rows and events commit together,
those facts agree with each other.

## What this does not claim

- The record says what a tool returned, not what it did. A tool that
  reports success without acting, or acts and reports nothing, is
  recorded as it reported.
- A `step.tool_started` with no matching finish means the outcome is
  unknown, not that the action did not happen.
- The name on an approval decision is a label the caller supplied, not a
  verified identity.
- The log is not tamper-evident. There is no secret and no hash chain.
  Anyone who can write the database file can rewrite history, which is
  why the runtime keeps the file readable and writable by its owner only
  ([`SECURITY.md`](../../SECURITY.md)).
- Timestamps come from the clock the consumer configures, which a
  deterministic test sets on purpose.
- Arguments, results, and approval text are stored verbatim. A tool that
  returns a secret puts it in the log, and the runtime does not redact it.
- Export delivery is exactly once across clean stops. A follower killed
  between handing an event to its sink and saving its cursor delivers
  that event again on restart
  ([`TestFollower_ExactlyOnceAcrossRestarts`](../../export/export_test.go)).
