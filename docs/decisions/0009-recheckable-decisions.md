# 9. Record what a decision needs to be decided again, and chain the events

Decision: each decision records what it takes to decide it again, the events
form a hash chain, and a policy is handed one defined form of the request, so
that a finished run can be replayed through a policy and the record checked
for alteration. A tool call's step records the hash of the tool's spec, with
the spec stored once by that hash in `tool_specs`, and the identity of the
policy that evaluated it, when the policy implements `IdentifiedPolicy`. A
`step.policy` event carries the fields of the run the policy saw and the
record does not otherwise keep, as `view`. Every event's hash covers the
previous event's and its own fields as stored, computed in the transaction
that writes it, and `Store.VerifyEvents` and `agentrt verify` find the first
event that does not chain. `agentrt.Recheck` rebuilds each recorded policy
evaluation, asks a policy, and reports where it decides differently. A policy
is handed the request's arguments and its tool's schema in `PolicyJSON` form
on every path: the loop, a gate's `Propose` and `Attach`, `Resume`, and a
re-check. Migration 6 adds the columns and backfills the chain. The core
module, the proxy, and `export/otel` carry it, and it is released in core
v0.5.0.

Why: an audit log says what was decided. Nothing in it showed that the same
policy would decide the same again, and nothing showed that the log itself
was intact. Worse, a policy was handed different bytes on the loop and on
resume: the loop gave it the agent's own bytes and the schema as registered,
and a resume gave it the stored request, compacted and HTML-escaped. So even
a policy that reads only its input could not be held to its record, because
one request reached it in different forms. The policy sits outside the model
([ADR 3](0003-policy-outside-the-model.md)), but the record could show only
that it ran. What an operator needs, after a policy changes or a run
surprises them, is to put a finished run in front of a policy and see whether
it would decide the same, and to know the record is the one the runtime
wrote.

**What is recorded.** A decision needs the request, the tool's spec, and what
the policy could see of the run. The request is already in the step. The
spec is not, and a tool's description, timeout, or terminal flag can change
between runs, so each step records the spec's hash and the spec is stored
once under it. The run's status, counts, tokens, cost, and active time at the
moment of the evaluation are in no row, because the rows move on, so the
event carries them as `view`. With `run.created`, the steps as stored, and
the approvals as they stood by the seq of their decisions, that rebuilds the
run the policy was handed. Rebuilding reads the record and asks only the
policy. The tables stay the state and the events stay the explanation:
nothing is replayed to reconstruct a run.

**The form a policy is handed.** `PolicyJSON` sorts keys, drops insignificant
whitespace, keeps each number as written, and escapes only what JSON
requires. It is the canonical form without the HTML escaping, so the same
bytes come out of the agent's pretty-printed input, the stored and escaped
request, and the stored canonical spec. The approval hash, the content hash,
loop detection, and the arguments a tool is called with are unchanged.
This is a behaviour change for a policy that matches bytes (one denying `&&`
in an argument could decide one way live and another on resume), which is the
case it removes; a policy that unmarshals what it is handed is unaffected.
The release notes say so.

**The chain.** The hash is the SHA-256 of seven netstrings, so another
language can recompute it with nothing but SHA-256. The chain runs by seq
across every run and not per run, so an event deleted from the middle of
another run's record, or a run deleted whole, breaks it where a per-run chain
would not. Each write reads the head inside its IMMEDIATE transaction, so
concurrent writers extend one chain. `Event` has no hash field, and a trace
is byte for byte what it was. `export.Follower.Head` returns the seq the
follower delivered up to and that event's hash, as the anchor an operator
keeps, and `agentrt verify -head SEQ:HASH` checks it.

**What this does not show.** These limits are part of the decision.

- The chain proves integrity only against a head kept where whoever can write
  the database cannot reach. A writer can recompute every hash after the row
  they changed, or cut events from the end, and the chain still verifies.
  Without a kept head it catches corruption and a careless edit, nothing more.
- Events stored before migration 6 were hashed when it ran. Their hashes say
  what they held then, not that they were never changed before, and the
  steps already stored have no spec or policy identity. A policy evaluation
  recorded then re-checks as "not re-checkable"; one made after the
  migration, as the resume of a run left waiting, is re-checked.
- A re-check proves the policy's answer, not the tool's behaviour. It does not
  run a tool, and it trusts what it reads: it does not show the record
  unaltered, which is the chain's work.
- A policy that reads outside state, such as a clock, a counter, a file, or a
  service, cannot be re-checked exactly. repo-steward's policy is one: it
  decides from workspace facts, so its recorded runs would differ when
  re-checked. That is a determinism finding about that consumer, which the
  check is there to make, and not a defect of the check.
- The proxy's rules applied before its policy (the duplicate rule, blocked
  tools, the pending cap, and a rejected or expired request not asked for
  again) are not re-checked. `agentrt-proxy recheck` covers the gate's policy
  and says what it leaves out.
- A `step.policy` event, or a step, longer than a page (`MaxPageText`) is not
  re-checkable, because a policy never saw a cut value.
- The state tables are not chained. An approval's own hash still binds the
  approval.

**Alternatives.** Event sourcing, rejected in
[ADR 2](0002-persisted-state-not-event-sourcing.md) and rejected again: a
re-check reads the record and reconstructs nothing, so the tables remain the
state. Storing the raw bytes a policy saw instead of defining a form, which
would be exact for the loop and for resume separately and would preserve the
inconsistency between them: the same request would still reach a policy as
different bytes, and a re-check would answer for one of them. A chain per
run, which is simpler to verify and cannot see a deleted run. Signing the
events, deferred: a secret in the writer's process proves nothing against the
writer, and a key kept out of its reach is a deployment the project does not
have.

**Tradeoffs.** Every event write reads the head and hashes seven fields, in
the transaction that already holds the write lock. The migration computes a
hash for every event already stored, in one transaction, so it takes time in
proportion to the log. A process of v0.4.0 or earlier that writes after the
migration appends events with no hash and breaks the chain at the first, so
every older process is stopped before the first open, and a v0.4.0 binary
refuses a migrated database. A policy that matches bytes may notice the
form. The surface to keep stable is larger by `IdentifiedPolicy`,
`PolicyJSON`, `Recheck` and its report, the verify methods, and the testkit
helpers.

Revisit when: a consumer needs events signed, which means a signer outside the
writer's reach; the state tables need the same assurance as the events; or a
policy that reads outside state needs a recheck that supplies what it read.
