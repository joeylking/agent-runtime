# agent-runtime

[![ci](https://github.com/joeylking/agent-runtime/actions/workflows/ci.yml/badge.svg)](https://github.com/joeylking/agent-runtime/actions/workflows/ci.yml)

A small Go runtime for executing tool-using agents under deterministic control.

The model loop is not the point. Any SDK gives you that. This runtime exists for
the controls around the loop:

- the agent asks, the runtime decides: every tool request is schema-validated and
  policy-evaluated before it executes, and the agent has no influence over policy;
- explicit, persisted state: runs, steps, and an append-only event log in SQLite;
- hard limits enforced by the driver, not by asking the model to be careful;
- deterministic testability: the whole loop runs against a scripted agent.

The first consumer is [repo-steward](https://github.com/joeylking/repo-steward), which supplies every
requirement for the public API. Abstractions are added when it demonstrates the
need, not before.

## Status

v0.2: the runtime that [repo-steward](https://github.com/joeylking/repo-steward)
and [casework](https://github.com/joeylking/casework) run on, with the
packages both rebuilt now shipped here and an MCP adapter that puts any
server's tools behind the policy ([docs/roadmap.md](docs/roadmap.md) items 1
and 2). The nested modules are tagged separately with a directory prefix:
`providers/ollama/v0.1.0`, `providers/anthropic/v0.1.0`,
`providers/openai/v0.1.0`, `mcp/v0.1.0`. [docs/architecture.md](docs/architecture.md) describes the loop,
approvals, interruption, and persistence; [docs/status.md](docs/status.md)
lists every control with the test that verifies it; [docs/decisions](docs/decisions)
records why it is built this way. The [wiki](https://github.com/joeylking/agent-runtime/wiki)
covers the same material at length for people using, extending, or evaluating
the runtime. The API is pre-1.0 and changes when its consumer needs it to.

### Compatibility

Pre-1.0, so the promise is narrow and written down rather than implied:

- within a minor version of the core, the exported API is additive only: a
  patch release adds symbols and fields and takes none away;
- a minor bump may change or remove exported API, and the release notes say
  which symbols and what to do instead;
- the nested modules version independently, with their own directory-prefixed
  tags, and each names the core version it requires in its `go.mod`, so
  upgrading one does not move the others;
- the SQLite schema only migrates forward. A newer core opens a database
  written by an older one and applies what is missing; there are no down
  migrations, so a database is not handed back to an older core;
- recordings stay valid across core versions. The replay key is the model name
  and the canonical JSON of the request (`replay.Key`), so a release that
  changed that shape would invalidate every recording and would say so.

[CONTRIBUTING.md](CONTRIBUTING.md) is how a change gets in, and
[SECURITY.md](SECURITY.md) is what the runtime does and does not defend against.

## Quick start

The core module requires Go 1.26 or later, so the current release and the one
before it both build it. The nested modules require 1.27 until they re-pin to a
core release declaring 1.26; their `go.mod` files say why.

The demo the project leads with is `examples/mcp`: a local model driving the
stock MCP filesystem server, where reads are allowed and a write stops the run
until an operator approves that exact call. It needs Node, for
`npx @modelcontextprotocol/server-filesystem`, an [Ollama](https://ollama.com)
server holding `qwen3:30b-a3b`, and a workspace, because the example and the
MCP adapter are nested modules:

```sh
go work init . ./providers/ollama ./providers/anthropic ./providers/openai ./mcp ./examples/live ./examples/mcp
go run ./examples/mcp pin      # hash the server's tools, print the hints it claims
go run ./examples/mcp run      # load the operator's rules, run to the first write
go run ./cmd/agentrt -db <path> approve <run>
go run ./examples/mcp run -resume <run>
go run ./cmd/agentrt -db <path> events <run>
```

`pin` prints one line per tool with the annotations the server asserts, which
classify nothing and are only there for the operator to disagree with.
[`examples/mcp/rules.json`](examples/mcp/rules.json) is the operator's answer:
four read tools, `write_file` as a local mutation with the server's own
destructive hint overruled in writing, and the other nine tools the filesystem
server offers left out, so `run` reports them unclassified and the model never
sees them. The run then pauses at `write_file` with the path, the line count,
and a preview of the content, and prints the two commands above with the run id
and the database filled in. The trail ends with the read that was allowed, the
write that waited, the operator who granted it, and the call that executed
after the grant.

The no-network path is `examples/scripted`, which needs neither a model nor
Node:

```sh
go test -race ./...
go run ./examples/scripted
```

It runs a scripted agent against three in-memory tools, including one
malformed request the runtime rejects and one destructive request the policy
denies, then prints the persisted event trace.

## Shape of the API

```go
store, _ := agentrt.OpenStore("runs.db")
driver, _ := agentrt.NewDriver(agentrt.Config{
    Store:  store,
    Agent:  myAgent,               // Decide(ctx, StepInput) (Decision, error)
    Policy: agentrt.DefaultPolicy(), // read/local allow, remote approval, destructive deny
    Tools:  []agentrt.Tool{...},   // Spec() with JSON schema, side effect, timeout
})
run, _ := driver.Start(ctx, "goal", agentrt.DefaultLimits())
```

## What a consumer no longer has to write

Both existing consumers rebuilt the same provider plumbing, renderer, trace,
and operator surface. These packages are in the core module and add no
dependency beyond the standard library:

- `providers`: transport and HTTP classification for a provider adapter (a
  timeout returned bare, retryable statuses transient), a bounded body read,
  tool-use id synthesis, a price lookup that refuses an unpriced paid model,
  and dollar parsing for budgets.
- `render`: a run's recorded steps to model messages, with synthesized
  tool-use ids, a recent-results window, a content cap, and hooks for the
  opening message, the assistant turn, and the observation; plus
  `render.Decide`, which maps a response to a decision and turns a truncated
  or tool-less reply into a deliberately invalid decision the runtime records
  and the next render nudges.
- `trace`: aligned and JSON Lines observers.
- `view`: run, step, and approval read models, and the rule for choosing the
  approval an operator means.

```sh
go run ./cmd/agentrt -db runs.db runs
go run ./cmd/agentrt -db runs.db show <run>       # steps and the waiting approval
go run ./cmd/agentrt -db runs.db events <run>     # the audit log, -json for JSONL
go run ./cmd/agentrt -db runs.db approve <run> -by joey -note "ok"
```

`cmd/agentrt` reads `-db` or `AGENTRT_DB`, prints aligned text or `-json`, and
exits 0, 1, or 2. It has no `resume`: resuming executes the approved request
and continues the loop, which needs the consumer's agent, tools, and policy.

A run ends `COMPLETED`, `FAILED` with a terminal reason, or pauses in
`WAITING_FOR_APPROVAL` on a hash-bound approval that `Approve` and `Resume`
continue. A `Model` given to the driver is wrapped in an accounting caller
that agents receive in `StepInput`: it records every attempt, enforces call,
token, and cost limits before dispatch, and retries transient failures. The
`replay` package provides scripted, recording, and replaying models so tests
never call a provider.

## Provider adapters

Each adapter is a nested module with its own `go.mod`, so a consumer imports
only the provider it uses and a provider SDK never reaches the core:

- `providers/ollama`: a local Ollama server over its native chat API, with
  no dependency beyond the standard library.
- `providers/openai`: any OpenAI-compatible chat completions endpoint, which
  covers most local servers, with no dependency beyond the standard library.
- `providers/anthropic`: the Claude Messages API through the official SDK,
  with the raw assistant turn round trip that keeps signed thinking blocks
  alive across a continuation, an opt-in strict schema subset, and a dated
  price table.

```sh
go get github.com/joeylking/agent-runtime/providers/ollama
```

All three implement `agentrt.Model` the same way: a `Config` carrying the
model id, `New(Config) (*Model, error)`, no streaming, no retries because the
accounting caller retries and records every attempt, the provider's raw bytes
on every response, and a stop reason mapped to `tool_use`, `end_turn`,
`max_tokens`, or `refusal`. A reply that was cut off or refused comes back
with no tool uses, so a partial call can never execute, and with its usage,
so the attempt is still charged.

`Name()` is `<provider>:<model>` unless the config overrides it, so one price
table and one recording directory can hold several providers.
`anthropic.Prices()` prices the paid models; `ollama.Free(...)` and
`openai.Free(...)` record local ones as free, which is what
`providers.PriceFor` needs to tell a free model from an unpriced one.

No test calls a provider: the Anthropic adapter is exercised against an
`httptest` fake, and each local adapter has one round trip behind the `live`
build tag that skips unless a server answers. Working on the nested modules
needs a workspace, which is what CI builds:

```sh
go work init . ./providers/ollama ./providers/anthropic ./providers/openai ./mcp ./examples/live ./examples/mcp
```

## MCP servers behind the policy

`mcp` is a nested module that exposes an MCP server's tools as
`agentrt.Tool`s, so they pass the same validation, policy, limits, and audit
log as any other tool. The MCP specification says a client must never make
decisions from tool annotations, because an untrusted server supplies them,
which is the premise of the design:

- a tool with no operator `Rule` naming its side-effect class is not
  registered, and the load report says so;
- `Pin` records each tool's name, description, and input schema in a manifest
  the operator reviews and stores; `Load` refuses any tool whose live schema
  or description no longer hashes to the pin, because a description is model
  input and a changed one is an injection vector;
- annotations are recorded and cross-checked. They can only contradict an
  operator's classification, never relax it, and a contradiction refuses the
  tool unless the rule allows the mismatch, which the report records;
- an operator may override a description, deny a parameter, and fix a
  parameter's value; a denied or fixed parameter is removed from the schema
  the model sees;
- the model sees `<server>_<tool>` unless the rule renames it, so two servers
  cannot collide;
- `isError` becomes a tool error the agent must work around, content is
  capped, and a server asking for more input is refused.

```go
manifest, _ := mcp.Pin(ctx, server)            // review and store this
tools, report, _ := mcp.Load(ctx, server, manifest, rules)
defer report.Connection.Close()
```

[ADR 4](docs/decisions/0004-operator-classified-tools.md) records why the class
is the operator's and why a hint may only refuse. `examples/mcp` is the whole
thing running: the quick start above is its four commands.

## A live run that stops for an operator

`examples/live` is the shortest complete consumer: the tools from
`examples/scripted` plus a terminal one to finish on, an `Agent` that is
`render.Messages` and `render.Decide` around the model, a stderr trace, and a
policy that sends the first change to the counter to an operator. It prints
the approve command with the run id filled in.

```sh
go run ./examples/live                              # pauses; prints the two below
go run ./cmd/agentrt -db <path> approve <run>
go run ./examples/live -db <path> -resume <run>
```

It runs `ollama:qwen3:30b-a3b` by default; `-model openai:<id>` with
`-base-url http://127.0.0.1:11434/v1` runs the same agent through the
OpenAI-compatible adapter.
