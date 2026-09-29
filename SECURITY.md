# Security

## Reporting a vulnerability

Report privately, through GitHub's private vulnerability reporting on this
repository: the **Security** tab, then **Report a vulnerability**
([direct link](https://github.com/joeylking/agent-runtime/security/advisories/new)).
That opens a draft advisory only the maintainer can read.

If that form is not available to you, open an ordinary issue saying that you
have a report to make and asking for a private channel, with no details in it.
Do not put a vulnerability in a public issue, a pull request, or a discussion.

Expect an acknowledgement within a week. A fix ships as a patch release of the
affected module with an advisory that names the versions.

## Supported versions

Pre-1.0, so only the current release line is fixed:

| Module | Supported |
|---|---|
| `github.com/joeylking/agent-runtime` | the latest minor |
| `github.com/joeylking/agent-runtime/providers/ollama` | the latest tag |
| `github.com/joeylking/agent-runtime/providers/anthropic` | the latest tag |
| `github.com/joeylking/agent-runtime/providers/openai` | the latest tag |
| `github.com/joeylking/agent-runtime/mcp` | the latest tag |

Older minors and older nested-module tags get no fixes. A nested module names
the core version it requires, so a core fix may need that module re-tagged too.

## What the runtime defends against

The premise, from [ADR 3](docs/decisions/0003-policy-outside-the-model.md), is
that repository content, dependency sources, tool output, and an MCP server's
tool list are all untrusted input that reaches the model. The model is expected
to be wrong or to be talked into something. What the runtime guarantees is that
whatever it concludes, the set of actions it can take is bounded by code it
cannot influence:

- every tool request is validated against the tool's JSON schema and then
  evaluated by a policy the runtime calls. The agent receives outcomes as
  observations and holds no handle to the policy or the store. A side-effect
  class with no mapping is denied, so policy fails closed.
- an approval is bound by a hash over its kind, capability, presentation, and
  the exact tool request. The hash is recomputed before the decision is
  accepted and again on resume, policy is re-evaluated on resume, and the
  recorded request is what executes — nothing else.
- call, token, cost, and time limits are enforced by the driver before
  dispatch, and an attempt whose outcome is unknown is charged at the
  conservative estimate.
- the audit log is written in the same transaction as the state it explains, so
  a run cannot act without the event that says it did. Run and step status
  transitions are compare-and-set inside that same transaction, so two
  processes racing to resume one approved run execute its side effect once,
  and an operator cancel is never silently overwritten by a loop that has not
  yet noticed it.
- `cmd/agentrt` sanitises every field it prints that may have originated from
  the model or a remote server — including C0/C1 control characters — before
  it reaches the terminal, so a tool result or a model reply cannot clear the
  screen, move the cursor, or forge the look of an approval prompt
  (`trace.Sanitize`). This applies to its aligned-text output; its JSON output
  is passed through unescaped beyond what `encoding/json` already does, on
  the reasoning that a consumer parsing JSON is not a terminal.
- `approve` and `reject` require explicit consent before deciding anything:
  `-approval <id>` naming the approval, `-yes`, or a `y` typed at an
  interactive prompt. Both print the approval being decided first. Run them
  non-interactively with none of those given and they refuse rather than
  assume consent. `cmd/agentrt` also never creates or migrates a database —
  `OpenExisting` opens only what is already there — so a mistyped `-db` path
  is an error rather than a silently created, empty database an operator
  might mistake for the real one.
- tools reached over MCP are classified by an operator, never by the server or
  the model ([ADR 4](docs/decisions/0004-operator-classified-tools.md)). Each
  tool's name, description, and input schema are pinned by a hash computed
  from the server's raw `tools/list` bytes, not from a value that has
  round-tripped through `float64` JSON decoding and can lose precision: a
  description is model input, so a server that rewords one between the review
  and the run has rewritten the instructions the model reads, and that tool is
  refused until the manifest is reviewed again. The server's own annotations
  may refuse a registration and never relax one. A tool with no operator rule
  is unavailable.
- an operator's `Deny` and `Fixed` parameter rules are enforced at call time,
  not only when the schema is built: a call that supplies a value for a
  parameter the operator fixed fails, instead of being silently overridden
  with the operator's value behind an approval that looked like it covered
  something else. A `Deny` or `Fixed` name that is misspelled, or that is
  reachable only through a schema branch (`oneOf`/`anyOf`/`allOf`) rather
  than as a top-level property, refuses the tool at load rather than leaving
  that parameter reachable.
- an MCP stdio server is started with a minimal base environment plus only
  the variables the operator names in `Env`/`InheritEnv`, not this process's
  whole environment, so a compromised or misconfigured server does not
  receive credentials meant for something else. Duplicate tool names, runaway
  pagination, and an HTTP response body over the configured bound are all
  refused rather than accepted or hung on.
- the provider adapters resolve their own API key and base URL instead of
  trusting a provider SDK's own environment-variable chain: the OpenAI
  adapter reads `OPENAI_API_KEY` from the environment only when the endpoint
  is the default OpenAI one, and refuses to send any key over `http://` to a
  host that is not loopback; the Anthropic adapter takes nothing from the
  environment but the key, ignores the SDK's own base-URL and auth-token
  variables, and refuses to build without a key or a price for the model.
  This keeps a key from reaching an endpoint the consumer did not configure
  it for.

## What it does not defend against

- **The database.** It is trusted as the operator's filesystem is. The hashes
  catch corruption and code paths that mutate a record after it was fixed, not
  someone who can write to the file. Anyone who can write to `runs.db` can
  approve anything; protect it with file permissions. The runtime does not
  set those permissions itself: `OpenStore` and `OpenExisting` create or open
  the file without changing its mode, so whatever the process's umask leaves
  it at is what it gets. The bundled examples show one way to do better —
  `examples/live` and `examples/mcp` write their database in a per-user
  directory created `0700` and `chmod` the file itself `0600` — but that is
  the example's own choice, not something the runtime enforces on a
  consumer's behalf.
- **The process.** The consumer's agent, tools, policy, and the runtime share
  one address space. There is no sandbox: a tool can do whatever the process
  can, and a tool's side-effect class is a claim the operator makes about it,
  not something the runtime verifies.
- **Prompt injection as such.** Untrusted content will reach the model and will
  sometimes steer it. The runtime's answer is the bound on what a steered model
  can do, not detection. If a policy allows a class of action, an injected
  instruction can use it.
- **What a tool does with its arguments.** Schema validation says the arguments
  fit the schema. Path traversal, command construction, and authorization
  inside a tool are the tool's own responsibility, and for an MCP tool they are
  the server's.
- **MCP servers.** A pinned server is still code this project did not write,
  reached over a pipe or a socket. Pinning covers the tool's name,
  description, and input schema; it does not cover what the server actually
  does when called, and its results are untrusted model input. The minimal
  environment a stdio server is started with bounds what it receives at
  connect time, not what it can otherwise read or do as a subprocess on the
  operator's machine, and a streamable-HTTP server is reached over whatever
  network access the process already has.
- **Secrets.** Provider keys come from the consumer's configuration.
  `Config` and `Model` redact the key in their own `String()` output, and an
  Anthropic SDK error no longer carries the outbound request's key header,
  but that is where the runtime's handling of the key itself stops: decisions,
  arguments, observations, and approval presentations are still persisted
  verbatim, so a tool that returns a secret puts it in the database and the
  audit log exactly as before. The runtime does not scan for or redact
  secrets in that data.
- **Multi-tenancy.** One database belongs to one operator. There are no users,
  no roles, and no authentication; `decided_by` is a string the caller supplies.
