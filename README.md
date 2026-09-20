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

v0.1: the runtime that [repo-steward](https://github.com/joeylking/repo-steward)
runs on. [docs/architecture.md](docs/architecture.md) describes the loop,
approvals, interruption, and persistence; [docs/status.md](docs/status.md)
lists every control with the test that verifies it; [docs/decisions](docs/decisions)
records why it is built this way. The [wiki](https://github.com/joeylking/agent-runtime/wiki)
covers the same material at length for people using, extending, or evaluating
the runtime. The API is pre-1.0 and changes when its consumer needs it to.

## Quick start

Requires Go 1.27 or later. No network access is needed after modules download.

```sh
go test -race ./...
go run ./examples/scripted
```

The example runs a scripted agent against three in-memory tools, including one
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

A run ends `COMPLETED`, `FAILED` with a terminal reason, or pauses in
`WAITING_FOR_APPROVAL` on a hash-bound approval that `Approve` and `Resume`
continue. A `Model` given to the driver is wrapped in an accounting caller
that agents receive in `StepInput`: it records every attempt, enforces call,
token, and cost limits before dispatch, and retries transient failures. The
`replay` package provides scripted, recording, and replaying models so tests
never call a provider.
