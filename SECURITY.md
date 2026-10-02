# Security

## Reporting a vulnerability

GitHub's private vulnerability reporting is **not enabled** on this
repository: `gh api repos/joeylking/agent-runtime/private-vulnerability-reporting`
returned `"enabled": false` on 2026-09-29. Until it is turned on, use the
fallback below rather than the **Security** tab.

Open an ordinary issue saying you have a report to make and asking for a
private channel. Put no details in the issue itself, not even in the title;
the maintainer will follow up with somewhere private to send the report. Do
not put a vulnerability in a public issue, a pull request, or a discussion.

Once private vulnerability reporting is enabled for this repository, prefer
it instead: the **Security** tab, then **Report a vulnerability** ([direct
link](https://github.com/joeylking/agent-runtime/security/advisories/new)).
That opens a draft advisory only the maintainer can read.

Expect an acknowledgement within a week. A fix ships as a patch release of
the affected module with an advisory that names the versions.

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
cannot influence.

### Policy and approvals

- Every tool request is validated against the tool's JSON schema and then
  evaluated by a policy the runtime calls. The agent receives outcomes as
  observations and holds no handle to the policy or the store. The
  built-in `SideEffectPolicy` denies a side-effect class it has no mapping
  for, so it fails closed.
- An approval is bound by a hash over its kind, capability, presentation,
  and the exact tool request. The hash is recomputed before a decision is
  accepted and again on resume; policy is re-evaluated on resume, the
  recorded arguments are checked against the tool's schema as registered
  then, and the recorded request is what executes. Only a step's latest
  approval can be a grant.
- `Limits.ApprovalTTL` bounds how long an approval may stay pending.
  `Limits.GrantTTL`, zero by default, bounds how long an approved grant
  may wait for `Resume`, measured from its decision; a grant older than
  that is expired at `Resume`, the run is cancelled as `approval_expired`,
  and nothing executes.
- `cmd/agentrt approve` and `reject` bind the decision to the hash of the
  approval they read and printed: they pass it to `agentrt.ApproveShown`
  or `RejectShown`, which compare it with the stored hash inside the
  transaction that records the decision, with the update conditional on
  it, and refuse with `agentrt.ErrApprovalChanged` if they differ. What an
  operator saw is what they decide, even against a concurrent writer.
  `-approval <id>`, `-yes`, or `y` typed at an interactive prompt is
  required before anything is decided; run non-interactively with none of
  those, the command refuses rather than prompt. Stdin is a terminal only
  when it is a character device that is not `/dev/null`, by identity
  (`os.SameFile`) rather than by path, so redirecting it from `/dev/null`
  is treated as no terminal, not as a prompt that reads nothing and
  declines; reaching the end of input before anything was typed at a real
  terminal refuses the same way, rather than being read as a typed "n".
  `-by`/`decided_by` is a label recorded exactly as given, never verified.

### Limits and accounting

- Call, token, cost, and time limits are enforced by the driver before
  dispatch. An attempt whose outcome is unknown (a timeout, a lost
  connection, a body cut off mid-read) is charged the conservative
  estimate: the request's estimated input tokens and its output cap.
- Usage a provider reports that cannot be charged is not believed. A
  negative count or more cached input than input, found by the runtime
  (`Usage.usable` in `model.go`), and a count an adapter could not read
  because it was negative, too large to hold, or not an integer, which
  the adapter reports as a `ServedError` wrapping `ErrUnusableUsage`, are
  both charged the conservative estimate instead; the reply is not used,
  the attempt is recorded as an error naming why, it is not retried, and
  the run fails as `model_unavailable`. Run totals and costs are added
  with saturating arithmetic, so a huge count fills them to the maximum
  rather than wrapping negative, and the next limit check refuses the run.

### Consumer JSON

- JSON a consumer hands the runtime (decision arguments and results, tool
  content, approval capabilities and presentations, a reconciliation's
  result) and every tool's input schema are refused when nested deeper
  than 256 levels (`maxJSONDepth` in `hash.go`), before anything recurses
  over them, as are malformed JSON, invalid UTF-8, an unpaired escaped
  surrogate, and a repeated key. What is refused is recorded as an
  invalid decision or a tool error, or fails the run as `internal_error`;
  nothing that is stored or hashed is replaced by a stand-in.
- A tool's input schema is compiled from itself and the standard
  metaschemas only: a `$ref`, `$schema`, or `$id` naming a file or a URL
  fails registration (`schema.go`), and nothing is read or fetched.

### The audit log and concurrent processes

- The audit log is written in the same transaction as the state it
  explains, so a run cannot act without the event that says it did. Run
  and step status transitions are compare-and-set inside that
  transaction: two processes racing to resume one approved run execute
  its side effect once, and an operator's cancel is never overwritten by
  a loop that has not yet noticed it.
- A RUNNING run is leased ([ADR 5](docs/decisions/0005-run-lease.md)).
  The stored owner is `Config.LeaseOwner` followed by a random suffix
  drawn per `Driver`, so two drivers never share an owner even when
  configured with the same name, and a restarted process waits out its
  previous life's lease. On a file-backed store the lease is renewed on a
  connection of its own, so a consumer holding `DB()` cannot starve the
  renewal. The writes that start a side effect, a tool start and a model
  request's dispatch, require the lease unexpired by the wall clock, so a
  loop stalled past its expiry starts nothing even if nobody has taken
  the run. When migration 5 adds the lease to an older database, each
  RUNNING run is given a lease of `DefaultLeaseTTL` (30 seconds) under a
  placeholder owner, so a process of the older version still inside a
  call has that long before a current process may take the run over.
- A side effect interrupted mid-flight is not run again automatically
  ([ADR 6](docs/decisions/0006-interrupted-side-effects.md)). When
  `Resume` takes over a run and the consumer has no `Config.Reconcile`,
  a step that was executing a tool that is not `ReadOnly` pauses the run
  on an approval of kind `interrupted_side_effect`
  (`agentrt.InterruptedSideEffect`, `resume.go`), whose presentation
  says the call started and its outcome is unknown. Approving and
  resuming runs it again, once, on the same step, after the usual schema
  check and policy re-evaluation; rejecting ends the run. A step whose
  tool is no longer registered, or whose arguments no longer pass the
  tool's schema, fails the run as `reconcile_conflict` instead. A step
  that was executing a `ReadOnly` tool, or deciding, continues. With
  `Config.Reconcile`, the consumer's reconciliation decides.

### The database file

- A new database is created readable and writable by its owner only
  (`0600`), and SQLite gives its WAL files the same mode. Both openers
  refuse with `agentrt.ErrInsecureMode`, naming the file, its mode, and
  the reason, a database or WAL file that group or others can write, a
  WAL file owned by another user than the database, and a WAL path that
  is not a regular file. A database or WAL file others can only read is
  made owner-only by a writer (`OpenStore`, or `OpenExisting` for
  writing) that owns it; a read-only open changes nothing. These rules
  are skipped where file modes do not mean owner, group, and others, as
  on Windows.
- `OpenExisting`, which `cmd/agentrt` uses, refuses a symbolic link with
  `agentrt.ErrSymlink` rather than following it, and refuses anything
  else that is not a regular file, so what is opened is what the
  operator named. `OpenStore` follows a link, as the consumer's own
  configuration names its database.
- A database holding a trigger or a view is refused by both openers with
  `agentrt.ErrUnsafeSchema`, because either would run SQL of the file's
  author inside the runtime's own statements. Every connection to a
  database file runs with `trusted_schema` off and SQLite's defensive
  mode on. A writer syncs every commit (`synchronous=FULL`), so a
  committed `step.tool_started` survives power loss.
- `cmd/agentrt`'s `runs`, `show`, and `events` read only the page they
  print, through `Store.ListRunsPage`, `GetRunCapped`, `ListStepsPage`,
  `ListApprovalsPage`, `ListEventsPage`, and `PendingApprovalIDsOf`. Each
  reads at most the rows asked for, caps every text column at
  `agentrt.MaxPageText` (64 Ki characters), and takes its "N more" total
  from a `COUNT`, so neither many rows nor one huge row can exhaust the
  operator's memory. `approve`, `reject`, and `cancel` read the one run
  and approval they decide whole, because the decision is bound to the
  approval's hash.

### What the operator sees

- `cmd/agentrt` escapes every field it prints in text mode that may have
  come from the model, a policy, or a remote server (`trace.Sanitize`):
  C0 and C1 control characters, bidirectional overrides and isolates,
  zero-width and other invisible formatting characters, the line and
  paragraph separators, and the Unicode tag block are shown as visible
  escapes, and more than four combining marks on one base character are
  cut with a count of how many were dropped. A tool result or a model
  reply cannot clear the screen, move the cursor, forge the look of an
  approval prompt, or display as other text than it is.
- The approval prompt prints each argument and presentation field on its
  own lines with its byte size, shows any string over 2000 bytes
  (`view.MaxFieldBytes`) by its head and tail, and ends with a line
  `cmd/agentrt` writes itself naming the approval and the tool, so the
  last thing an operator reads before deciding is not text the model or
  the policy chose. `-json` output passes strings through
  `encoding/json` without terminal escaping, but bounds an approval's
  capability, presentation, and arguments the same way
  (`view.BoundApproval`), and an event payload over `MaxPageText` is
  replaced by `{"truncated":true,"length":n,"head":...}`.
- `mcp`'s `Report.String` and the errors of `Pin` and `Load`, and the
  examples' own output, escape text a server or a model chose the same
  way.

### Model providers and MCP servers

- Neither the provider adapters (Anthropic, OpenAI, Ollama) nor the MCP
  streamable-HTTP client follow a redirect unless the consumer supplies
  an `http.Client` with a `CheckRedirect` of its own: a followed redirect
  could hand an API key, a bearer token, or the whole request to whatever
  host it named. A 3xx is returned as a permanent error naming only the
  host it pointed to (`providers.RefuseRedirects`, `mcp`'s default
  `CheckRedirect`).
- The adapters resolve their own key and base URL instead of trusting a
  provider SDK's environment chain. The OpenAI adapter reads
  `OPENAI_API_KEY` only when the endpoint is the default OpenAI one. The
  Anthropic adapter takes nothing from the environment but
  `ANTHROPIC_API_KEY`, ignores the SDK's own base-URL and auth-token
  variables, and refuses to build without a key or, unless
  `Config.Name` is set, without a price for the model. Both refuse to
  send a key over plain `http://` to any host but exactly `localhost` or
  a literal loopback address, and then refuse a connection whose dialled
  address is not loopback (for a client whose transport is an
  `*http.Transport`; a consumer's own `RoundTripper` is left as given). Both print, marshal, and log their `Config`
  and `Model` with the key redacted, and an Anthropic SDK error, which
  holds the outbound request and its key header, is converted before it
  leaves the adapter. Response bodies are read under a 16 MiB bound.
- Tools reached over MCP are classified by an operator, never by the
  server or the model ([ADR 4](docs/decisions/0004-operator-classified-tools.md)).
  Each tool's name, description, and input schema are pinned by a hash
  computed from the server's raw `tools/list` bytes, with keys matched
  exactly as the SDK matches them. A server that rewords a description
  between the review and the run has that tool refused until the
  manifest is reviewed again. The server's annotations may refuse a
  registration and never relax one; a tool with no operator rule is
  unavailable.
- A server-supplied input schema must be self-contained: a `$ref`,
  `$dynamicRef`, or `$recursiveRef` that is not a fragment of the
  document, an `$id` or `$schema` that is not a fragment or a well-known
  draft URI, or, under draft-04, an `id` naming a scheme or host,
  anywhere in the document, is refused by `mcp.Load` before the compiler
  sees it (`selfContained` in `mcp/load.go`); the core's compiler would
  refuse it again at registration. Load's checks, the compile included,
  fall inside `Server.ConnectTimeout`.
- An operator's `Deny` and `Fixed` parameter rules are enforced at call
  time: a call that supplies a value for a fixed parameter fails rather
  than being silently overridden. A `Deny` or `Fixed` name that is
  misspelled, or reachable only through `oneOf`/`anyOf`/`allOf`, refuses
  the tool at load.
- An MCP stdio server does not inherit this process's environment. It
  gets `PATH`, `HOME`, and `TMPDIR` (their Windows equivalents on
  Windows), then each variable named in `Server.InheritEnv`, then
  `Server.Env` as written; `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and
  `MCPGODEBUG` only when named. On Unix it is started in a process group
  of its own, and `Close` kills the whole group after the SDK's shutdown,
  descendants included. On Linux it is also started with `Pdeathsig`
  (`SIGKILL`), so the kernel kills it if this process dies first; its own
  descendants are not killed that way, and on other Unix systems a server
  outlives a crash of this process until it reads the end of its stdin.
  On Windows no group is made and `Close` ends only the server itself.
  Duplicate tool names, runaway pagination, and an HTTP response body
  over 16 MiB are refused rather than accepted or hung on.

### Files the runtime writes besides the database

- `replay.Recorder` creates its directory `0700` and writes each
  recording through a temporary file it created itself in that
  directory, `0600`, synced, then renamed into place, so no one else can
  prepare the name, read a recording, or see one half written.
- `examples/live` and `examples/mcp` keep their database, sandbox, and
  manifest under `os.UserCacheDir()/agentrt`, created `0700`, not under
  a fixed name in the shared temporary directory, and `examples/mcp`
  refuses to read a manifest or rules file that group or others can
  write.

## What it does not defend against

- **The database beyond what is listed above.** The mode checks and the
  trigger and view refusal catch a database another user can write, or
  one crafted to run SQL through the runtime's own connection; they do
  not make the database a trust boundary. The hashes catch corruption
  and code paths that mutate a record after it was fixed, not someone
  with write access to the file: anyone who can write `runs.db`, which
  includes root and the file owner's account, and included the file's
  group before this release refused group-writable files, can approve
  anything in it. The main database file's owner is not checked, only
  its mode. SQLite on a network filesystem is not supported: file
  locking over NFS or SMB is unreliable enough that two processes can
  each believe they hold the write lock. Clock skew between hosts sharing
  a database is not supported either: expiry and lease logic reads the
  local clock, and a host whose clock disagrees can misjudge how old a
  record is.
- **The process.** The consumer's agent, tools, policy, and the runtime share
  one address space. There is no sandbox: a tool can do whatever the process
  can, and a tool's side-effect class is a claim the operator makes about it,
  not something the runtime verifies. The database driver
  (`modernc.org/sqlite`) is C SQLite transpiled to Go, not a Go
  implementation written against Go's own memory model: it is outside the
  memory-safety guarantees the rest of this Go program has, in the same way
  a cgo binding to the original C would be.
- **Prompt injection as such.** Untrusted content will reach the model and will
  sometimes steer it. The runtime's answer is the bound on what a steered model
  can do, not detection. If a policy allows a class of action, an injected
  instruction can use it.
- **What a tool does with its arguments.** Schema validation says the arguments
  fit the schema. Path traversal, command construction, and authorization
  inside a tool are the tool's own responsibility, and for an MCP tool they are
  the server's.
- **MCP servers.** A pinned server is still code this project did not
  write, reached over a pipe or a socket. Pinning covers the tool's name,
  description, and input schema; it does not cover what the server does
  when called, and its results are untrusted model input. A stdio server
  runs as the operator: the minimal environment withholds variables, not
  files, so credential files under `HOME` (such as `~/.aws`, `~/.ssh`, or
  a token cache), anything else the operator's account can read, and any
  directory on `PATH` the operator can write remain as reachable to the
  server as to the operator, as does the network. A streamable-HTTP
  server is reached over whatever network access the process has, and
  this process's HTTP clients honour the proxy variables.
- **Re-running an interrupted side effect.** An operator who approves an
  `interrupted_side_effect` runs a call that may already have taken
  effect. The runtime cannot tell; the approval says so, and the choice
  is the operator's.
- **Secrets.** Provider keys come from the consumer's configuration. The
  adapters redact the key in their own output and keep it out of their
  errors, but decisions, arguments, observations, and approval
  presentations are persisted verbatim, so a tool that returns a secret
  puts it in the database and the audit log. The runtime does not scan
  for or redact secrets in that data.
- **Multi-tenancy.** One database belongs to one operator. There are no users,
  no roles, and no authentication; `decided_by` is a string the caller
  supplies, and approve identity is a label an operator's tooling can trust
  only as far as it trusts whoever can write to the database.
