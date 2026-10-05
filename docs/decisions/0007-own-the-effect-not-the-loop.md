# 7. The runtime owns the effect, not the loop

Decision: the runtime's controls are offered to a loop it does not own.
A `Gate` takes each action a caller's loop proposes and does what the
`Driver` does with its agent's: records the decision verbatim, checks the
arguments against the tool's schema, evaluates policy, pauses on a
hash-bound approval, enforces the limits, takes the run's lease, executes
the tool, and writes the observation and the audit events in the same
transactions. The caller decides; the gate decides whether it runs, runs
it, and records it. The `Driver` stays, as the reference loop and the one
both consumers use, and it now runs on the same code: a `Driver` is a gate
with an agent, so there is one implementation of the sequence and not two
that could drift. A gate executes the tool itself. A path where the caller
runs the tool and reports the outcome back is refused.

Why: a research pass on 2026-10-05 compared the project with agent
frameworks, durable execution engines, automation platforms, and agent
security tooling. The loop, durable state, pause-and-resume approval,
budgets, and tracing are now standard in every agent framework and every
durable execution engine. Owning the loop is no longer a reason to adopt
this library, and it stops anyone who already has a loop from adopting it
at all: the choice was to replace theirs.

What was not found elsewhere in Go, by reading the code of Microsoft's
Agent Governance Toolkit Go package, Google's ADK Go, and Eino, is the
part this project was built around: an approval bound by hash to the exact
request, durable, with policy re-evaluated on resume; the rule that an
interrupted side effect is not run again without a reconciliation or an
operator; an audit event committed with the state change it explains; and
MCP tools pinned by hash and classified by an operator. None of these is
about the loop. Each is about the effect, the moment a tool is called and
something outside changes. The project's identity is therefore to own the
effect and not the loop: any loop proposes an action, and the gate decides,
executes, and records.

The gate executes the tool because the runtime's strongest claims depend on
it. An action the policy gated runs once per approval, and an interrupted
side effect waits for an operator, because the runtime writes the tool's
start before the call and its outcome after, under the lease. If the caller
ran the tool and reported back, the runtime would be recording a claim:
at-most-once would become the caller's promise, and the audit log would say
what a program said happened. A gate that executes keeps both in the
runtime. The cost is that a tool is registered with the gate and not left
in the caller's own code.

This is the first addition justified by outside research and not by a
consumer's demonstrated need, which [ADR 1](0001-custom-runtime.md) and the
roadmap require. No consumer asked for it: repo-steward and casework run on
the `Driver` and are unchanged. The rule still governs abstractions inside
the runtime, and this exception is deliberate, decided once, and recorded
here. It does not set a precedent for the next addition, which needs a
consumer as before.

Nothing a `Driver` consumer sees changes. The test that holds this is
`TestGate_ExternalLoopMatchesTheDriver`: a loop written outside the runtime
leaves the same rows, events, payloads, and clock readings as the `Driver`
running the same decisions. The additions that came with the gate are
small. `Decision.Origin` records who proposed a step (`model`, `plan`, or
`operator`), changes no stored byte when empty, and is not shown to the
policy. `Limits.GrantTTL` is judged again at `Execute`, because a caller
holding an approved step can wait where a `Driver` cannot. `Session.Input`
gives a loop that decides from the run's record what an agent is handed.
`testkit.Scenario.Loop` runs the crash harness over a caller's own loop.

Alternatives: leaving the runtime as a loop, which keeps the project
small and leaves it where every framework already is. A proxy or
middleware that sits between a loop and its tools, which would work for
any language; whether such a form should exist is being researched
separately and is undecided, and a Go gate does not rule it out. A path
where the caller executes and reports the outcome, refused above. Two
implementations of the sequence, one for the `Driver` and one for the
gate, which would drift: the first difference between them would be a
control that held for one and not the other.

Tradeoffs: the gate is Go only. A caller's loop is trusted as an `Agent`
implementation is: it is the consumer's own code in the consumer's own
process, and the gate bounds what it can make happen, not what it
decides. A fresh step's gap between `Propose` and `Execute` is covered by
the lease and by no other limit, as an agent's slow `Decide` is; the time
limits are checked when the step starts. The grant of an approved step has
no lease while it waits, so `GrantTTL` is its bound. A step's `ModelCaller`
remains usable after its step ends, as an agent's does under the `Driver`;
it is held to the run's limits and refuses to send once the lease is lost.
A session that is abandoned leaves the record as a crash would, and
`Attach` handles it by the interruption contract of
[ADR 6](0006-interrupted-side-effects.md). The surface the project must
keep stable is larger by a type and a handful of methods.

Revisit when: a loop in another language needs the same controls, which
may call for a form the gate is not, or when a consumer's own loop shows the
session needs something it lacks. The planned direction is in the
[roadmap](../roadmap.md).
