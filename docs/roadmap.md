# Roadmap

agent-runtime is a library that puts the actions of one tool-using agent
under deterministic control, whether the runtime runs the agent's loop or
the loop is the caller's own. Two consumers, [repo-steward](https://github.com/joeylking/repo-steward)
and [casework](https://github.com/joeylking/casework), run on the current
release. This roadmap turns what those consumers demonstrated into things
other people can use. It is ordered by leverage, and every item names the
consumer evidence that justifies it, because the rule from
[ADR 1](decisions/0001-custom-runtime.md) still holds: abstractions are
added when a consumer demonstrates the need, not before. One exception is
recorded: the gate, the first addition justified by outside research and
not by a consumer, which [ADR 7](decisions/0007-own-the-effect-not-the-loop.md)
explains. The rule still governs every other abstraction in the runtime.

## What the runtime is for

Teams who need one agent to touch a real system and later show what it
did and who authorized it. The loop, durable state, a pause for approval,
budgets, and tracing are standard in agent frameworks and in durable
execution engines, so owning the loop is not what this project is for: it
owns the effect, the moment a tool is called and something outside
changes. Any loop proposes an action, and the runtime decides, executes,
and records it. The `Driver` is the reference loop, and a `Gate` serves a
loop the runtime does not own. The controls that are not readily available
elsewhere, and that this project exists to provide:

- approvals bound by hash to the exact request, durable across process
  restarts, with policy re-evaluated on resume;
- an audit log written in the same transaction as the state it explains;
- call, token, cost, and time limits enforced before dispatch, with
  ambiguous attempts charged conservatively;
- fail-closed policy evaluated outside the model, with no handle the
  model can reach;
- deterministic tests of the whole loop with recorded, byte-identical
  model replay;
- an interruption contract under which the runtime never re-executes a
  side effect.

## What it is not for

Not a workflow engine, not a multi-agent orchestrator, not a prompt or
memory framework, not a service. Timers, fan-out, distributed workers,
memory, and retrieval belong to other tools, and so does deciding what an
agent does next: the runtime does not have to run the loop, and it does not
have to be the only thing that does. It stays a library that those tools
could embed. Items that would move it off this line are refused, not
deferred.

## Module layout

The project is pre-1.0, and this is the whole compatibility promise. Within a
minor version of the core, the exported API is additive only. A minor bump may
change or remove exported API and the release notes say which symbols and what
to do instead. The nested modules version independently, with their own
directory-prefixed tags, and each names the core version it requires in its
`go.mod`. The SQLite schema only migrates forward: a newer core opens a
database written by an older one and applies what is missing, and there are no
down migrations. Recordings stay valid across core versions as long as a
release has not changed what `render` produces for a step in them, because the
replay key is the model name and the canonical JSON of the request
(`replay.Key`); the release notes say when `render`'s output changes, as
README.md's Compatibility section does for v0.3.0.

The core module `github.com/joeylking/agent-runtime` keeps two
dependencies: a JSON Schema validator and SQLite. Everything below that
adds a dependency lives in a nested module in this repository with its
own `go.mod`, tagged with a directory prefix (`mcp/v0.1.0`,
`providers/ollama/v0.1.0`, `bench/v0.1.0`, `export/otel/v0.1.0`). A consumer imports only what it uses, and a
vulnerability in a provider SDK never taints the core.

## Items

### 1. Ship what both consumers rebuilt

**Problem.** Both consumers wrote an Ollama or Anthropic adapter, a
transient-error classifier, a price table with a paid-model refusal, a
step-log to message renderer with a recent-results window, an operator
surface over `runs.db` (list, show, approve, reject, cancel), and a
stderr trace observer. Three of those exist in near-identical copies. A
new consumer today rebuilds all of it before its first live run.

**Scope.**

- In the core module, dependency-free: `providers` helpers (transport
  classification, price lookup that refuses an unpriced paid model,
  dollar parsing, bounded body reads); `render` (steps to messages with
  synthesized tool-use ids, a recent-results window, a content cap, and
  hooks for the opening message and observation text); `trace` (stderr
  and JSON Lines observers); `view` (run, step, and approval summaries,
  and the pending-approval selection rule); `NeedApproval` and
  `DecodePresentation` helpers for policies; `cmd/agentrt` with `runs`,
  `show`, `events`, `approve`, `reject`, `cancel`.
- Nested modules: `providers/ollama`, `providers/anthropic` (official
  SDK, raw assistant turn round-trip so signed thinking blocks survive,
  opt-in strict schema subset), `providers/openai` (OpenAI-compatible
  chat completions, which covers most local servers).
- `examples/live`: one command against a local model with the scripted
  tools, printing the audit trail.

**Out of scope.** Streaming, embeddings, resume (it needs the consumer's
agent, tools, and policy; the CLI documents the pattern), a reference
`Agent`, and a runtime table for raw model turns. The last two wait for
item 4.

**Done when** a new consumer reaches its first live run with no code
beyond an `Agent`, its tools, and a policy; both existing consumers
delete their copies; and both consumers' recorded replays still pass,
which proves the extracted renderer and adapters are byte-faithful.

### 2. MCP tool adapter

**Problem.** MCP servers give agents a large, ungoverned tool surface.
Nothing in front of them enforces side-effect policy, hash-bound
approvals, limits, or an audit log. The MCP specification itself says
tool annotations are hints that clients must never base decisions on.

**Scope.** `mcp`: connect to an MCP server over stdio or streamable
HTTP, list its tools, and expose each as an `agentrt.Tool`.

- Registration refuses any tool without an operator-supplied side-effect
  class. Server annotations are recorded and cross-checked: an operator
  class more permissive than the server's own hint fails registration
  unless explicitly overridden.
- The tool set is pinned: name, input schema, and description are hashed
  into a manifest at registration. A server that later presents a
  different schema or description for a pinned tool has that tool
  refused, because descriptions are model input and a changed
  description is an injection vector.
- Operators may override descriptions and restrict a tool's schema
  (deny a parameter, fix its value).
- Results map `content` and `structuredContent` into `ToolResult`, with
  `isError` becoming an observation failure, and a size cap.

**Out of scope.** Resources, prompts, sampling, elicitation, and the
server side of MCP. A server that asks for more input
during a call has that call refused.

**Done when** `examples/mcp` runs a local model against a stock MCP
filesystem server with writes requiring approval; the load report shows
the unclassified tools refused, and the audit trail shows the allowed,
approved, and executed calls. This example
demonstrates every control at once and is the demo the project leads
with.

### 3. Evaluation vocabulary as a module

**Problem.** Most agent evaluations report completion only. repo-steward
scores each scenario with an explicit denominator over completed,
safe_nonresult, incorrect_refusal, correct_refusal, false_success, and
failed, and treats a single false success as disqualifying. That
vocabulary and the recording conventions are useful to anyone measuring
an agent that acts.

**Scope.** `bench`: the outcome taxonomy, a result file format, a
summarizer, and the convention that recordings and results are only
comparable within one commit. Scenario definition and oracles stay in
the consumer.

**Done when** repo-steward's `bench summarize` runs on the extracted
module unchanged, and casework publishes results in the same format.

**Status.** Built in the repository, shipped in core v0.3.1
with `bench/v0.1.0` and `export/otel/v0.1.0`. The module reproduces
repo-steward's nine result files and casework's published evaluation
exactly. Done: on 2026-10-03 repo-steward scores its benchmarks through
it, with its nine result files converted once and their numbers
unchanged, and casework publishes its evaluation in the same format with
a gate that now fails on any unsafe outcome.

### 4. Test kit for consumers

**Problem.** A consumer wants to assert that its policy denies a class of
request, that its tools survive interruption at any step, and that its
agent renders identical model context from identical steps, without a
model.

**Scope.** `testkit`: the scripted agent (already public), an
interruption harness that kills the loop after step N and resumes, a
policy conformance table, and schema fuzzing of tool arguments. The
generic parts of both consumers' model agents are examined here to
decide whether a reference `Agent` belongs in the runtime.

**Done when** both consumers replace their own interruption and policy
tests with the kit.

**Status.** Built in the repository as the core package `testkit`,
shipped in core v0.3.1. The examination found no reference `Agent`
worth shipping yet: about ten generic lines remain in each consumer's
agent, and the generic candidate is casework's raw model-turn table, which
would be a store table, not an `Agent`. Done: on 2026-10-03 repo-steward's
policy tests became conformance tables with a Never assertion and
fuzzed arguments over its tools, and casework's crash-recovery and policy
contract tests run on the kit, every old assertion kept.

### 5. Event export, not a UI

**Problem.** Operators already have dashboards. The runtime should feed
them rather than compete.

**Scope.** `export`: follow the events table and emit OpenTelemetry
spans (one per run, one per step, one per model attempt) or JSON Lines.
Since events are complete and committed with state, the follower needs
no other input.

**Done when** a run's trace appears in a stock collector with no code in
the consumer.

**Status.** Built in the repository: the core package `export` and the
nested module `export/otel`, shipped in core v0.3.1 and
`export/otel/v0.1.0`. Done: a run's trace appeared in a stock
OpenTelemetry collector on 2026-10-02, with no code in the consumer.

### 6. Portable approval channel

**Problem.** Approval today assumes the operator can reach the SQLite
file. Real operators are in chat or a browser.

**Scope.** An `Approver` interface over `Approve`, `Reject`, and
`Cancel`, plus a reference webhook implementation that presents the
approval's presentation JSON, verifies the caller, and honours expiry.

**Done when** repo-steward's publication approval can be granted from
the reference implementation and the hash still binds.

**Status.** Built in the repository as the core packages `approver` and
`approver/webhook`, with `examples/approver`, shipped in core v0.3.1. Proven against the real repo-steward binary: a publication approval
was granted through the webhook and the hash still bound on resume. Done:
on 2026-10-03 repo-steward's approve, reject, and cancel decide through
the approver and print the approval first, and casework's server decides
through it with the hash the browser was shown.

### 7. Publish the ideas

**Problem.** The design decisions travel further than a Go module.

**Scope.** Short pieces, in the wiki and cross-posted, on hash-bound
approvals, ambiguous-attempt accounting, policy outside the model, and
"the tables are the state, the events are the explanation". Each links
to the test that verifies the claim.

**Status.** Six essays published on 2026-10-03 under `docs/essays` and
on the wiki, each claim linked to the test that verifies it. The item
stays open: new ideas get written up as they are built.

### 8. Open-source hygiene

`CONTRIBUTING.md`, `SECURITY.md`, issue templates, runnable `Example_`
functions for pkg.go.dev, a stated pre-1.0 compatibility promise, a
public roadmap issue mirroring this file, and support for the current
and previous Go release.

## Direction

The gate is the first of a planned order of work. These are intentions, not
promises, and they are in this order. The exception ADR 7 records is for
the gate alone; each later item is justified when it is taken up.

### 9. The gate

**Problem.** The controls were available only to a loop the runtime owns,
which stopped anyone with a loop of their own from using them.

**Scope.** `Gate`, `Session`, `OpenStep`, and `Verdict`: the same controls
for a caller's loop, with the `Driver` running on the same code,
`Decision.Origin`, `Session.Input`, a `GrantTTL` re-check at `Execute`, and
`testkit.Scenario.Loop`.

**Done when** a loop written outside the runtime leaves the same record as
the `Driver`, and the crash harness runs over it.

**Status.** Built and verified, not yet released. ADR 7 has the
reasoning and the limits: a fresh step's gap between `Propose` and
`Execute` bounded by the lease and nothing else. The language-neutral
question ADR 7 left open was researched on 2026-10-05, with an inventory
of the code, research on how MCP hosts behave, an independent design, and
a measurement of how models answer a pending result. Its first form is
`agentrt-proxy`, a local MCP proxy over stdio in the nested module `proxy`,
which routes an MCP host's tool calls through a `Gate`, one run per call.
It is built as a small, scoped experiment, with what it cannot govern
stated first ([docs/proxy.md](proxy.md), [ADR 8](decisions/0008-mcp-proxy.md)),
and is not yet released. The Go library remains the full form.

### 10. Re-checkable decisions

**Problem.** An audit log says what was decided. It does not show that the
same policy would decide it again.

**Scope.** Each step records its tool spec by hash and the policy's
identity, events are hash-chained, and a finished run can be replayed
through a policy to show the same decisions.

### 11. Argument-source rules

**Problem.** A tool's arguments can carry authority, such as a path or a
recipient, and a model can be steered into supplying the wrong one.

**Scope.** Tool results an operator labels untrusted reach the loop as
opaque handles, and arguments that carry authority must come from the run's
goal, a trusted tool's result, or an approval. This constrains values. It
does not prevent prompt injection, and the documentation says so.

### 12. Policy-gated promotion

**Problem.** A step an agent repeats could run without a model call.

**Scope.** Research. Repeated agent steps may run as deterministic plan
steps under the same gate, where promotion is itself an approved, recorded,
reversible action. `Decision.Origin` exists for this: `plan` is a valid
origin and nothing uses it yet.

## Sequence

Items 1 and 2 shipped in v0.2.0 (core) with the nested modules at
v0.1.0, and both consumers deleted their copies with their recorded
replays passing unchanged. Item 8 shipped in v0.2.1 (core) with the
nested modules at v0.1.1; it came next because it needed no new code.
Items 3 through 6 shipped together in core v0.3.1 on 2026-10-02,
followed by `bench/v0.1.0` and `export/otel/v0.1.0`. Both consumers adopted
items 3, 4, and 6 on 2026-10-03, so items 1 through 6 and 8 are done.
The first six essays of item 7 were published the same day, in
`docs/essays` and on the wiki; v0.3.2 and `bench/v0.1.1` collected the
small additions the consumers' adoption asked for. Item 7 stays open for
each idea built from here.

The gate, item 9, was added on 2026-10-05 after the research pass ADR 7
records, and is not yet released; its first form outside Go, the stdio MCP
proxy of ADR 8, followed the same day and is not released either. The order from there is items 10, 11,
and 12, with item 12 as research.
