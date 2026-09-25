# Roadmap

agent-runtime is a library for running one tool-using agent under
deterministic control. Two consumers, [repo-steward](https://github.com/joeylking/repo-steward)
and [casework](https://github.com/joeylking/casework), run on v0.1. This
roadmap turns what those consumers demonstrated into things other people
can use. It is ordered by leverage, and every item names the consumer
evidence that justifies it, because the rule from
[ADR 1](decisions/0001-custom-runtime.md) still holds: abstractions are
added when a consumer demonstrates the need, not before.

## What the runtime is for

Teams who need one agent to touch a real system and later show what it
did and who authorized it. The controls that are not readily available
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
memory, and retrieval belong to other tools. The runtime stays a library
that those tools could embed. Items that would move it off this line are
refused, not deferred.

## Module layout

The project is pre-1.0, and this is the whole compatibility promise. Within a
minor version of the core, the exported API is additive only. A minor bump may
change or remove exported API and the release notes say which symbols and what
to do instead. The nested modules version independently, with their own
directory-prefixed tags, and each names the core version it requires in its
`go.mod`. The SQLite schema only migrates forward: a newer core opens a
database written by an older one and applies what is missing, and there are no
down migrations. Recordings stay valid across core versions, because the replay
key is the model name and the canonical JSON of the request (`replay.Key`); a
release that changed that shape would invalidate every recording and would say
so.

The core module `github.com/joeylking/agent-runtime` keeps two
dependencies: a JSON Schema validator and SQLite. Everything below that
adds a dependency lives in a nested module in this repository with its
own `go.mod`, tagged with a directory prefix (`mcp/v0.1.0`,
`providers/ollama/v0.1.0`). A consumer imports only what it uses, and a
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
server side of MCP. Servers that require `input_required` round trips
are refused at registration.

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

### 5. Event export, not a UI

**Problem.** Operators already have dashboards. The runtime should feed
them rather than compete.

**Scope.** `export`: follow the events table and emit OpenTelemetry
spans (one per run, one per step, one per model attempt) or JSON Lines.
Since events are complete and committed with state, the follower needs
no other input.

**Done when** a run's trace appears in a stock collector with no code in
the consumer.

### 6. Portable approval channel

**Problem.** Approval today assumes the operator can reach the SQLite
file. Real operators are in chat or a browser.

**Scope.** An `Approver` interface over `Approve`, `Reject`, and
`Cancel`, plus a reference webhook implementation that presents the
approval's presentation JSON, verifies the caller, and honours expiry.

**Done when** repo-steward's publication approval can be granted from
the reference implementation and the hash still binds.

### 7. Publish the ideas

**Problem.** The design decisions travel further than a Go module.

**Scope.** Short pieces, in the wiki and cross-posted, on hash-bound
approvals, ambiguous-attempt accounting, policy outside the model, and
"the tables are the state, the events are the explanation". Each links
to the test that verifies the claim.

### 8. Open-source hygiene

`CONTRIBUTING.md`, `SECURITY.md`, issue templates, runnable `Example_`
functions for pkg.go.dev, a stated pre-1.0 compatibility promise, a
public roadmap issue mirroring this file, and support for the current
and previous Go release.

## Sequence

Items 1 and 2 shipped in v0.2.0 (core) with the nested modules at
v0.1.0, and both consumers deleted their copies with their recorded
replays passing unchanged. Item 8 comes next because it needs no new
code. Items 3 through 6 follow as consumer evidence arrives; 7 is
continuous.
