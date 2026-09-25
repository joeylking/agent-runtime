# Contributing

## What gets added

[ADR 1](docs/decisions/0001-custom-runtime.md) sets the rule: an abstraction is
added when a consumer demonstrates the need, not before. The two consumers are
[repo-steward](https://github.com/joeylking/repo-steward) and
[casework](https://github.com/joeylking/casework). Everything public in the
runtime today exists because one of them wrote it first, or wrote it twice.

So a change starts with the need, not the code. Open an issue with the
**consumer need** template: who the consumer is, what it had to build itself,
why the runtime is the right home for it, and which
[roadmap](docs/roadmap.md) item it supports. That is the currency the roadmap
is priced in.

A proposal with no consumer behind it is refused rather than deferred, and so
is anything that moves the project off the line in the roadmap's "What it is
not for": a workflow engine, a multi-agent orchestrator, a prompt or memory
framework, a service.

The maintainer reviews and merges. There is no CLA.

## Module layout

The core module `github.com/joeylking/agent-runtime` keeps two dependencies: a
JSON Schema validator and SQLite. Anything that adds a dependency lives in a
nested module in this repository with its own `go.mod`, tagged with a directory
prefix (`mcp/v0.1.0`, `providers/ollama/v0.1.0`), so a consumer imports only
what it uses and a provider SDK never reaches the core. A change that would add
a third dependency to the core belongs in a nested module instead.

Nested modules pin the core to its release tag, so working across them needs a
workspace, which resolves the in-tree core instead:

```sh
go work init . ./providers/ollama ./providers/anthropic ./providers/openai ./mcp ./examples/live ./examples/mcp
```

`go.work` is not committed.

The core's `go` directive is the previous Go release, so a consumer on either of
the last two builds it, and CI runs the core on both. A nested module's
directive is whatever the core release it pins requires, which `go mod tidy`
enforces; the comment above each one says so. Working on a nested module
therefore needs 1.27 today, even in the workspace, because an older toolchain
refuses the module graph. Do not raise the core's directive without a reason a
consumer can read.

## Running every suite

The core module. `GOWORK=off` keeps the workspace out of it, so the core is
exercised with only its own two dependencies, which is how CI sees it:

```sh
GOWORK=off go vet ./...
GOWORK=off go test -race -count=1 ./...
```

Each nested module, through the workspace:

```sh
for m in providers/ollama providers/anthropic providers/openai mcp examples/live examples/mcp; do
  (cd "$m" && go vet ./... && go test -race -count=1 ./...)
done
```

The live tests are behind the `live` build tag: they call a local
[Ollama](https://ollama.com) server holding `qwen3:30b-a3b`, so they are free
but neither deterministic nor available in CI, and they skip when nothing
answers. `providers/openai` uses the same server through its `/v1` endpoint.
Run them before changing an adapter:

```sh
(cd providers/ollama && go test -tags live -count=1 ./...)
(cd providers/openai && go test -tags live -count=1 ./...)
```

The examples. `examples/scripted` needs nothing and CI runs it; `examples/live`
needs the Ollama server; `examples/mcp` needs that and Node, for
`npx @modelcontextprotocol/server-filesystem`, and is the demo that exercises
every control at once:

```sh
go run ./examples/scripted
go run ./examples/live
go run ./examples/mcp pin && go run ./examples/mcp run
```

`Example` functions carry `// Output:` comments that `go test` verifies, so
they are tests as well as documentation. Keep their output deterministic: fix
ids with `Config.NewID` and print no timestamps.

## Recordings

No test calls a provider. `replay.Recorder` captures a real model's exchanges
once and `replay.Replayer` serves them afterwards, byte-identical. The key is
the SHA-256 of the model name and the canonical JSON of the rendered request
(`replay.Key`), so anything that changes what goes on the wire changes the key
and the recording is no longer found.

That means a change to the rendered prompt, to a tool's name, description, or
input schema, or to a provider adapter's request mapping needs the consumers'
recordings re-recorded, in the consumer, before the change lands. A recording
is only comparable within one commit. Identical requests replay in the order
they were recorded, and re-recording a key replaces its whole sequence.

## Style

Read a neighbouring file first; these are the rules it follows.

- Doc comments are terse and say why, not what the code already says. Every
  exported symbol has one.
- Every exported symbol is justified by a consumer requirement. If you cannot
  name the consumer, keep it unexported.
- No speculative abstraction: no interface with one implementation and no
  option nobody passes.
- Tests are named `TestX_Behaviour`, where `X` is the thing under test and the
  behaviour is a sentence about what must hold —
  `TestApproval_TamperedRowRefused`, not `TestApproval2`.
- Every control in [docs/status.md](docs/status.md) names the test that
  verifies it. A new control adds its row.
- A decision that constrains the design goes in `docs/decisions` as a short
  ADR: decision, why, alternatives, tradeoffs, revisit when.

## Commits

An imperative subject naming the change, then a body that says why it was
needed. The subject is what the release notes read like:

```
Refuse a pinned tool whose description changed

A description is model input, so a server that rewords one between the
review and the run has rewritten the instructions the model reads. Pinning
the schema alone would let that through.
```

No attribution trailers.
