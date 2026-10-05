# The MCP proxy

`agentrt-proxy` puts the runtime's gate in front of MCP servers for a program
that is not written in Go. An MCP host, the application that runs a model and
gives it tools, starts the proxy as one of its MCP servers. The proxy connects
as a client to the real servers through the [`mcp`](../mcp) module, which
pins their tools and takes each tool's side-effect class from the operator,
and it offers the host only those tools. Every `tools/call` the host makes is
handed to a `Gate`, which records it, checks its arguments against the
tool's schema, evaluates the policy, pauses on an approval bound by hash to
the exact request when the policy asks, executes the upstream call itself,
and records the outcome. The proxy talks to the host over stdin and stdout
and opens no network listener.

It is a small, deliberately scoped experiment. The Go library remains the
full form of the runtime: the proxy has no model, no loop, and no view of a
conversation, so several of the runtime's controls cannot apply.
[ADR 8](decisions/0008-mcp-proxy.md) records why it is built the way it is.

## What it is and is not

The proxy governs only calls that go through it. If the host gives the model
a shell, file access, or other tools of its own, the model can bypass the
proxy, and it can approve its own request with the operator command or by
editing the database. So the proxy is for hosts without such tools, or with
them disabled or behind the host's own prompts, or inside a sandbox whose
only way out is the proxy.

It is for one person who is both the host's user and the operator. It does
not separate them.

The upstream servers run as that same user. No server may be able to reach
the proxy's configuration, its manifests, or its database: a server that
can write them can rewrite the policy or forge an approval. Give a
filesystem server a root that does not contain them. The proxy refuses to
start a stdio server whose arguments name one of those files or a directory
holding one, as a best-effort check (see Configuration). It is a guard
against a mistake in the configuration, not a boundary: it cannot see what
a server reaches by other means, and it protects neither the proxy's binary
nor the host's own configuration, either of which a server that can write
them can replace.

It never sees model calls, so it sets no token or cost limits, and it never
sees the user's prompt.

Each call is a separate run, so there are no limits or policy over a
conversation's history.

The duplicate rule refuses a deliberate identical repeat of a mutating call
inside the window, not only a retry.

Results are capped and flattened.

The pin covers what a server presents, not what it does.

What it keeps:

- an approval bound by hash to the exact request, durable across restarts,
  with policy evaluated again when the request is collected;
- a mutating call whose outcome is unknown, cut off by a crash, timed out,
  or left without an answer from the server, is not run again, as the
  identical request, without an operator's approval, and other mutating
  calls to its tool are refused until an operator resolves it (see
  Limitations for what "identical" leaves out);
- the audit record is committed with each state change;
- tools are pinned and classified by the operator;
- an identical mutating call is not executed twice within the window.

## One run per call

MCP gives a server no notion of a conversation. A host may start one proxy
process per session, one per call, or several at once, and it passes no
session identifier. So each tool call is its own run, with the id
`<session>.<suffix>`, where `session` is the name in the configuration and
the suffix is 16 random hex characters, and the goal `call <tool>`.

A run holds the call's one tool step. The proxy registers every upstream tool
with the gate as a terminal tool, so a call that succeeds completes its run in
the transaction that records the result. A call that the policy denied, whose
arguments were invalid, or whose tool returned an error leaves its run
running after that step; the proxy then ends it with a `fail` decision whose
origin is `operator`, because the proxy made that decision on the operator's
configuration and the model did not. The tool call itself is recorded with
origin `model`. A mutating call whose outcome is unknown is not ended: the
proxy proposes it again in the same run, with origin `operator`, and that
step waits for an operator (see Timeouts and lost connections). Every run's
limits are fixed: three steps, three consecutive failures, a loop threshold
of three, and the configured approval and grant expiries. A call's run needs
at most its tool step, one re-run, and the closing decision. An approved
re-run whose outcome is unknown again is asked about again in the same run,
once; when the next re-run's outcome is unknown too, the step limit ends the
run, and the proxy asks the same question in a fresh run of the request at
once, so an approval is left waiting
(`TestUnknown_StepLimitKeepsTheToolBlocked`).

For `agentrt runs` this means one row per call that started something new: a
call that collects an approved request, or is refused by one of the rules
below, adds no row. A run ends `COMPLETED`, `FAILED`, or `CANCELLED`, or
waits in `WAITING_FOR_APPROVAL`. It stays `RUNNING` only while a call
executes it, or when the process running it stopped: then the next identical
call ends it, or, if it was cut off while executing a mutating call, pauses
it for an operator, and so does the next mutating call to the same tool
(`TestIndex_ALateBeginLeavesNoRunBehind`,
`TestProxy_AbandonedCallIsEndedAndRunFresh`). The events a run carries
(`TestProxy_RunsRecordWhatHappened`,
`TestProcess_CrashThenApproveRunsItAgainOnce`,
`TestUnknown_TimeoutAfterTheEffectWaitsForAnOperator`):

| Outcome | Events |
|---|---|
| allowed | `run.created`, `run.started`, `step.started`, `step.decided`, `step.policy`, `step.tool_started`, `step.tool_finished`, `run.finished` |
| denied or invalid | `run.created`, `run.started`, `step.started`, `step.decided`, `step.policy` (denied only), `step.failed`, `step.started`, `step.decided`, `run.finished` |
| pending, then collected | `run.created`, `run.started`, `step.started`, `step.decided`, `step.policy`, `approval.requested`, `approval.decided`, `run.resumed`, `step.policy`, `step.tool_started`, `step.tool_finished`, `run.finished` |
| interrupted, approved, run again | `run.created`, `run.started`, `step.started`, `step.decided`, `step.policy`, `step.tool_started`, `lease.taken_over`, `step.interrupted`, `step.policy`, `approval.requested`, `approval.decided`, `run.resumed`, `step.policy`, `step.tool_started`, `step.tool_finished`, `run.finished` |
| timed out, then waits | `run.created`, `run.started`, `step.started`, `step.decided`, `step.policy`, `step.tool_started`, `step.tool_finished`, `step.failed`, `step.started`, `step.decided`, `step.policy`, `approval.requested` |

Because there is no run that spans calls, several requests can wait for
approval at once, a rejection ends only the request it rejected, calls with
different arguments run in parallel, and two proxy processes on one session
name work together. The rules that span calls are the proxy's own, kept in an
index of its calls in the same database: a table of its own, `proxy_calls`,
under migrations of its own in `proxy_schema_migrations`, as
[architecture](architecture.md#persistence) allows a consumer. The runtime's
tables are not written, so `agentrt` reads the file as before.

## The rules across calls

The proxy keys each call by the SHA-256 of the registered tool name and
`agentrt.CanonicalJSON` of the arguments, the form the runtime hashes an
approval over. Object key order, whitespace, and how a string's characters
are escaped do not matter. Every string's contents and every number's
literal do, so `1250.5` and `1250.50`, or `10` and `1e1`, are different
requests, as they are different approvals. Arguments the runtime refuses as
JSON where it enters, a repeated key, invalid UTF-8, an unpaired surrogate
escape, nesting past 256 levels, or a number past its bound, have no key: no
rule applies to them, they never match another request, and the gate
records them as `INVALID` (`TestRequestKey_NoCollisions`,
`TestProxy_ArgumentsTheRuntimeRefusesNeverCollect`). What executes is
always the recorded request.

Before starting anything, the proxy looks at the runs of this session with
the same key and applies, in order:

1. **A run waiting on an approval for this request**: the call collects it.
   While the approval is pending the call is held, then answers
   `PENDING_APPROVAL`. Once it is approved, the gate attaches to the run,
   evaluates the policy again and judges the grant's age, and executes the
   recorded request, never this call's bytes. If the policy now asks for a
   different approval, the call answers `PENDING_APPROVAL` for that one.
2. **A run executing under a live lease**: another call, in this process or
   another, is executing it, or a process that crashed has not yet lost its
   lease. Nothing runs, and the call answers `IN_PROGRESS`.
3. **A run left running by a call that is gone, its lease expired**: the gate
   takes it over. A tool that is not read-only, cut off while executing,
   pauses on an approval of kind `interrupted_side_effect` and the call
   answers `INTERRUPTED`. A read cut off, a call cut off before its tool ran,
   or one cut off after its outcome was recorded leaves nothing to execute:
   the proxy ends that run and the rules continue with it finished. A
   mutating call cut off before its tool ran, abandoned after the policy
   allowed it or its lease lost before it executed, made no attempt: the
   identical call runs under the ordinary rules and the tool is not blocked
   (`TestProxy_AbandonedCallIsEndedAndRunFresh`,
   `TestUnknown_CutOffBeforeExecutingIsNoAttempt`).
4. **A request rejected, cancelled, or expired within the repeat window**:
   the call answers `REJECTED`, with the operator's note, or `EXPIRED`, and no
   new approval is asked for within the window. This holds for the identical
   request only: a model that changes one character of the arguments makes
   a new request, which asks again, and `max_pending` bounds how many such
   requests wait at once, not how often a model asks. An expired approval
   that asked whether an attempt with an unknown outcome runs again refuses
   nothing: an expiry resolves nothing, and rule 6 asks again.
5. **The duplicate rule**: for a tool that is not read-only, a run with this
   key whose tool call executed with a known outcome, a result or an error
   the server returned, within `repeat_window` of now, refuses the call as
   `DUPLICATE`, with the earlier call's time and outcome. A call whose server
   answered with content the runtime refused executed, and counts. A denied
   or invalid earlier attempt does not count, because nothing ran, and an
   attempt whose outcome is unknown is not a duplicate: rule 6 decides it.
   Rules 4 and 5
   read the request's runs newest first, and the newest that decides
   anything decides.
6. **An unknown outcome**: for a tool that is not read-only, when the last
   attempt at this request, at any age, has an outcome that is unknown, cut
   off by a crash, timed out, or failed with no answer from the server, and
   no attempt has executed with a known outcome since, the new run asks an
   operator with an `interrupted_side_effect` approval naming that attempt,
   whatever the policy says short of `deny`, and the call answers
   `UNKNOWN_OUTCOME` or `INTERRUPTED`. An operator's rejection does not
   resolve it: the identical call asks again, never runs on `allow` alone
   (`TestUnknown_RejectionUnblocksAndAnIdenticalCallAsksAgain`).
7. Otherwise a new run begins and the call is proposed.

Two more rules apply before a new run begins. While another request to the
same tool has an unknown outcome that no operator has resolved, every new
call to that tool that is not read-only is refused as `BLOCKED`, naming the
approval that asks about it, so a model cannot get around an unknown outcome
by changing an argument. A run of that tool left running past its lease is
taken over first. An unknown outcome is resolved, and the tool unblocked:

- when an operator approves the re-run and the identical call collects it
  with an outcome that is known, a result or an error the server returned,
  or an answer whose result could not be recorded; a collected re-run whose
  outcome is unknown again is a new unknown outcome, asked about at once;
- when an operator rejects the re-run, or cancels its run; the identical
  call is still not run on the policy alone (rule 6);
- never by an expiry. An approval about it that expires unanswered, a
  re-run that meets the run's step limit, or a run that stopped before the
  proxy asked (a crash after the attempt's outcome was recorded) leaves
  nothing waiting, so the next mutating call to that tool, or the identical
  call, asks the same question again in a fresh run and is answered with
  that approval's id. The tool is never blocked with nothing for an operator
  to act on (`TestUnknown_StepLimitKeepsTheToolBlocked`,
  `TestUnknown_CrashBeforeTheReRunKeepsTheToolBlocked`,
  `TestUnknown_ExpiryAsksAgainRatherThanUnblocks`).

An operator sees the approval in `agentrt runs` as a run
`WAITING_FOR_APPROVAL`, in `agentrt show <run>`, and on the proxy's stderr.
A question the proxy asks again while deciding a call to another request
of the tool is not held to `max_pending`. And a call
that needs an approval while `max_pending` approvals of the session are
already waiting is refused as `BLOCKED` without asking.

The duplicate rule is what makes a host's blind retry after a lost response
safe: the retry does not execute a second payment, and it hands the model the
first one's outcome. It also refuses a repeat the model meant, inside the
window. To repeat a call to a tool deliberately, wait out the window, or turn
the rule off for that tool with `"repeat_window": "0s"` in its rule, or for
every tool with the top-level `repeat_window`. Turning it off also turns off
rule 4 for that tool. Rule 6 does not depend on the window, and turning the
window off does not turn it off.

The window is measured from when the earlier run finished.

## How the rules stay safe across processes

Within one process, a call's reading of the index and the runs, and whatever
it begins or collects, happens under one lock; executing and holding happen
outside it, so calls with different requests run in parallel. Across
processes, the index holds at most one open row per key, enforced by a
unique index, and a new row is claimed in one SQLite transaction, which
begins `IMMEDIATE` and so holds the database's write lock: the open row is
read again inside it and must still be the one the call saw, or the call
reads again. A row is closed only when the next one for its key is claimed,
and only once its run has finished, so a run that has not finished is
always behind its key's open row, where the next identical call finds it.
The row is written before the run is begun, so a crash between the two
leaves a row without a run, which means nothing started; what a call did is
always read from its run and steps, never from the index. A call that finds
the open row without a run begins that run itself, under the row's id, and
the store refuses a second run with one id, so of two calls, or two
processes, that try, exactly one begins it and the other decides again
(`TestIndex_RowWithoutARunIsBegunUnderItsID`, `TestIndex_OneCallBeginsARow`,
`TestIndex_ALateBeginLeavesNoRunBehind`,
`TestProcess_TwoProxiesOnOneSession`). A row whose run has not begun is
settled when a scan first finds it, so it is not read on every call, and
the call that begins its run unsettles it
(`TestIndex_RowWithoutARunIsSettledWhenFound`).

The `max_pending` cap is exact within one process. Two processes that begin
approval-requiring calls at the same moment each check the count before
asking, so together they can go past it by one each.

## The approval round trip

As the model experiences it, with `hold` at its default of 15 seconds:

1. The model calls `fs_write_file`. The policy requires an approval, so the
   call is held, and if the request carried a progress token the host is sent
   a progress notification about once a second.
2. If the operator approves within the hold, the call executes and returns
   the result, in the same call
   (`TestProxy_ApprovedWithinTheHoldCompletesInOneCall`).
3. Otherwise the call returns a normal result, not an error:

   > PENDING_APPROVAL: this request is waiting for an operator's approval
   > (approval 4a49…). Nothing has been executed. Do not call this tool again
   > until you have been told it was approved, do not try another tool to
   > achieve the same thing, and do not ask the user to approve it: the
   > operator decides elsewhere. Tell the user it is waiting. After approval,
   > call this tool again with exactly the same arguments to run it and get
   > the result.

4. Told it was approved, the model calls `fs_write_file` again with the same
   arguments, and the proxy executes the recorded request once and returns
   the result (`TestProxy_ApprovalRoundTrip`). A third identical call within
   the window is `DUPLICATE` and runs nothing.

The wording follows a measurement with two small local models over 96 runs,
recorded in ADR 8: after a prose pending result they stopped and reported,
did not switch tools or claim success, and once told the request was approved
re-called the same tool with identical arguments every time. A JSON-shaped
pending result made them poll and ask the user to approve.

As the operator experiences it, the proxy writes to its stderr, which a host
keeps in its log:

```
agentrt-proxy: approval d7ce…5f35 (local_mutation) for fs_write_file waits for an operator: run laptop.0b41…e5bd
agentrt-proxy:   decide with: agentrt -db "/home/me/proxy/proxy.db" approve laptop.0b41…e5bd -approval d7ce…5f35   (or reject)
```

`agentrt runs` lists the waiting run and `agentrt show <run>` shows the
request. `agentrt approve` prints the approval, the server, the upstream
tool, the arguments, the operator's fixed values if the tool has any, and the policy's reason,
and asks; `reject` takes a `-note`, which the model is shown. The decision is
bound to the hash of what was printed, as for any run. The
[webhook approver](../approver/webhook) works on the same database too.
Approval is decided only outside the host: the proxy offers no tool that
approves, and it uses neither MCP elicitation nor sampling toward the host.
Nothing the model sees names the operator command, the database, the
configuration, or a run id (`TestProxy_ModelVisibleTextNamesNoOperatorPath`).

An approval's capability, which its hash binds, is the server, the upstream
tool, the registered name, the side-effect class, the arguments, and the
operator's fixed values for that tool, which the request does not carry. A
fixed value changed in the configuration between the approval and the
collection makes the policy ask for a new approval instead of executing under
a value nobody approved (`TestProxy_ChangedFixedValueAsksAgain`). The
presentation adds the question and the policy's reason.

The approval's expiry, `approval_ttl`, and the grant's, `grant_ttl`, are the
runtime's `Limits.ApprovalTTL` and `Limits.GrantTTL`, 24 hours each by
default. A pending approval past its expiry is expired when its request is
next called, and a grant past its expiry is expired when it is collected;
either way nothing executes and the call answers `EXPIRED`
(`TestProxy_ExpiredIsNotAskedAgain`, `TestProxy_StaleGrantExecutesNothing`).

## The status tool

The proxy serves one tool of its own, `gate_status`, with an optional
`approval_id` argument. It answers from the database. Without an id, it
lists the session's pending approvals, each with its id, tool, a summary of
its arguments, and how long it has waited. With an id, from the last hour,
it answers for that one: pending; approved and awaiting collection, with the
instruction to call the original tool again with exactly the same
arguments; approved and executed, only when its step executed, with what
its step recorded: it succeeded, the server reported a failure (with the
error), the server answered and its result could not be recorded, or its
outcome is unknown (with the approval that now asks whether it runs again,
if one waits); approved but not executed, with why, such as a policy that
denied it at collection or an operator who cancelled it; rejected, with the
note; or expired. It never executes, decides, or writes anything, and it is
not recorded as a run (`TestProxy_StatusToolReportsAndNeverExecutes`,
`TestProxy_StatusSaysWhetherAnApprovedRequestExecuted`,
`TestUnknown_StatusSaysWhatCameOfAnApprovedRequest`). The measurement found that a
model merely asked for an update looks for a tool like this and passes the
approval id.

Its name is `gate_status` unless `status_tool` names another. The proxy
refuses to start if an upstream tool registers under the same name
(`TestProxy_StatusToolNameClashRefused`).

## What the model sees

A result is what the gate recorded, through the `mcp` module's mapping: the
server's structured content when it sent some and it fits, else its text
blocks joined, with a block that is not text replaced by a placeholder, the
whole capped at 64 KiB. Structured content that is an object is passed back
as structured content too, and as its JSON in a text block. A result is
data and is passed back as the server sent it, unescaped. No tool is listed
with an output schema. An upstream tool's error is an `isError` result
carrying the error text the gate recorded, escaped, not a protocol error
(`TestProxy_UpstreamErrorAndStructuredResult`).

Every other answer is prose that starts with a fixed prefix, so a host or a
test can recognise it:

| Prefix | isError | Meaning |
|---|---|---|
| `PENDING_APPROVAL` | no | waiting for an operator; nothing executed |
| `IN_PROGRESS` | no | the same request is executing, or its outcome is not yet known; retry shortly |
| `INTERRUPTED` | no | an earlier attempt was cut off and its outcome is unknown; an operator decides whether it runs again |
| `UNKNOWN_OUTCOME` | no | this call, or an earlier attempt of it, timed out or failed with no answer from the server, so it may have taken effect; an operator decides whether it runs again |
| `UNRECORDED` | no | the server answered this mutating call, so it executed, but its answer could not be recorded; it may have taken effect, and it is not to be retried |
| `DENIED` | yes | the policy does not allow it, with the policy's reason |
| `INVALID` | yes | the arguments do not match the schema, with the runtime's reason |
| `REJECTED` | yes | an operator rejected or cancelled this request, with the note |
| `EXPIRED` | yes | the approval expired before it was used |
| `DUPLICATE` | yes | the identical call already executed within the window, with its time and outcome |
| `BLOCKED` | yes | another request to this tool has an unknown outcome that an operator has not resolved, or too many approvals are waiting |
| `UNAVAILABLE` | yes | the proxy could not read or write its records; says whether anything was executed |
| `GATE_STATUS` | no | the status tool's answer |

Text a model, a server, or an operator chose is escaped with
`trace.Sanitize`, which also turns a newline into a space, and bounded
before it is put in these answers and in an error result: a denial's
reason, a schema error, an operator's note, a duplicate's quoted result or
error, an unknown outcome's error, and the status tool's summaries
(`TestProxy_ServerTextInProseIsEscaped`). A result returned as the call's
result is not prose and is passed back as recorded. The runtime's schema
errors quote the address it compiles a tool's schema under; the proxy cuts
that address out.

## Calls, cancellation, and shutdown

The host's `tools/call` requests are handled concurrently. When the host
cancels a call, or times out, while its upstream call is running, the
upstream call is not cancelled: it finishes within the tool's timeout, its
outcome is recorded, and an identical call afterwards gets it as `DUPLICATE`
(`TestProxy_HostCancelLetsTheCallFinish`).

## Timeouts and lost connections

A call to a tool that is not read-only that times out, or fails without an
answer from the server, a dropped connection or a protocol error, may have
taken effect: the server may have acted before the proxy lost it. The core
records it as a `tool_error`, with failure `timeout` or `error`. The proxy
treats every such error as an unknown outcome, except the ones it knows are
the server's answer or a refusal before anything was sent: an `isError`
result, which the `mcp` module records as the server, the tool, the
content's size, and the server's text; content the runtime refused; and the
`mcp` module's own refusals of a denied or fixed parameter
(`TestUnknown_ClassifiesRecordedErrors`). A read's error is always an
ordinary failure.

Content the runtime refused, not usable JSON, nested past 256 levels, or a
number past its bound, is an answer: the server answered, so for a tool that
is not read-only the call executed and only its result could not be
recorded. The proxy does not report that as a failure, which would invite a
retry, nor as an unknown outcome, which would ask an operator to run again
a call the server is known to have answered: the call answers `UNRECORDED`,
saying it executed and its answer could not be recorded, and it counts as
executed, so the identical call within the window is `DUPLICATE`
(`TestUnknown_UnrecordedResultCountsAsExecuted`). A read whose content was
refused is an ordinary failure.

For an unknown outcome the run is not ended. In the same run the proxy
proposes the recorded request again, with origin `operator`, and the policy
asks an operator with an `interrupted_side_effect` approval whose capability
names the earlier attempt (its run, step, start time, and how it ended), and
whose presentation says that attempt may have taken effect. The call
answers `UNKNOWN_OUTCOME`: the outcome is unknown, it may have taken effect,
and an operator decides. The operator sees it at once on stderr and in
`agentrt runs`. While it waits, the identical call answers the same and runs
nothing, inside the window or long after it, and other mutating calls to the
tool are `BLOCKED`. Approved, the identical call runs it once more; if that
outcome is unknown too, the question is asked again (see One run per call).
Rejected, nothing runs, the tool is unblocked, the identical call is
`REJECTED` within the window, and after it asks an operator again (rule 6).
Expired, nothing runs and the tool stays blocked: the next mutating call to
it asks again
(`TestUnknown_TimeoutAfterTheEffectWaitsForAnOperator`,
`TestUnknown_RejectionUnblocksAndAnIdenticalCallAsksAgain`). An error the
server returned is a known failure: no operator is asked, the identical call
within the window is `DUPLICATE` with the error, and after the window it runs
again (`TestUnknown_ServerErrorIsAKnownFailure`). Only the hold for an approval ends
with the host's request. When stdin closes, or on SIGINT or SIGTERM, the proxy
accepts no more calls, lets the calls in flight finish and be recorded, closes
the upstream connections, and exits
(`TestProcess_StdinCloseMidCallFinishesTheStep`).

## Restart and crash

An approval is a row in the database. A proxy started after the one that
asked for it collects it like any other
(`TestProcess_RestartCollectsAnApprovalGrantedWhileItWasGone`).

A proxy killed while an upstream call runs leaves that call's run running
under a lease that is no longer renewed. Until the lease expires, `lease_ttl`
after its last renewal and 30 seconds by default, an identical call answers
`IN_PROGRESS`. After it expires, the next identical call takes the run over:
the step is marked interrupted, and for a tool that is not read-only the run
waits on an `interrupted_side_effect` approval and the call answers
`INTERRUPTED`. While it waits, every other mutating call to that tool is
`BLOCKED`, and other tools work. A proxy stopped after an attempt's unknown
outcome was recorded and before it asked about it leaves a run with nothing
waiting; the next mutating call to that tool takes it over, asks the
question in a fresh run, and is `BLOCKED` on it
(`TestUnknown_CrashBeforeTheReRunKeepsTheToolBlocked`). If the operator approves, the next identical
call runs the request again, once: this is the documented approved re-run,
under an approval whose presentation says the first attempt may have taken
effect, and whose capability names the earlier attempt's step and start time.
If the operator rejects, nothing runs, the identical call is `REJECTED`
within the window and asks an operator again after it, and the tool is no
longer blocked
(`TestProcess_CrashThenApproveRunsItAgainOnce`,
`TestProcess_CrashThenRejectRunsNothingAndUnblocks`,
`TestProcess_CrashThenRejectAsksAgainAfterTheWindow`,
`TestProcess_CrashOfAnApprovedPaymentAsksAgain`).

## Configuration

One JSON file, given with `-config`. It is read only when the current user
owns it and its group and others cannot write it, and so are the manifests.
The database is checked the same way before the store opens it, when it
exists, and again after, and a database path that is a symbolic link is
refused, as `agentrt` refuses one; the store then applies its own rules. The
directory holding these files is not checked, by the proxy or by the
runtime: keep it where only you can write. Unknown fields are refused,
anywhere in the file, and so is a repeated key, in the file and in a
manifest, which would otherwise read as its last value. Relative paths for
the database and the manifests are relative to the file's own directory.

| Field | Default | Meaning |
|---|---|---|
| `database` | required | the SQLite file of the runs, approvals, and the proxy's index |
| `session` | required | 1 to 64 letters, digits, `_`, or `-`; the first part of every run id |
| `servers` | required | the upstream servers, below |
| `policy` | `agentrt.DefaultPolicy` | side-effect class to `allow`, `require_approval`, or `deny`; a class left out keeps the default (read-only and local mutation allowed, remote mutation approval, destructive denied) |
| `hold` | `15s` | how long a call is held for its approval; `0s` answers at once |
| `repeat_window` | `10m` | the window of rules 4 and 5; `0s` turns both off |
| `max_pending` | `5` | approvals the session may have waiting |
| `approval_ttl` | `24h` | how long an approval may stay pending; `0s` is no expiry |
| `grant_ttl` | `24h` | how long a grant may wait to be collected; `0s` is no expiry |
| `status_tool` | `gate_status` | the status tool's name |
| `lease_ttl` | `30s` | the lease a call holds on its run while it executes; how long a crashed call reads as in progress; at least `3s`, since it is renewed at a third of it |

Each server takes the `mcp.Server` fields and its pin and rules:

| Field | Meaning |
|---|---|
| `name` | required; tools are offered as `<name>_<tool>` unless a rule renames them |
| `command` | a stdio server's argv; or |
| `endpoint` | a streamable HTTP server's URL |
| `env`, `inherit_env` | the only environment a stdio server gets beyond `PATH`, `HOME`, and `TMPDIR` |
| `connect_timeout` | bounds connecting, the handshake, and the listing; 30s by default |
| `default_timeout` | a call's bound when its rule names none |
| `manifest` | the file `pin` writes and serving checks the server against |
| `rules` | the operator's rules, by the server's own tool names; a tool with none is not offered |
| `skip_path_check` | `true` turns off the check below for this server |

A stdio server whose command arguments name the configuration file, a
manifest, or the database, or a directory holding one, is refused at
startup. Each argument is read for the paths it may name: the argument
itself, the part after its first `=` or `:`, a `file://` URL's path, and the
rest of a short flag such as `-r/path`, with a leading `~` read as the home
directory; a relative path is relative to the proxy's working directory.
Each one that exists is compared by identity with the protected files and
every directory above them, so a symbolic link or a different case on a
case-insensitive filesystem names the same directory
(`TestConfig_RefusesAServerThatReachesTheProxysFiles`,
`TestConfig_ReachCheckReadsCommonSpellings`). It is a best-effort guard
against a mistake in the configuration, not a boundary: it reads what the
configuration says, not what the server can reach, which can be a path it
reads from its own configuration or is told later, or one spelled in a way
this check does not read; and it protects neither the proxy's binary nor
the host's own configuration. Set `skip_path_check` only for a server that
cannot write there.

A rule is the shape [`examples/mcp/rules.json`](../examples/mcp/rules.json)
uses (`mcp.FileRule`): `side_effect`, `timeout`, `description`, `deny`,
`fixed`, `rename`, and `allow_hint_mismatch`, plus two overrides for that one
tool: `outcome`, which replaces its class's policy outcome, and
`repeat_window`. `terminal` is accepted and changes nothing, since every
call's run ends with its call. There is no condition on arguments and no
rule over history: the Go library is the way to write either, and the proxy
keeps its policy behind one internal interface so that an external
evaluator could be added later.

A complete example against the filesystem server that `examples/mcp` pins:
reads allowed, a write needing an approval. Install the server once as that
example does, `(cd examples/mcp && npm ci --ignore-scripts)`, and write the
file, here `~/proxy/proxy.json`, mode `0600`. The server's root,
`/home/me/sandbox`, holds none of the proxy's files, which are all in
`~/proxy`:

```json
{
  "database": "proxy.db",
  "session": "laptop",
  "policy": {"local_mutation": "require_approval"},
  "servers": [
    {
      "name": "fs",
      "command": ["node", "/path/to/agent-runtime/examples/mcp/node_modules/@modelcontextprotocol/server-filesystem/dist/index.js", "/home/me/sandbox"],
      "connect_timeout": "30s",
      "default_timeout": "5s",
      "manifest": "fs.manifest.json",
      "rules": {
        "read_text_file": {"side_effect": "read_only", "timeout": "5s"},
        "list_directory": {"side_effect": "read_only", "timeout": "5s"},
        "write_file": {"side_effect": "local_mutation", "timeout": "10s", "allow_hint_mismatch": true}
      }
    }
  ]
}
```

The server marks `write_file` destructive and the operator classifies it as a
local mutation, which `allow_hint_mismatch` records. Then:

```sh
agentrt-proxy pin -config ~/proxy/proxy.json     # writes fs.manifest.json, 0600, and prints the server's hints
agentrt-proxy check -config ~/proxy/proxy.json   # what is offered, refused, and unclassified
```

`pin` prints each tool with the annotations the server asserts, which
classify nothing. `check` loads everything as serving would and prints the
report; with the file above it registers `fs_list_directory`,
`fs_read_text_file`, and `fs_write_file` and lists the server's other eleven
tools as unclassified. It exits non-zero when a server registers nothing.

The proxy builds standalone against the versions its `go.mod` pins, with no
workspace; from this repository:

```sh
(cd proxy && go build -o ~/bin/agentrt-proxy ./cmd/agentrt-proxy)
```

## The host

Any MCP host that starts stdio servers takes the same shape: a command and
its arguments. For example:

```json
{
  "mcpServers": {
    "gated": {
      "command": "/home/me/bin/agentrt-proxy",
      "args": ["-config", "/home/me/proxy/proxy.json"]
    }
  }
}
```

The host lists `fs_list_directory`, `fs_read_text_file`, `fs_write_file`, and
`gate_status`. Give it no shell or file tool of its own, or keep those behind
the host's own prompts, for the reasons at the top of this page.

## A demonstration with no model

`proxy/example` scripts a host against a fake payment server bundled with
the proxy, and needs no model, no key, and no Node:

```sh
go run ./proxy/example
```

It shows a read that runs; a payment that waits, is approved from a second
process, and is collected once; a proxy killed while a payment is inside the
server, after which the re-call reports the outcome unknown and waits for the
operator, who rejects it; and a host that lost a response and retries, which
pays once through the proxy and twice straight against the server. It waits
for every process it started before it exits (`TestDemo_RunsEveryStep`).

## Limitations

Besides the scope at the top of this page:

- The proxy reads the upstream servers' tools once, when it starts.
  `tools/list_changed` is ignored, so a server's new or changed tools are seen
  after a restart, and only after `pin` and the rules agree with them.
- The `max_pending` cap can be passed by one per process when several
  processes ask at the same moment.
- `BLOCKED` stops new calls to a tool with an unresolved unknown outcome.
  Any approval for that tool is still collected by its identical call,
  including one an operator granted after the block began.
- An operator who stays away does not unblock a tool: when the approval
  about its unknown outcome expires, the next mutating call to the tool,
  or the identical call, asks the same question again in a fresh run. Until
  an operator approves or rejects, every other mutating call to that tool
  is `BLOCKED`.
- "Not run again without an operator's approval" holds for the identical
  request, keyed as above. After a rejection or an expiry, a request
  spelled differently, `7` for `7.0` or with an optional field added, is a
  different request: it is blocked while the unknown outcome is unresolved,
  and after an operator rejected the re-run it runs under the ordinary
  policy, which may allow it.
- The reach check of a server's arguments is a guard against a mistake,
  not a boundary, and protects neither the proxy's binary nor the host's
  configuration (see Configuration).
- The rules across calls key a request by its registered tool name. Two
  server entries pointing at the same upstream, or a tool renamed between
  restarts, are different tools to them, so `BLOCKED` and the duplicate rule
  do not carry from one name to the other.
- The status tool answers for an approval from the last hour, from at most
  200 calls.
- An approval id is shown to the model, so that it can ask about it; the
  operator command needs the run id too, which the model is never shown.
- An upstream error's text names the server and the upstream tool.
- In one place the proxy reads the runtime's `runs` table directly, by
  primary key, inside its own statement, to settle a row whose run has not
  begun only if it still has not.
- If the database fails under a call, the proxy answers `UNAVAILABLE` and the
  call's run may be left running with no lease; the next identical call ends
  it.
