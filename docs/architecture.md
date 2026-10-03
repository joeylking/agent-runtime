# Architecture

agent-runtime is a library. A consumer supplies an agent, a set of tools,
and a policy; the runtime supplies the loop and every control around it.

## The loop

```
Start(goal, limits)
  └─ loop until terminal or paused
       check limits (steps, failures, loops, time)
       start step ─ record step.started
       agent.Decide(StepInput{run, steps, approvals, tools, model})
       record the decision verbatim
       validate arguments against the tool's JSON schema
       policy.Evaluate(request, run view)
         allow            → execute with timeout → observation
         deny             → observation, counts as a failure
         abort            → run FAILED (policy_abort)
         require_approval → approval row with hash → run WAITING
       terminal tool succeeded → run COMPLETED with its result
```

Every state change is written in the same SQLite transaction as its audit
event, and the observer sees events only after commit. The tables are the
state; the events are the explanation. Nothing is reconstructed by replay.

A step that ends the run is written in the run's transaction, so a crash
cannot leave a finished terminal step under a running run. Run and step
status transitions are compare-and-set inside that transaction: a loop
writes only while its run is RUNNING and it holds the run's lease, Resume
moves a run out of WAITING once, and the loser of either race gets
`ErrRunState`. An operator's Cancel therefore stops a live loop at its
next write. What a tool did or a
model call cost is recorded even when the caller's context is cancelled
after it ran; the cancellation is returned afterwards.

Each recorded state change takes one reading of `Config.Now`, in an order
that does not change between releases: a consumer with a deterministic
clock keys its own records on that sequence.

A loop loads its run's steps and approvals once, when `Start` or `Resume`
enters it, and keeps them. After each write commits, the loop decodes the
columns that write stored, so what it keeps equals a fresh load. It still
reads the run row at every step. `docs/performance.md` has the numbers.

JSON a consumer supplies is checked where it enters: decision arguments
and results, tool content, approval capabilities and presentations, and a
reconciliation's result must be one well-formed value nested no deeper
than 256 levels, valid UTF-8, with no escaped surrogate outside a pair and
no repeated object key, because anything else would be stored or hashed
lossily. The depth bound is far below encoding/json's 10,000: the runtime
stores and hashes a consumer's value inside objects of its own, and a
value that passed at 10,000 failed once wrapped, unencodable on Go 1.27
and unreadable from the store on Go 1.26. A tool's input schema is held
to the same depth, because it is part of every approval request. What
fails is observed (`invalid_decision` for a decision, whose bytes are
kept verbatim beside it in `Decision.InvalidArgs` or `InvalidResult` with
`Args` or `Result` left empty, so the record of a call that could not be
parsed never looks like one that could; `tool_error` for content that
arrived after the side effect, keeping the bytes), or fails the run as
`internal_error` for a policy decision or a reconciliation's result, and
never panics. Nothing that is stored or hashed is ever replaced by a
stand-in: a value that would not encode fails the write that needed it,
and the run fails as `internal_error` with nothing more executed. The
canonical encoder behind every content and approval hash is written out in
`hash.go` and pinned byte for byte to the hashes already stored.

A row that will not decode, as one an earlier build wrote past the
decoder's limit, does not make its run unreadable: `ListSteps` and
`ListApprovals` return it with `DecodeError` set and the undecodable field
empty, `Cancel` closes its run, a driver asked to continue the run fails
it as `internal_error` without rewriting the row, and such an approval
cannot be decided.

## What the agent sees and cannot do

`StepInput` carries the run, every prior step in order, the approvals, the
tool specs, and a `ModelCaller`. The agent is stateless between steps and
renders its own model context from the recorded steps, so the audit log
and the agent's view are the same data. The `ModelCaller` is the agent's
only handle to a model: it reserves a row per attempt before dispatch,
records usage, latency, and cost, enforces call, token, and cost limits
before every request, and retries transient failures. A retry waits at
least as long as the provider's `RetryAfter` asks, up to
`ModelConfig.MaxRetryAfter`, and fails as `model_unavailable` rather than
sleep past the context's deadline or the run's time limits. Concurrent
requests within a step reserve their projection before dispatch, and a
request with no output cap is refused under a token or cost limit because
it cannot be projected. A `ServedError` from an adapter is charged at the usage the
provider reported and not retried. Usage that cannot be charged, a
negative count or more cached input than input as reported, or a
`ServedError` wrapping `ErrUnusableUsage` from an adapter that found a
count negative, too large to hold, or not an integer, is replaced by the
conservative estimate (the request's estimated input and its output cap),
the reply is not used, and the run fails as `model_unavailable` without a
retry. Run totals and costs saturate at the largest value rather than
wrap, so a huge reported count trips the limits instead of slipping
under them. The agent has no handle to the policy
or the store. The agent and the policy each receive copies: a change
either makes to what it was handed reaches neither the driver nor the
other, except a write into a `RawMessage`'s bytes, which only that
consumer's own later views carry.

## Approvals

A `require_approval` outcome stores the kind, capability, presentation,
and the exact request, with a hash over all four. `Approve` recomputes the
hash before accepting the decision, inside the transaction that records
it; an approval whose fields no longer hash, as one an earlier build
stored with the hash of an encoding error, is refused with
`ErrApprovalHash`. `ApproveShown` and `RejectShown` also compare the hash
the operator was shown in that transaction, and the update is conditional
on it, so a writer cannot change the row between the check and the
decision.

Only a step's latest approval, in insertion order, can be a grant. A step
paused again, by a policy that now wants something else or by a
reconciliation, has a newer approval, and until that one is approved
`Resume` returns `ErrRunState` and changes nothing: an older grant is
spent. `Resume` checks the recorded arguments against the tool's schema
as registered now, as the loop checks a fresh decision, and ends the step
as `invalid_decision` if they fail; then it re-evaluates policy while the
run is still WAITING and executes the recorded request only when policy
allows it or asks for exactly the approval that was granted; a different
answer pauses again. The move to RUNNING, the resume-time policy decision,
and the tool start commit together. The loop and Resume share one
dispatch over policy outcomes, and an outcome it does not know fails the
run.
Approvals may carry an expiry that cancels the run when next touched.
With `Limits.ApprovalTTL` set, a pending approval expires that long after
it was requested; it bounds only the wait for a decision, never a grant.
With `Limits.GrantTTL` set, a grant expires that long after it was
decided if `Resume` has not acted on it by then: `Resume` finds it
expired and cancels the run as `approval_expired`, as it does a stale
pending one, and nothing executes. `GrantTTL` is zero by default, which
is no expiry, and a run stored before it existed reads it as zero; a
consumer that refuses a stale grant in its own tool, as casework does,
leaves it unset.
`Approve`, `Reject`, and `Cancel` exist as package functions over a
`Store` as well as driver methods: the package functions are the operator
path, used by `cmd/agentrt`, because an operator holds the database and
not the consumer's agent, tools, or policy. The driver methods judge expiry
and stamp decisions with the driver's clock, the package functions with
the wall clock.
`Cancel` ends any non-terminal run as `operator_cancelled`; it exists
because an approval, once granted, cannot be rejected, so a run approved
but never resumed would otherwise have no way to be closed.

## Interruption

A running run is leased to the call executing it: an owner id and an
expiry on the run's row, taken in the transaction that moves the run to
RUNNING, released in the one that pauses or finishes it, and renewed every
third of `Config.LeaseTTL` by a heartbeat that does not wait for the loop,
so a long tool or model call keeps it. The owner is `Config.LeaseOwner`
followed by a random suffix drawn per `Driver`, so no two drivers, in one
process or two, ever share one; a restarted process waits out the leases
its previous life held like any other. One driver also refuses a second
`Start` or `Resume` of a run it is executing with `ErrRunLeased`. Lease
times come from the wall clock, never `Config.Now`. On a file-backed
store the heartbeat renews on a connection of its own, so a consumer
holding `DB()` cannot delay it; a `:memory:` store has one connection,
which is its whole database, and no other process can take its runs.

Every loop write to a RUNNING run requires the lease, and the writes that
start a side effect, the tool start and a model request's dispatch, also
require it unexpired by the wall clock: a loop stalled past its expiry
starts nothing, even if nobody has taken the run. A loop that loses its
lease, because another owner took it or it expired unrenewed, stops before
its next tool call or model request and returns `ErrLeaseLost`; a tool
already running cannot be recalled, and its outcome is recorded only if
its loop still holds the lease, otherwise the step is left for the next
owner. Resume of a RUNNING run whose lease is live and another owner's
returns `ErrRunLeased` and changes nothing. An operator's Cancel needs no
lease; a tool running when the run is cancelled still has its outcome
recorded, as a `step.tool_finished` event marked `late` and carrying the
observation, appended after `run.finished`, while the run stays cancelled
and the step keeps what Cancel wrote. ADR 5 has the reasoning.

A run found RUNNING with its lease expired, or with none, is taken over
by Resume, which records `lease.taken_over` when an owner had held it,
this driver included, and marks the step in flight interrupted with an
observation. What follows depends on the consumer:

- With `Config.Reconcile`, the consumer decides, against its own journals
  and the outside world: continue the loop, complete the run with a
  recovered result, wait for approval, or fail as a conflict. Waiting puts
  the tool call that was executing back through the ordinary pause on the
  `require_approval` decision the reconciliation supplies, provided its
  arguments were JSON and pass the tool's schema now; a step interrupted
  while deciding, or one that fails those checks, cannot wait, and the run
  fails as a conflict rather than wait on nothing.
- Without it, a step interrupted while executing a tool that is not
  `ReadOnly` does not let the run continue, because the agent would ask
  for the same thing again in a new step. The run pauses on that request
  through the ordinary pause, as an approval of kind
  `interrupted_side_effect` whose presentation says the tool started and
  its outcome is unknown. Approving and resuming runs it again, once, on
  the same step, through the same policy re-evaluation as any grant;
  rejecting ends the run. A step whose tool is no longer registered, or
  whose arguments no longer pass the tool's schema, cannot wait, and the
  run fails as `reconcile_conflict`. A step interrupted while executing a
  `ReadOnly` tool, or while deciding, continues as before. ADR 6 has the
  reasoning.

The runtime never executes a step twice of its own accord, nor lets the
agent ask for an interrupted side effect anew: an interrupted side effect
is run again only when the consumer's reconciliation says so or an
operator approves it, and an approved re-run is the same step executing
again, under an approval that says the first attempt's outcome is
unknown. A resume that marked the step interrupted and stopped before
pausing it leaves it for the next resume to pause.

## Time

Two clocks are read. `Config.Now` stamps what is recorded and judges
approval expiry and the time limits; the wall clock stamps leases and
nothing else. A process suspended, or a step that takes wall-clock time,
affects them differently:

- A lease is renewed only while its process runs. A process suspended for
  longer than `LeaseTTL`, or whose heartbeat is starved, finds its lease
  expired when it wakes: it starts no further tool or model call, and
  another process may already have taken the run over. The tool it was
  running when suspended cannot be recalled.
- Approval expiry is judged at the next touch, by the clock of whoever
  touches it: the driver's `Now` for `Driver.Approve` and `Resume`, the
  wall clock for the package functions. A grant's `GrantTTL` is judged
  only by `Resume`, with the driver's `Now`, against the grant's
  `DecidedAt`. A suspension neither extends nor
  shortens an expiry; a deterministic `Now` that does not advance never
  expires anything.
- The active time limit counts step durations by `Now`, so a step during
  which the process was suspended counts in full.

Leases compare wall clocks across processes. Processes sharing a database
file must run on one host, or on hosts whose clocks agree to well within
`LeaseTTL`; clock skew beyond that is unsupported, and so is SQLite on a
network filesystem, whose locking and `fsync` SQLite cannot rely on.

Four reads exist for tools that sit beside a consumer rather than inside
it. `Store.Lease` returns a run's lease owner and expiry; an empty owner
means nobody holds it. `Store.ListEventsAfter` reads events after a
sequence number, for one run or all of them, in commit order and capped
at `MaxPageText`; `export` follows the table through it and never
writes. `Store.ApprovalReason` returns the reason of the policy decision
that paused the run on an approval, read from its `approval.requested`
event, so it is the pausing decision even after a resume-time policy
decided otherwise. `CompileTool` checks a tool spec as `NewDriver` does
and compiles its schema once; `ToolSchema.Check` and `CheckArgs` are the
runtime's acceptance of a tool call's arguments, with the text an invalid
decision records, so a consumer's tests can validate without a driver.
`Operator` is the driverless decision path on a chosen clock, and a
decision on an expired approval returns `ErrApprovalExpired`.

## Persistence

One SQLite file in WAL mode with runs, steps, model_calls, approvals,
events, and a schema_migrations table. Migrations are forward only and a
released one is never edited; the fifth, the run lease, is the first since
v0.1, and a database written by v0.2.1 migrates with its runs intact. A
consumer may keep its own tables in the same file under its own
migrations. Every transaction begins IMMEDIATE and waits on the busy
timeout, migrations are applied in one such transaction so concurrent
first opens are safe, and a database a newer build migrated is refused.
Lists follow insertion order: stored timestamps drop trailing zeros and do
not sort as text. `OpenExisting` opens a database without creating or
migrating it, read-only if asked, and opens the file named: a symbolic
link is refused with `ErrSymlink`, and anything else that is not a
regular file is refused too. `OpenStore` follows a symbolic link, as the
consumer's own configuration names its database.

Every process and operator binary of a version before the lease must be
stopped before this version first opens a database: an older version
ignores leases, so it would resume a run a current process is executing,
and nothing in the file can stop it. When the lease migration is applied,
each RUNNING run is given a lease one `DefaultLeaseTTL` long under a
placeholder owner, so a loop of the older version still inside a call has
that long before a current process may take the run over.

A store opened for writing syncs every commit (`synchronous=FULL`): in
WAL mode `NORMAL` can lose the last commits on power loss, and the last
commit may be the `step.tool_started` of a tool that ran.

The database is the operator's, and the file is kept that way. A new
database is created readable and writable by its owner only, and SQLite
gives its WAL files the same mode. Both openers refuse a database, or a
WAL file beside it, that group or others can write, and a WAL file owned
by another user than the database, with `ErrInsecureMode`: an approval's
hash has no secret in it, so whoever can write the file can forge an
approval. A database others can only read, as earlier releases created
them, is made owner-only by a writer that owns it; a read-only open
changes nothing. Where file modes do not mean owner, group, and others,
as on Windows, these rules are skipped.

A database may come from someone else, as one an operator is asked to
inspect. Every connection to a database file, the heartbeat's included,
runs with `trusted_schema` off and SQLite's defensive mode on, and both
openers refuse a file holding a trigger or a view with
`ErrUnsafeSchema`: the runtime creates neither, and either would run the
file author's SQL inside the runtime's statements. A front end reading
such a file uses the page reads, `ListRunsPage`, `GetRunCapped`,
`ListStepsPage`, `ListApprovalsPage`, `ListEventsPage`, and
`PendingApprovalIDsOf`: each reads at most the page asked for, caps every
text column at `MaxPageText` characters, and takes its total from a
`COUNT`, so neither many rows nor one huge row can exhaust its memory.
`cmd/agentrt`'s `runs`, `show`, and `events` read only through them, by
way of `view.RunsPage` and `view.DetailPage`; `approve`, `reject`, and
`cancel` read the one run and approval they decide whole, because a
decision is bound to the approval's hash.

Within those rules the database is trusted as the operator's filesystem
is; hashes catch corruption and code paths that mutate a record after it
was fixed, not a local attacker who can write the file.

## Modules

The core module `github.com/joeylking/agent-runtime` holds the loop, the
store, policy, limits, and the helpers that need nothing beyond the
standard library. Anything that adds a dependency is a nested module in
this repository with its own `go.mod`, tagged with a directory prefix, so a
consumer imports only what it uses and a provider SDK never reaches the
core.

- the core module: the driver, the SQLite store, policy and limits,
  `providers`, `render`, `trace`, `view`, `replay`, `scripted`,
  `cmd/agentrt`, `examples/scripted`, and the packages below that need
  nothing beyond the core's own dependencies:
  - `export`: a read-only follower of the events table that hands each event,
    in commit order and from a cursor, to a sink, with a JSON Lines sink.
  - `testkit`: what a consumer's tests use without a model: a crash and
    resume harness, a policy conformance table, schema fuzzing of tool
    arguments, and a render equivalence check.
  - `approver`: the interface an operator surface decides approvals
    through, bound to the hash it showed, and its store-backed
    implementation.
  - `approver/webhook`: the reference signed HTTP approval channel over
    `approver`.
  - `examples/approver`: a scripted run decided through the webhook as a
    chat bot would, then resumed.
- `providers/ollama`: a local Ollama server over its native chat API.
- `providers/anthropic`: the Claude Messages API through the official SDK,
  with the raw assistant turn that keeps signed thinking blocks alive.
- `providers/openai`: any OpenAI-compatible chat completions endpoint.
- `mcp`: an MCP server's tools as runtime tools, pinned by hash and
  classified by an operator.
- `bench`: the outcome vocabulary, result file, summarizer, and comparison
  for scoring an agent that acts, with no dependencies at all.
- `export/otel`: an exporter that turns the events `export` delivers into
  OpenTelemetry spans, and the provider an operator's `OTEL_*` environment
  configures.
- `examples/live`: one live model run against in-memory tools that stops
  for an operator before the first change.
- `examples/mcp`: the same controls over the MCP filesystem server,
  pinned to an exact version and integrity hash in `package-lock.json`,
  installed with `npm ci --ignore-scripts` and run by `node` from that
  install, where reads are allowed and a write waits for an approval.

## What is deliberately absent

Multi-agent orchestration, a planning DSL, prompt templates, memory or
retrieval, a multi-provider abstraction, a service mode, and a UI. Each
would be added when a consumer demonstrates the need, not before.

The MCP adapter covers tools only: resources, prompts, sampling,
elicitation, and the server side of MCP are out of scope.
