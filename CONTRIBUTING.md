# Contributing

## What gets added

[ADR 1](docs/decisions/0001-custom-runtime.md) sets the rule: an abstraction is
added when a consumer demonstrates the need, not before. The two consumers are
[repo-steward](https://github.com/joeylking/repo-steward) and
[casework](https://github.com/joeylking/casework). Everything public in the
runtime today exists because one of them wrote it first, or wrote it twice,
with one deliberate exception: the `Gate`, which came from outside research,
and which [ADR 7](docs/decisions/0007-own-the-effect-not-the-loop.md)
records. It does not loosen the rule for anything else.

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
prefix (`mcp/v0.1.0`, `providers/ollama/v0.1.0`, `bench/v0.1.0`,
`export/otel/v0.1.0`), so a consumer imports only
what it uses and a provider SDK never reaches the core. A change that would add
a third dependency to the core belongs in a nested module instead.

Nested modules pin the core to its release tag, so working across them needs a
workspace, which resolves the in-tree core instead:

```sh
go work init . ./providers/ollama ./providers/anthropic ./providers/openai ./mcp ./bench ./export/otel ./proxy ./examples/live ./examples/mcp
```

`go.work` is not committed.

The core's `go` directive is the previous Go release, so a consumer on either of
the last two builds it, and CI runs every module on both. A nested module's
directive can be no lower than what the core release it pins requires, which
`go mod tidy` enforces, so a core release that raises its directive drags the
nested modules up at their next re-pin. Do not raise the core's directive
without a reason a consumer can read.

## Running every suite

The core module. `GOWORK=off` keeps the workspace out of it, so the core is
exercised with only its own two dependencies, which is how CI sees it:

```sh
GOWORK=off go vet ./...
GOWORK=off go test -race -count=1 ./...
```

Each nested module, through the workspace:

```sh
for m in providers/ollama providers/anthropic providers/openai mcp bench export/otel proxy examples/live examples/mcp; do
  (cd "$m" && go vet ./... && go test -race -count=1 ./...)
done
```

## Lint

`gofmt`, [staticcheck](https://staticcheck.dev), and
[govulncheck](https://golang.org/x/vuln/cmd/govulncheck) run in CI's `lint`
job on the newest supported Go only, plus a tidy check on the core.
Install the two tools at the versions CI pins (`.github/workflows/ci.yml`
names the current ones):

```sh
go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
```

The core, `GOWORK=off` so it is checked with only its own two dependencies:

```sh
gofmt -l .
GOWORK=off go mod tidy -diff
GOWORK=off staticcheck ./...
GOWORK=off govulncheck ./...
```

`gofmt -l .` must print nothing; `go mod tidy -diff` must print nothing and
exits non-zero when `go.mod` or `go.sum` would change.

Each nested module, through the workspace (`staticcheck` and
`govulncheck` only — a tidy check against the nested module's pinned core
version belongs to the release check below, not here, because the
workspace overrides that pin):

```sh
for m in providers/ollama providers/anthropic providers/openai mcp bench export/otel proxy examples/live examples/mcp; do
  (cd "$m" && staticcheck ./... && govulncheck ./...)
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
needs the Ollama server; `examples/mcp` needs that and Node, and is the demo
that exercises every control at once. It runs the MCP filesystem server pinned
by version and integrity hash in `examples/mcp/package-lock.json`, started with
`node` from a local install, so install it once, without running any package's
install scripts, before `pin` or `run`:

```sh
go run ./examples/scripted
go run ./examples/live
(cd examples/mcp && npm ci --ignore-scripts)
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

## Releasing

The core and each nested module tag independently
(`v0.2.1`, `mcp/v0.1.1`, `providers/ollama/v0.1.1`, ...), and CI's
`release-check` workflow exists because `go work` overriding every nested
module's pinned core version, which is what makes day-to-day development
across modules practical, also means ordinary CI never builds the
dependency graph a consumer actually resolves. The order below exists so
that graph is proven to build, with `GOWORK=off`, before anything is
tagged:

1. **Tag the core** only once CI is green on the exact commit being
   tagged — not a later commit, and not a commit CI hasn't run against
   at all.
2. **Re-pin the nested modules** to the new core version. The proxy and the
   checksum database can lag a freshly pushed tag by a few minutes; wait for
   the proxy to actually serve it, then let `go get` do the re-pin, then
   confirm what it wrote against the checksum database independently. For
   each nested module that requires the core (`providers/ollama`,
   `providers/anthropic`, `providers/openai`, `mcp`, `export/otel`), as below.
   `export/otel` must be re-pinned to a core release that contains the
   `export` package its example imports. `bench` has no requirements at all,
   not even the core, so it has nothing to re-pin: it is tagged after the
   core like the others, and this step does not apply to it. `proxy`
   requires `mcp` as well as the core, so like the examples it is re-pinned
   only once the new `mcp` tag exists, in step 5, and tagged after that.

   ```sh
   # Wait until the proxy serves the tag just pushed.
   until curl -sf https://proxy.golang.org/github.com/joeylking/agent-runtime/@v/vX.Y.Z.info; do sleep 5; done

   (cd providers/ollama && go get github.com/joeylking/agent-runtime@vX.Y.Z)

   # Confirm the go.sum lines go get just wrote independently, against the
   # checksum database, before trusting them.
   curl -sf https://sum.golang.org/lookup/github.com/joeylking/agent-runtime@vX.Y.Z
   ```

   Never bypass the checksum database to make a re-pin go through —
   `GONOSUMDB`, `GOPRIVATE`, `GOFLAGS=-mod=mod`, or `GOPROXY=direct` all skip
   the verification that catches a tag whose content changed after the fact
   or a proxy serving something other than what was pushed, and none of them
   is needed here: the core's modules are public, and `go get` at default
   settings re-pins and verifies both together. If the proxy or the checksum
   database has not caught up, wait; do not route around them. Then run
   `go mod tidy` in each, and push the result as an ordinary commit.
3. **Wait for both CI and the release check to go green** on that commit.
   The release check does not trigger on an ordinary push (see the
   comment at the top of `.github/workflows/release-check.yml`), so run it
   with `workflow_dispatch` for this commit, with the `examples` input off
   because the examples cannot be re-pinned until the nested tags exist
   (`gh workflow run release-check.yml -f examples=false`); it is what actually builds
   each nested module against its newly pinned core release with
   `GOWORK=off`, which `go work`-based CI cannot exercise.
4. **Tag the nested modules**, now that both are green for that commit.
   Push the tags one at a time: GitHub creates no push event when more
   than three tags arrive in one push, so the release check would not run
   for any of them.
5. **Re-pin the examples** (`examples/live`, `examples/mcp`) and `proxy` to
   the new nested-module tags, tidy, and push. Then run the release check once
   more with its default inputs, which includes the examples, and once it is
   green for that commit tag `proxy`. Until `proxy` first builds against
   released versions, it is not in the release check's module list: the
   first release that carries the `Gate` and `mcp.ReadOwned` adds it there
   in this step (see the note in `.github/workflows/release-check.yml`).
6. **Create a GitHub release for every tag** — the core's and each nested
   module's. The changelog's `## [Unreleased]` section becomes that
   release's dated section in `CHANGELOG.md`, and the GitHub release body
   is that section's content; `## [Unreleased]` is then empty again for
   the next cycle.

A pushed tag is never moved, force-pushed, or re-pointed at a different
commit, on the core or on a nested module: a consumer's `go.sum` pins the
content a tag names, and moving it after the fact breaks that promise
silently. A mistake in a tagged commit is fixed by tagging a new patch
version, not by rewriting the old tag.

A release that adds a schema migration changes what `OpenStore` writes to a
database on first open. Before such a release's database is opened by
anything, stop the operator binary (`agentrt`) and every consumer process
that has that database open, on every host that shares it — old and new
schema versions are not read together, and `OpenExisting` refuses a mismatch
rather than guessing, but only after something has tried to open the file.
Restart them on the new build once the migration has run once.

Migration 6, which adds the events' hash chain, is also a reason to stop
every older process first: one still writing after it appends events with no
hash, and the first breaks the chain for `agentrt verify`.
`TestStore_ReleasedMigrationsAreUnchanged` pins migration 6 on its own while
it is unreleased; when v0.5.0 is tagged, fold it into the pin for the
released migrations, as migration 5 was, so that no released migration is
edited.

## Style

Read a neighbouring file first; these are the rules it follows.

- Doc comments are terse and say why, not what the code already says. Every
  exported symbol has one.
- Every exported symbol is justified by a consumer requirement. If you cannot
  name the consumer, keep it unexported. The `Gate` and what came with it are
  the one recorded exception.
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
