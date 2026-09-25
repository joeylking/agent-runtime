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

## What the agent sees and cannot do

`StepInput` carries the run, every prior step in order, the approvals, the
tool specs, and a `ModelCaller`. The agent is stateless between steps and
renders its own model context from the recorded steps, so the audit log
and the agent's view are the same data. The `ModelCaller` is the agent's
only handle to a model: it reserves a row per attempt before dispatch,
records usage, latency, and cost, enforces call, token, and cost limits
before every request, and retries transient failures. The agent has no
handle to the policy or the store.

## Approvals

A `require_approval` outcome stores the kind, capability, presentation,
and the exact request, with a hash over all four. `Approve` recomputes the
hash before accepting the decision. `Resume` re-evaluates policy and
executes the recorded request only when policy allows it or asks for
exactly the approval that was granted; a different answer pauses again.
Approvals may carry an expiry that cancels the run when next touched.
`Approve`, `Reject`, and `Cancel` exist as package functions over a
`Store` as well as driver methods: the package functions are the operator
path, used by `cmd/agentrt`, because an operator holds the database and
not the consumer's agent, tools, or policy.
`Cancel` ends any non-terminal run as `operator_cancelled`; it exists
because an approval, once granted, cannot be rejected, so a run approved
but never resumed would otherwise have no way to be closed.

## Interruption

A run found RUNNING with a step in flight is resumed by marking the step
interrupted with an observation, calling the consumer's reconciliation,
and acting on its outcome: continue the loop, complete the run with a
recovered result, wait for approval, or fail as a conflict. A side effect
is never re-executed by the runtime; the consumer reconciles against its
own journals and the outside world.

## Persistence

One SQLite file in WAL mode with runs, steps, model_calls, approvals,
events, and a schema_migrations table. A consumer may keep its own tables
in the same file under its own migrations. The database is trusted as the
operator's filesystem is; hashes catch corruption and code paths that
mutate a record after it was fixed, not a local attacker.

## Modules

The core module `github.com/joeylking/agent-runtime` holds the loop, the
store, policy, limits, and the helpers that need nothing beyond the
standard library. Anything that adds a dependency is a nested module in
this repository with its own `go.mod`, tagged with a directory prefix, so a
consumer imports only what it uses and a provider SDK never reaches the
core.

- the core module: the driver, the SQLite store, policy and limits,
  `providers`, `render`, `trace`, `view`, `replay`, `scripted`,
  `cmd/agentrt`, and `examples/scripted`.
- `providers/ollama`: a local Ollama server over its native chat API.
- `providers/anthropic`: the Claude Messages API through the official SDK,
  with the raw assistant turn that keeps signed thinking blocks alive.
- `providers/openai`: any OpenAI-compatible chat completions endpoint.
- `mcp`: an MCP server's tools as runtime tools, pinned by hash and
  classified by an operator.
- `examples/live`: one live model run against in-memory tools that stops
  for an operator before the first change.
- `examples/mcp`: the same controls over the stock MCP filesystem server,
  where reads are allowed and a write waits for an approval.

## What is deliberately absent

Multi-agent orchestration, a planning DSL, prompt templates, memory or
retrieval, a multi-provider abstraction, a service mode, and a UI. Each
would be added when a consumer demonstrates the need, not before.

The MCP adapter covers tools only: resources, prompts, sampling,
elicitation, and the server side of MCP are out of scope.
