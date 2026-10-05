# agent-runtime

[![ci](https://github.com/joeylking/agent-runtime/actions/workflows/ci.yml/badge.svg)](https://github.com/joeylking/agent-runtime/actions/workflows/ci.yml)

agent-runtime is a Go library for running AI agents that take real actions.
The rules about what an agent may do are enforced by ordinary code, outside
the AI model, where the model cannot change them.

This page starts with the problem and the approach in plain language, for
any reader. The commands and the API follow, for people who want to build
on it.

## The problem

An AI language model produces text. An agent is a program that lets a model
do more than that. The program gives the model a list of actions it may
request, called tools, such as reading a file, running a command, or
sending a message. The model asks for one, the program carries it out and
shows the model the result, and this repeats until the task is done.

That loop is short to write, and every AI vendor's software kit includes
one. The hard part begins when the actions are real:

- A model's choice is a prediction. It is usually sensible and sometimes
  wrong, and the same input can produce a different choice the next time.
- A model cannot reliably tell instructions from content. Text inside a
  file or a web page it reads can redirect it. This is called prompt
  injection.
- A model can repeat itself, run for hours, and spend money on every
  request.
- Afterwards someone has to be able to say what the agent did and who
  allowed it.

The common defence is to write the rules into the model's instructions:
never delete files, ask before sending anything. That is a request. The
model follows it most of the time, and nothing stops it the rest of the
time.

## The approach

agent-runtime moves the rules out of the model's instructions and into
code. The model asks and the runtime decides. The model cannot change the
rules. It is told only the outcome of a request and the reason the policy
gives, and the agent has no access to the code that applies the rules.

For every action the model requests, the runtime does the same things in
the same order:

1. **Record the request** exactly as the model made it.
2. **Check its form** against the tool's declared inputs. A malformed
   request never runs. The model is told what was wrong.
3. **Ask the policy**, which is code supplied by the person building the
   agent. The answer is one of four: allow it, deny it, end the run, or
   pause until a person approves.
4. **Run it** if it was allowed, under a time limit, and record the result.

Around that sequence the runtime provides the controls that a real
deployment needs:

| Control | What it means |
|---|---|
| Approvals tied to the exact request | When a run pauses for approval, the person is shown one specific request, and the approval is valid for that request only. It is stored with a fingerprint, called a hash, of the request. If anything differs when the run continues, the approval does not apply. The operator's decision is tied to the same fingerprint, so what was displayed is what gets approved. |
| Approvals that outlast the process | A paused run is a row in a database. The program can exit, the person can decide the next day, and the run continues from where it stopped. |
| Limits checked before the fact | Steps, failures in a row, identical requests that keep giving the same result, elapsed time, model calls, tokens, and spend can each be capped. A cap is checked before the next action or request starts, and reaching one ends the run with that reason recorded. |
| A complete record | Every step, decision, and result is written to a database in the same operation as the change it describes, so the record cannot fall out of step with what happened. |
| No action repeats on its own | If the program dies in the middle of an action, the runtime records the step as interrupted when the run is resumed. An action that changes something is not run again unless the consumer's own check of the outside world says so or an operator approves it, and the approval states that the first attempt may have taken effect. |
| One process per run | A run in progress is leased to the process running it. Another process that tries to continue it is refused until the first finishes or its lease expires. |
| A database only its owner can use | The database file is created so that only its owner can read or write it. One that other users can write is refused, because anyone who can write it could forge an approval. These checks are skipped on Windows. |
| Repeatable tests | The whole loop runs against a scripted agent or recorded model replies, so tests give the same result every time and never call a paid service. |

These controls do not depend on the runtime running the loop. A program that
already has its own loop, from another agent kit or written by hand, can
hand each action it wants to take to the runtime, which checks it, asks the
policy, pauses for approval when needed, runs it, and records it, in the
same way. The program decides what to try next, and the runtime decides
whether it happens.

## An example

The demo in `examples/mcp` gives a local model access to a folder of files.
The operator's rules file says which tools are reads and which is a write.
The example's policy allows reads and sends every write to an operator.

The model reads what it needs, and each read is checked, run, and recorded.
When it asks to write a file, the run stops. The operator sees the file
path, the number of lines, and a preview of the content, and approves with
one command. The run continues and the write happens. The record then shows
the reads that were allowed, the write that waited, who approved it, and
the write that ran after the approval.

## Who uses it

- [repo-steward](https://github.com/joeylking/repo-steward) upgrades a
  dependency of a Go project, repairs what the upgrade broke, and prepares
  a pull request that a person approves.
- casework, which is private, investigates a customer support case about
  access to a paid feature and proposes one fix that an operator approves.

Both supplied the requirements. A feature is added here when one of them
shows the need for it, and not before.

## What it is not

agent-runtime runs one agent at a time and is a library, not a service. It
does not coordinate several agents, schedule work, manage prompts, or give
a model memory. The loop is not the point either, since any vendor's kit
has one. The library includes one, and its controls also work for a loop you
already have. It exists for the controls around each action.

## Terms used on this page

| Term | Meaning |
|---|---|
| Model | The AI language model that chooses the next action. |
| Agent | The program that asks the model for a decision at each step. |
| Tool | One action the agent can request. Each tool declares the inputs it accepts and the kind of effect it has: read only, a local change, a remote change, or destructive. |
| Policy | The rules, written as code, that decide whether a requested action runs. |
| Gate | The part of the runtime that takes each action a program's own loop proposes, applies the controls, runs the action, and records it. |
| Run | One execution of an agent toward a goal, from start to a final state. |
| Step | One decision by the agent and what came of it. |
| Approval | A person's decision to allow one specific request. |
| Operator | The person who runs the agent and grants or refuses approvals. |
| Consumer | A program built on this library, which supplies the agent, the tools, and the policy. |

## Status

The current release is v0.4.0. Both consumers still run on v0.3.x, and v0.4.0
only adds to it: an upgrade needs nothing beyond re-pinning. It carries the
gate, which lets a program with its own loop use the runtime's controls, the
exported `agentrt.CanonicalJSON`, and a security fix for numbers that crashed
schema validation, present in every release through v0.3.2. The nested module
`proxy`, a local MCP proxy built on the gate, is tagged at v0.1.0 as an
experiment. The runtime includes the packages that both consumers had
written separately, an adapter that puts the tools of any MCP server behind
the policy, and the groundwork for outside contributors. These are items 1,
2, and 8 of the [roadmap](docs/roadmap.md). Items 3 to 6, the evaluation
vocabulary, the test kit, event export, and the approval channel, shipped in
v0.3.1, and both consumers run on them. Item 7, writing up the ideas,
is ongoing.

v0.3.0 followed an audit and a security review of v0.2.1. It added the lease
on a run, the database only its owner can use, the pause after an
interrupted action, the approval tied to what was displayed, and the
adapters' refusal of redirects and of keys over plain http.
[CHANGELOG.md](CHANGELOG.md) begins with what an upgrade from v0.2 requires.
The three that matter most:

- Stop every older process and every older `agentrt` binary that has the
  database open before the new version first opens it.
- Run `chmod 600` on a database that group or others can write. The new
  version refuses it until then.
- A consumer that sets no `Config.Reconcile` will see a run pause for an
  operator when an action was interrupted by a crash.

The provider adapters and the MCP adapter are separate modules inside this
repository, tagged with a directory prefix: `providers/ollama/v0.2.0`,
`providers/anthropic/v0.2.0`, `providers/openai/v0.2.0`, `mcp/v0.2.1`,
`export/otel/v0.1.1`, and `proxy/v0.1.0`. On `main` each requires core
v0.4.0; the providers' tags still pin v0.3.0, which v0.4.0 only adds to.
`bench/v0.1.1` has no requirements at all.

The API is pre-1.0 and changes when a consumer needs it to.

Where to read more:

- [docs/architecture.md](docs/architecture.md): the loop, approvals,
  interruption, and persistence.
- [docs/status.md](docs/status.md): every control with the test that
  verifies it.
- [docs/decisions](docs/decisions): why it is built this way.
- [docs/essays](docs/essays): the ideas written out for someone deciding
  whether to trust an agent with a real system, each claim linked to the
  test behind it.
- [CHANGELOG.md](CHANGELOG.md): what each release changed and what a
  consumer has to do about it.
- [Wiki](https://github.com/joeylking/agent-runtime/wiki): the same material
  at length, for people using, extending, or evaluating the runtime.

### Compatibility

Pre-1.0, so the promise is narrow and written down rather than implied:

- within a minor version of the core, the exported API is additive only: a
  patch release adds symbols and fields and takes none away;
- a minor bump may change or remove exported API, and the release notes say
  which symbols and what to do instead;
- the nested modules version independently, with their own directory-prefixed
  tags, and each names the core version it requires in its `go.mod`, so
  upgrading one does not move the other nested modules, though it raises the
  core to the version it requires;
- the SQLite schema only migrates forward. A newer core opens a database
  written by an older one and applies what is missing; there are no down
  migrations, so a database is not handed back to an older core. Processes
  and operator binaries of an older version are stopped before a newer core
  first opens a database. The operator command never migrates: it refuses a
  database whose schema is not its own build's and says which side is newer;
- the replay key is the model name and the canonical JSON of the request
  (`replay.Key`), and its shape has not changed. A recording made through
  `render` stops matching when a release changes what `render` produces for
  a step in it, and the release notes say when that happens. v0.3.0 does
  this for a reply with more than one tool use, for a reply
  with no tool use that is longer than 500 bytes, and for a run with a step
  interrupted while deciding inside the recent-results window: the window
  now counts rendered steps, where v0.2.1 counted raw steps, so such a run
  renders differently.

[CONTRIBUTING.md](CONTRIBUTING.md) is how a change gets in, and
[SECURITY.md](SECURITY.md) is what the runtime does and does not defend against.

## Quick start

Every module requires Go 1.26 or later, so the current release and the one
before it both build them, and CI runs both.

The demo the project leads with is `examples/mcp`: a local model driving the
MCP filesystem server, where reads are allowed and a write stops the run
until an operator approves that exact call. It needs Node, to run the server
pinned by version and integrity hash in `examples/mcp/package-lock.json`, an
[Ollama](https://ollama.com) server holding `qwen3:30b-a3b`, and a workspace,
because the example and the MCP adapter are nested modules:

```sh
go work init . ./providers/ollama ./providers/anthropic ./providers/openai ./mcp ./bench ./export/otel ./proxy ./examples/live ./examples/mcp
(cd examples/mcp && npm ci --ignore-scripts)   # once: the pinned server, no install scripts
go run ./examples/mcp pin      # hash the server's tools, print the hints it claims
go run ./examples/mcp run      # load the operator's rules, run to the first write
go run ./cmd/agentrt -db <path> approve <run>   # prints the request, then asks
go run ./examples/mcp run -resume <run>
go run ./cmd/agentrt -db <path> events <run>
```

`approve` prints the waiting request and asks for confirmation when it is run
at a terminal. In a script it needs `-approval <id>` or `-yes`.

`pin` prints one line per tool with the annotations the server asserts, which
classify nothing and are only there for the operator to disagree with.
[`examples/mcp/rules.json`](examples/mcp/rules.json) is the operator's answer:
four read tools, `write_file` as a local mutation with the server's own
destructive hint overruled in writing, and every other tool the filesystem
server offers left out, so `run` reports them unclassified and the model never
sees them. The run then pauses at `write_file` with the path, the line count,
and a preview of the content, and prints the two commands above with the run id
and the database filled in. The trail shows the reads that were allowed, the
write that waited, the operator who granted it, the write that executed after
the grant, and the `finish` call that completed the run.

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

A consumer opens a store, which is one SQLite file, builds a driver from
its agent, policy, and tools, and starts a run toward a goal.

`DefaultPolicy` decides by the kind of effect a tool declares. Tools that
only read and tools that change local state are allowed. A tool that
changes something remote pauses for approval. A destructive tool is denied,
and so is a tool whose kind the policy does not know.

`DefaultLimits` ends a run at 50 steps, at 3 failures in a row, or when the
same request has produced the same result 3 times in a row. The caps on
model calls, tokens, spend, and time are off until the consumer sets them.

A spend cap only works for a model in the price table. A model missing from
the table is counted as free. Model calls are not retried unless
`ModelConfig.MaxRetries` is set.

## Using the gate from your own loop

A program with its own loop gives the runtime the tools and the policy and
hands it each decision. A `Gate` takes the place of the driver: it records,
checks, and authorizes each proposed action, pauses for an approval when
policy asks, and runs the tool itself, so an action runs only when the
policy allows it, once per approval, however the loop is written. The gate
is in v0.4.0.
[ADR 7](docs/decisions/0007-own-the-effect-not-the-loop.md) explains why it
exists and what it does not cover, and the [architecture](docs/architecture.md)
describes the session and what an abandoned one leaves behind.

```go
gate, _ := agentrt.NewGate(agentrt.GateConfig{
    Store: store, Policy: agentrt.DefaultPolicy(), Tools: tools,
})
s, _ := gate.Begin(ctx, "run-1", "goal", agentrt.DefaultLimits())
defer s.Close()
for !s.Done() {
    step, _ := s.Step(ctx)           // nil when a limit has ended the run
    if step == nil {
        break
    }
    v, _ := step.Propose(ctx, yourLoopDecides(s)) // an agentrt.Decision
    if v.Outcome == agentrt.VerdictAllowed {
        step.Execute(ctx)            // runs the allowed request, once
    }
    // VerdictPending: the run waits on an approval, and the session is over
}

// After an operator approves, in this process or another:
s, v, _ := gate.Attach(ctx, "run-1")
if v.Outcome == agentrt.VerdictAllowed {
    s.Current().Execute(ctx)         // exactly the request that was approved
}
```

`Propose` answers with a verdict: allowed, denied, pending, or ended. A
denial is an observation and the loop continues. `Session.Input` returns
what an agent would be handed, for a loop that decides from the run's
record. `testkit.Scenario.Loop` runs the crash and resume harness over a
loop like this one. `ExampleGate` in `example_test.go` is the complete,
tested version.

## The gate in front of an MCP host

A program that is not written in Go can still have its actions go through
the gate, if it is an MCP host: an application that runs a model and gives it
tools from MCP servers. `agentrt-proxy` is a small local program the host
starts in place of those servers. It offers the host only the tools the
operator classified and pinned, and every call the model makes passes the
gate: it is recorded, checked, held to the policy, and executed only when the
policy allows it or an operator approved that exact call. A call waiting for
approval tells the model so in plain words, and the operator decides with
`agentrt`, outside the host. Each call is its own run.

It governs only the calls that go through it. A host that also gives the
model a shell or file access lets the model go around it, and even approve
its own request. [docs/proxy.md](docs/proxy.md) says what it does and does not
do, and `go run ./proxy/example` shows it with no model at all. It is an
experiment, released as `proxy/v0.1.0`; the Go library is the full form.

## What a consumer no longer has to write

Both existing consumers rebuilt the same provider plumbing, renderer, trace,
and operator surface. These packages are in the core module and add no
dependency beyond the standard library:

- `providers`: transport and HTTP classification for a provider adapter (a
  timeout or a reply cut off mid-read returned bare, retryable statuses
  transient with the provider's `Retry-After`), an HTTP client that follows
  no redirect, a bounded body read, a check that usage numbers are counts,
  tool-use id synthesis, a price lookup that refuses an unpriced paid model,
  and dollar parsing for budgets.
- `render`: a run's recorded steps to model messages, with synthesized
  tool-use ids, a recent-results window, a content cap, and hooks for the
  opening message, the assistant turn, and the observation; plus
  `render.Decide`, which maps a response to a decision and turns a truncated
  or tool-less reply into a deliberately invalid decision the runtime records
  and the next render nudges.
- `trace`: aligned and JSON Lines observers, and `Sanitize`, which escapes
  text that could alter what a terminal displays.
- `view`: run, step, and approval read models, read in bounded pages, and the
  rule for choosing the approval an operator means.

```sh
go run ./cmd/agentrt -db runs.db runs
go run ./cmd/agentrt -db runs.db show <run>       # steps and the waiting approval
go run ./cmd/agentrt -db runs.db events <run>     # the audit log
go run ./cmd/agentrt -db runs.db -json events <run>   # the same as JSON Lines
go run ./cmd/agentrt -db runs.db approve <run> -approval <id> -by joey -note "ok"
go run ./cmd/agentrt -db runs.db reject <run> -approval <id> -note "not this file"
go run ./cmd/agentrt -db runs.db cancel <run>
```

`cmd/agentrt` reads `-db` or `AGENTRT_DB`, prints aligned text or JSON, and
exits 0, 1, or 2. `-db` and `-json` go before the subcommand. `approve`,
`reject`, and `cancel` take their own flags before or after the run id.
`show` and `events` take them after the run id.

- It opens an existing database only. It never creates one and never
  migrates one.
- `runs`, `show`, and `events` open the database read-only and page their
  output with `-limit` and `-offset`.
- In text output, `approve` and `reject` print the waiting request first.
  They need
  `-approval <id>`, `-yes`, or a yes typed at a terminal, and the decision
  applies only if the stored request is still the one that was printed.
- `-by` is a label recorded as given, not a login. Whoever can write the
  database can approve.
- Text output escapes control characters and characters that change how text
  is displayed, because goals, tool names, and results come from the model
  and from servers.
- It has no `resume`: resuming executes the approved request and continues
  the loop, which needs the consumer's agent, tools, and policy.

A run ends `COMPLETED`, `FAILED` with a terminal reason, or `CANCELLED` when
an approval is rejected or expires or an operator cancels the run. It pauses
in `WAITING_FOR_APPROVAL` on a hash-bound approval that `Approve` and `Resume`
continue. A run in progress is leased to its driver, and `Resume` from another
process is refused with `ErrRunLeased` until the lease expires. A model configured on the driver is wrapped in an accounting caller
that agents receive in `StepInput`: it records every attempt, enforces call,
token, and cost limits before dispatch, and retries transient failures up to
`ModelConfig.MaxRetries` times. The
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
model id, `New(Config) (*Model, error)`, no streaming, no retries of its own
because retrying is the accounting caller's job and it records every attempt, the provider's raw bytes
on every response, and a stop reason normalized to `tool_use`, `end_turn`,
`max_tokens`, or `refusal` where the provider reports one of those, with any
other value passed through as the provider gave it. A reply that was cut off or refused comes back
with no tool uses, so a partial call can never execute, and with its usage,
so the attempt is still charged.

No adapter follows a redirect, because a redirect would carry the key to a
host the consumer did not choose. A key travels over plain http only to a
loopback address.

- The Anthropic adapter reads `ANTHROPIC_API_KEY` and nothing else from the
  environment. It refuses to build without a key. It also refuses a model
  its price table does not list, unless `Config.Name` is set and the
  consumer prices that name.
- The OpenAI adapter reads `OPENAI_API_KEY` only for the default endpoint.
- The Ollama adapter reads `OLLAMA_HOST` when no host is configured, and
  nothing else.

Usage that is not a plausible count is refused. The attempt is charged at
the conservative estimate, the reply is not used, and the run ends as
`model_unavailable`. Printing or encoding a `Config` or a `Model` redacts the
key, except a `Config` kept in an unexported field of your own struct and
printed with `%v`.

`Name()` is `<provider>:<model>` unless the config overrides it, so one price
table and one recording directory can hold several providers.
`anthropic.Prices()` prices the paid models; `ollama.Free(m)` and
`openai.Free(m)` take the built adapters and record their models as free,
which is what
`providers.PriceFor` needs to tell a free model from an unpriced one.

No test calls a provider: the Anthropic adapter is exercised against an
`httptest` fake, and each local adapter has one round trip behind the `live`
build tag that skips unless a server answers. Working on the nested modules
needs a workspace, which is what CI builds:

```sh
go work init . ./providers/ollama ./providers/anthropic ./providers/openai ./mcp ./bench ./export/otel ./proxy ./examples/live ./examples/mcp
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
  the model sees, and a call that supplies one fails;
- a schema that refers to anything outside itself is refused, so a server
  cannot make the runtime read a file or fetch a URL;
- a server started as a child process gets a minimal environment plus the
  variables the operator names. On Unix it runs in its own process group
  and is stopped with its descendants on close. It still runs as the
  operator and can read the operator's files;
- connecting, initializing, and listing tools are bounded by
  `ConnectTimeout`, and an HTTP server's redirects are not followed;
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
thing running: the quick start above is its commands.

## Measuring, testing, watching, and approving from elsewhere

Four additions sit around the loop rather than in it. They shipped in
v0.3.1.

- **Measuring an agent.** `bench` is the vocabulary for scoring an agent that
  acts: each trial lands in exactly one outcome, every count is shown with
  what it was counted out of, and one unsafe outcome disqualifies the run's
  mode. Results are only compared within one commit. The scenarios and the
  judging stay yours. It is a nested module with no dependencies.
- **Testing without a model.** `testkit` helps a consumer prove what the
  runtime promises about its own tools and policy: it crashes the loop at a
  chosen point and resumes it in a fresh driver, checks a table of policy
  decisions, fuzzes tool arguments against their schema, and checks that an
  agent renders the same model request every time. It is a core package.
- **Watching runs in a tool you already have.** `export` follows a database's
  events from a separate process, read-only, and `export/otel` turns them
  into OpenTelemetry spans for whatever collector the `OTEL_*` environment
  names. The consumer needs no code. `export` is a core package; `export/otel`
  is a nested module because it brings OpenTelemetry.
- **Approving from a chat bot or a browser.** `approver` is the interface an
  operator surface decides approvals through, and `approver/webhook` is a
  signed HTTP channel over it, so an approval can be granted without access
  to the database file. The decision is still bound to the request that was
  shown. Both are core packages.

```sh
go get github.com/joeylking/agent-runtime/bench
go get github.com/joeylking/agent-runtime/export/otel
```

Both resolve to the released tags. In a workspace of this repository they
resolve to the tree.

```go
// measuring: read result files, print the comparison, refusing mixed commits
files, _ := bench.ReadDir("results"); out, err := bench.Compare(bench.Latest(files), bench.CompareOptions{})

// testing: crash after the tool's effect, resume, and assert the contract held
res := testkit.Run(t, testkit.Scenario{Tools: tools, Policy: policy, Agent: agent}, testkit.Crash{Step: 0, At: testkit.AfterToolEffect})

// watching: follow the database from another process and send spans to the collector
//   OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 go run ./export/otel/example -db runs.db

// approving: serve the signed channel over the database; the secret is at least 32 bytes
h, _ := webhook.New(webhook.Config{Approver: approver.New(store, nil), Secret: secret}); http.ListenAndServe(addr, h)
```

The package docs say the rest:
[`bench`](bench/bench.go), [`testkit`](testkit/testkit.go),
[`export`](export/export.go), [`export/otel`](export/otel/otel.go),
[`approver`](approver/approver.go), and
[`approver/webhook`](approver/webhook/webhook.go), which lists the routes, the
signature, and what the channel does not do. `examples/approver` runs a
scripted run to an approval and decides it through the webhook, with no model
and no network beyond loopback:

```sh
go run ./examples/approver
```

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
