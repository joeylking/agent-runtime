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
  a run cannot act without the event that says it did.
- tools reached over MCP are classified by an operator, never by the server or
  the model ([ADR 4](docs/decisions/0004-operator-classified-tools.md)). Each
  tool's name, description, and input schema are pinned by hash: a description
  is model input, so a server that rewords one between the review and the run
  has rewritten the instructions the model reads, and that tool is refused
  until the manifest is reviewed again. The server's own annotations may refuse
  a registration and never relax one. A tool with no operator rule is
  unavailable.

## What it does not defend against

- **The database.** It is trusted as the operator's filesystem is. The hashes
  catch corruption and code paths that mutate a record after it was fixed, not
  someone who can write to the file. Anyone who can write to `runs.db` can
  approve anything; protect it with file permissions.
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
  reached over a pipe or a socket. Pinning detects a changed tool surface; it
  does not make the server trustworthy, and its results are untrusted model
  input.
- **Secrets.** Provider keys come from the consumer's configuration and the
  runtime neither stores nor redacts them. Decisions, arguments, observations,
  and approval presentations are persisted verbatim, so a tool that returns a
  secret puts it in the database and the audit log.
- **Multi-tenancy.** One database belongs to one operator. There are no users,
  no roles, and no authentication; `decided_by` is a string the caller supplies.
