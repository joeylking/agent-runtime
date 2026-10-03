# An approval is a hash, not a yes

An agent built on agent-runtime asks for actions, and a policy written in
ordinary code decides whether each one runs. One of the policy's answers
is "ask a person first". The run then pauses until an operator, the person
responsible for the agent, approves or rejects the request. This essay is
about what that approval is, because a careless design turns it into a
yes that can be spent on something nobody read.

## Yes to what?

The simplest approval is a flag on the run: approved, true. The trouble
is the time between asking and acting. The operator may decide an hour or
a day later. Meanwhile the request can be re-planned, the stored row can
be edited, the policy can change, or the tool can be upgraded. A flag
says someone agreed. It does not say to what.

agent-runtime stores an approval as a row holding four things
([`approval.go`](../../approval.go)):

- the kind, a name the policy chooses, such as `publication`;
- the capability, what the operator would be granting;
- the presentation, what the operator is shown;
- the exact request: the run and step it belongs to, the tool's full
  registered specification (name, description, input schema, kind of
  effect, timeout), and the arguments.

The row carries a SHA-256 hash over all four, computed from a canonical
JSON encoding with sorted keys. That encoder is written out by hand in
[`hash.go`](../../hash.go) rather than left to Go's `encoding/json`,
because stored hashes must still verify after a Go release changes how
the standard library escapes or formats something. Golden tests pin it
byte for byte to the hashes already stored
([`TestContentHash_Golden`](../../hash_test.go),
[`TestCanonicalJSON_MatchesEncodingJSON`](../../hash_test.go)). A value
that will not encode is an error, never a stand-in hash
([`TestApprovalHash_ErrorsRatherThanSubstituting`](../../hash_test.go)).

The paused run is a row in a SQLite file, so the process can exit and the
operator can decide the next day. Approving recomputes the hash from the
stored fields before accepting the decision, so a row whose request was
edited behind the runtime's back is refused
([`TestApproval_TamperedRowRefused`](../../approval_test.go)). Approving
does not continue the run. A separate `Resume` executes exactly the
recorded request and carries on
([`TestApproval_ApproveThenResumeExecutesRecordedRequest`](../../approval_test.go)).

A consumer uses the capability and presentation to bind what matters to
it. repo-steward's publication approval names the frozen proposal and its
own hash in the capability, and shows the branch, commit, and files in the
presentation
([`internal/policy/policy.go`](https://github.com/joeylking/repo-steward/blob/main/internal/policy/policy.go)).
A changed proposal produces a different approval hash, so an old approval
can never authorize it. The runtime's `NeedApproval` helper builds such a
decision, and both fields enter the hash
([`TestNeedApproval_PausesAndBindsWhatWasShown`](../../policy_test.go)).

## Bound to what was shown

A hash on the row catches a request edited without its hash. It did not,
at first, protect the decision itself. The operator command,
`agentrt approve`, read the approval, printed it, waited for the operator
to type yes, and then approved whatever the row held at that moment. The
hash contains no secret, so anyone able to write the row could replace the
request and store a hash that verified. The security review reproduced
exactly that: it changed the approval while the command was waiting for
the operator's answer, and the yes was applied to a request the operator
never saw.

The fix binds the decision to what was displayed. `ApproveShown` and
`RejectShown` take the hash of the approval the front end printed. They
compare it with the stored hash inside the transaction that records the
decision, and the database update is conditional on that hash, so nothing
can change the row between the check and the write. A mismatch returns
`ErrApprovalChanged`, nothing is decided, and the command asks the operator
to look again
([`TestDecide_BindsToTheHashActuallyShown`](../../cmd/agentrt/main_test.go),
[`TestApproval_ShownDecisionIsAtomic`](../../security_test.go)).

The same rule holds wherever an approval is decided away from the
database. The reference webhook channel, for a chat bot or a browser,
requires every decision to carry the hash its caller was shown, and
refuses a stale one with nothing decided
([`TestHandler_HashBindsAgainstAConcurrentChange`](../../approver/webhook/webhook_test.go),
[`TestHandler_StaleHashIsRefusedAndNothingDecided`](../../approver/webhook/webhook_test.go)).
Both consumers now decide this way: repo-steward's approve, reject, and
cancel, and casework's browser approval.

## Checked again on resume

An approval is not a standing permission. `Resume`
([`resume.go`](../../resume.go)) treats a grant as one input among several
and checks the rest before anything executes:

1. **Only the step's latest approval counts.** A step that paused again
   has a newer approval, and an older grant is spent. While the newest one
   is pending, `Resume` returns `ErrRunState` and changes nothing, however
   often it is called
   ([`TestResume_OnlyTheLatestApprovalOfAStepGrants`](../../security_test.go),
   [`TestResume_RepausedStepDoesNotPileUpApprovals`](../../security_test.go)).
2. **The hash is verified again.**
3. **The request is rebuilt from the tool as registered now,** and its
   arguments are checked against the tool's current schema. Arguments that
   no longer fit end the step as an invalid decision, and the agent
   decides again
   ([`TestResume_ApprovedArgumentsMeetTheCurrentSchema`](../../security_test.go)).
4. **The policy is asked again,** while the run is still waiting. The
   request executes only if the policy now allows it, or asks for exactly
   the approval that was granted, meaning the same hash. Any other answer
   pauses the run again with a new approval
   ([`TestApproval_ResumeRepausesWhenPolicyWantsDifferentApproval`](../../approval_test.go)).
   Because step 3 rebuilt the request, a tool whose description, schema,
   or timeout changed since the pause no longer matches the old hash
   ([`loop.go`](../../loop.go)).

The move out of waiting, the policy's new decision, and the tool's start
commit in one transaction, so a crash between them cannot drop the
approved request
([`TestResume_CrashAfterResumeKeepsTheApprovedRequest`](../../integrity_test.go)).
The move succeeds only if the run is still waiting, so of two processes
resuming the same approved run, one executes the request and the other gets `ErrRunState`
([`TestResume_ConcurrentResumeExecutesOnce`](../../integrity_test.go)).

## Approvals expire

Two limits bound how old an approval may be. `Limits.ApprovalTTL` bounds
how long one may wait for a decision; an approval past it cancels the run
as `approval_expired` the next time anyone touches it
([`TestApproval_ExpiryCancelsRun`](../../limits_test.go)).
`Limits.GrantTTL` bounds how long a granted approval may wait for
`Resume`, measured from the decision. A stale grant cancels the run with
nothing executed
([`TestResume_GrantNotResumedWithinTheTTLExpires`](../../security_test.go)),
and the first limit alone never expires a grant
([`TestResume_ApprovalTTLAloneNeverExpiresAGrant`](../../security_test.go)).
`GrantTTL` is off by default. casework, which is private, leaves it off
and instead has its tool re-check the specific approval, its expiry, and
every precondition before anything leaves the process
(`internal/tools/act.go` in casework).

## What the hash deliberately leaves out

The approval binds what the model asked for. For a tool from an MCP
server, the operator can fix a parameter's value in configuration: the
parameter is removed from the schema the model sees and injected into
every call after the runtime has validated the model's arguments. That
fixed value is outside the approval hash on purpose. It is the operator's
own setting, not part of the request, and the operator already knows it
([`mcp/load.go`](../../mcp/load.go),
[`TestFixed_RemovedFromTheSchemaAndInjectedAtCall`](../../mcp/tool_test.go)).
What keeps this honest is that a call carrying a value for a fixed
parameter fails rather than being silently overridden. Otherwise the
approval could show one value while the server received another
([`TestFixed_ACallThatSetsAFixedParameterFails`](../../mcp/tool_test.go)).

The hash also leaves out who decided and the note they wrote, which are
recorded beside it, and the code behind the tool's name.

## What this does not claim

- The hash is not a signature. It has no secret, so anyone who can write
  the database file can forge an approval or approve directly. That is why
  the runtime creates the file readable and writable by its owner only and
  refuses one that others can write
  ([`SECURITY.md`](../../SECURITY.md)). The hash catches corruption and
  code that mutates a record after it was fixed, not a local attacker.
- The name recorded as the approver is a label given by the caller, not a
  verified identity.
- An approval binds a request. It does not make the action safe, and the
  world can change between the decision and the execution. A tool that
  depends on outside state should check it again when it runs, as
  casework's does.
- A change to an operator's fixed MCP parameter between the approval and
  the resume is not caught by the hash.
