# Changelog

All notable changes to this project are documented here, in the shape
described by [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

The core module (`github.com/joeylking/agent-runtime`) and its nested
modules (`mcp`, `providers/ollama`, `providers/anthropic`,
`providers/openai`) version independently — see docs/roadmap.md's "Module
layout" — but the nested modules have so far always been re-tagged and
released alongside the core release they pin, so each dated section below
covers both together and says which nested-module tag shipped with it. An
entry names a specific module only when the change is not in the core.

## [Unreleased]

## [v0.3.0] - 2026-10-01

Nested modules `mcp`, `providers/ollama`, `providers/anthropic`, and
`providers/openai` released at v0.2.0, pinning this core release.

This release follows an audit and a four-position security review of
v0.2.1. Several entries change behaviour a consumer or an operator will
meet on the first run. Before upgrading:

1. **Stop every older process and operator binary before this version
   first opens a database.** `OpenStore` applies migration 5, the run
   lease, on first open. An older version ignores leases and would resume
   a run a current process is executing, and nothing in the file can stop
   it. Stop every consumer process and every `agentrt` binary that has the
   database open, on every host that shares it, then start them on this
   release.
2. **Fix the database file's mode if group or others can write it.** Such
   a database, or a WAL file beside it, is refused with
   `agentrt.ErrInsecureMode` until you run `chmod 600` on it. One that
   group or others can only read is tightened to owner-only by the first
   writer that owns it.
3. **A consumer without `Config.Reconcile` will see interrupted side
   effects pause for an operator.** After a crash, `Resume` no longer lets
   the run carry on past a tool call that was executing and is not
   `ReadOnly`: the run waits on an `interrupted_side_effect` approval.
   Handle it like any other approval, or supply `Config.Reconcile`.
4. **MCP stdio servers get only the variables you name.** Put each
   variable a server needs in `Server.InheritEnv` or `Server.Env`.
5. **The Anthropic adapter needs a key and a priced model when it is
   built, and refuses plain `http://` to anything but loopback.** Set
   `Config.APIKey` or `ANTHROPIC_API_KEY`; for a model the built-in price
   table does not list, set `Config.Name` and price that name in your own
   `PriceTable`.
6. **Redirects are not followed** by any provider adapter or by the MCP
   HTTP client. Point `BaseURL` or the server URL at the final address, or
   supply an `http.Client` with a `CheckRedirect` of your own.
7. **`examples/mcp` needs `(cd examples/mcp && npm ci --ignore-scripts)`
   once** before `pin` or `run`; it no longer runs `npx`.
8. **Code that reads a stored `Decision` for unparseable arguments reads
   `InvalidArgs`**, not `Args`: see Changed.
9. **Code that reads `Step` or `Approval` should check `DecodeError`**, set
   on a row that did not decode.
10. **repo-steward's `TestDecide_MapsFirstToolUse` must be updated when it
    re-pins**: `render.Decide` names the tool uses it did not execute in
    the recorded reason.
11. **Anything that parses trace or `agentrt` text output** must accept a
    date and a zone on every timestamp.
12. **Expect each step to take longer on disk.** Writers now sync every
    commit; see Performance.

### Security

- **An approval could be decided against something other than what the
  operator was shown.** `cmd/agentrt approve` and `reject` read the
  approval, print it, and decided against whatever the row held by then.
  They now pass the hash of the approval they printed to the new
  `agentrt.ApproveShown` / `RejectShown`, which compare it with the
  stored hash inside the transaction that records the decision, with the
  update conditional on it, and refuse with `agentrt.ErrApprovalChanged`
  if it differs; the command then exits 1 and asks the operator to look
  again. `view.ApproveShown` and `view.RejectShown` wrap them.
- **Text from a model, a policy, or a server was printed to the terminal
  as it was**, so a tool result could clear the screen or forge an
  approval block. `cmd/agentrt` now escapes every such field with
  `trace.Sanitize`, which escapes C0 and C1 controls, bidirectional
  overrides and isolates, zero-width and other invisible formatting
  characters (the zero-width joiner and non-joiner included), the line
  and paragraph separators, and the Unicode tag block as visible
  `\u{XXXX}`, and cuts more than four combining marks on one base
  character with a count of how many were dropped. Arabic, Hebrew,
  Devanagari, Thai, Japanese, and single-codepoint and flag emoji print
  unchanged; an emoji built with zero-width joiners, and text that uses
  the zero-width non-joiner, now show the joiner as an escape. `mcp`'s
  report and errors, and the examples' output, are escaped the same way.
- **The approval prompt printed arguments as one unbounded line**, so a
  padded argument could push the rest, and the id needed to decide, off
  screen. It now prints one field per line with its byte size, shows any
  string over 2000 bytes by its head and tail, says the presentation is
  supplied by the consumer's policy and may contain model-chosen text,
  and ends with a line `agentrt` writes itself naming the approval and
  the tool. `-json` output bounds an approval's capability, presentation,
  and arguments the same way (`view.BoundApproval`).
- **`runs`, `show`, and `events` read every row**, so a crafted or huge
  database could exhaust the operator's memory. They now read only the
  page they print, through new bounded store reads that cap every text
  column at `agentrt.MaxPageText` (64 Ki characters) and count totals
  with `COUNT`. They take `-limit` and `-offset` (defaults 100 runs, 500
  steps and approvals, 500 events; `-limit 0` reads everything, a page at
  a time) and say how many more there are and how to see them. `-json`
  output carries the limit, the offset, and the totals.
- **A database file others could write was opened**, and anyone who can
  write it can forge an approval. A new database and its WAL files are
  created owner-only (`0600`); an existing database or WAL file that
  group or others can write, a WAL file owned by another user than the
  database, and a WAL path that is not a regular file are refused with
  `agentrt.ErrInsecureMode`, naming the file, its mode, and the fix. A
  database others can only read is made owner-only by a writer that owns
  it. The rules are skipped on Windows.
- **A crafted database could run its author's SQL inside the runtime's
  statements.** A database holding a trigger or a view is refused by both
  openers with `agentrt.ErrUnsafeSchema`, and every connection to a
  database file runs with `trusted_schema` off and SQLite's defensive
  mode on.
- **`OpenExisting` followed a symbolic link.** It now refuses one with
  `agentrt.ErrSymlink`, and refuses anything else that is not a regular
  file, so `agentrt` opens the file the operator named. `cmd/agentrt`
  turns each open refusal into one sentence at exit 1, matched by
  `errors.Is` and `errors.As`.
- **Usage a provider reported was believed.** A negative count or more
  cached input than input lowered a run's totals and cost below the
  limits. Such usage, and usage an adapter could not read because a
  count was negative, too large to hold, or not an integer (reported as a
  `ServedError` wrapping the new `agentrt.ErrUnusableUsage`, which
  `providers.ErrUnusableUsage` now is), is charged the conservative
  estimate instead; the reply is not used, the call is not retried, and
  the run fails as `model_unavailable`. Totals and costs saturate at the
  maximum rather than wrap negative, so a huge count trips the limits.
- **JSON nested deep enough to fail later could be stored.** Decision
  arguments and results, tool content, approval capabilities and
  presentations, a reconciliation's result, and tool input schemas
  nested deeper than 256 levels are refused where they enter: as an
  invalid decision, a tool error keeping the bytes, a failed registration,
  or a run failed as `internal_error`.
- **A tool schema could read a file or fetch a URL when compiled.** Core
  schemas are compiled from themselves and the standard metaschemas
  only; a `$ref`, `$schema`, or `$id` naming anything else fails
  registration. `mcp.Load` also refuses a server-supplied schema that
  reaches outside itself before the compiler sees it, inside
  `Server.ConnectTimeout`.
- **Leases could be shared or starved.** The stored lease owner is now
  `Config.LeaseOwner` followed by a random suffix per `Driver`: a sweep
  reproduced a side effect executed more than once when two processes, or
  a restarted one, used the same `LeaseOwner`. A driver refuses a second
  call on a run it is executing (`ErrRunLeased`). On a file-backed store
  the heartbeat renews on its own connection, so a consumer holding
  `DB()` cannot starve it. The writes that start a tool or dispatch a
  model request require the lease unexpired by the wall clock, so a loop
  stalled past its expiry starts nothing. Taking over one's own expired
  lease is recorded as `lease.taken_over`. A fast resume after a crash
  now comes from a lower `Config.LeaseTTL`, not a stable `LeaseOwner`.
- **Redirects carried keys to other hosts.** The provider adapters and
  the MCP streamable-HTTP client follow no redirect unless the consumer's
  `http.Client` has a `CheckRedirect` of its own; a 3xx is a permanent
  error naming only the host it pointed to (`providers.RefuseRedirects`).
- **A key could leave the machine over plain http.** Both key-carrying
  adapters send a key over `http://` only to exactly `localhost` or a
  literal loopback address, and refuse a connection whose dialled
  address is not loopback (`providers.CheckEndpoint`, `Loopback`,
  `LoopbackOnly`, `ErrNotLoopback`). The OpenAI adapter reads
  `OPENAI_API_KEY` only for the default endpoint. The Anthropic adapter
  takes nothing from the environment but `ANTHROPIC_API_KEY`, ignores the
  SDK's base-URL and auth-token variables, and refuses to build without a
  key or, unless `Config.Name` is set, a price for the model.
- **Keys could be printed.** The OpenAI and Anthropic `Config` and
  `Model` print, marshal to JSON, and log through `log/slog` with the key
  redacted, and an Anthropic SDK error, which holds the request and its
  key header, is converted before it leaves the adapter.
- **An MCP stdio server inherited this process's whole environment**,
  provider keys included. It now gets `PATH`, `HOME`, and `TMPDIR` (the
  Windows equivalents on Windows) plus the variables named in
  `Server.InheritEnv` and `Server.Env`. On Unix it runs in a process group
  of its own and `Close` kills the group, descendants included; on Linux
  the kernel also kills it if this process dies first (`Pdeathsig`).
- **MCP pins and results could disagree with what the SDK used.** Keys in
  a `tools/list` page are matched exactly, as the SDK matches them, so a
  page carrying both `tools` and `Tools` cannot pin one list and register
  the other; an SSE event the SDK ignores is not recorded; structured
  results keep numbers past 2^53 as sent. The pin itself is computed from
  the raw `tools/list` bytes rather than a `float64` round trip.
- **An MCP call could set a fixed parameter behind an approval.** A call
  that supplies a value for a parameter the operator fixed now fails
  instead of being overridden, and a `Deny` or `Fixed` name that is
  misspelled or appears only inside `oneOf`/`anyOf`/`allOf` refuses the
  tool at load.
- **`examples/mcp` ran whatever `npx` fetched that day.** It now runs the
  filesystem server pinned by version and integrity hash in
  `package-lock.json`, installed with `npm ci --ignore-scripts` and
  started with `node`, and refuses a manifest or rules file that group or
  others can write. Both examples keep their files under
  `os.UserCacheDir()/agentrt`, created `0700`, instead of fixed names in
  the shared temporary directory.
- **Recordings were written world-readable through a predictable name.**
  `replay.Recorder` creates its directory `0700` and writes each
  recording through a temporary file of its own, `0600`, synced, then
  renamed.
- **CI:** checkouts set `persist-credentials: false`, every job verifies
  the module cache with `go mod verify` before building, workflows run
  with read-only permissions, and every action is pinned to a commit.
  Dependabot holds a version back before proposing it: for Go modules 3
  days for a patch, 7 for a minor, and 14 for a major version; 7 days for
  GitHub Actions.
- `golang.org/x/text` is past GO-2026-5970 in every module (the affected
  symbol was not called), and `modernc.org/sqlite` and `golang.org/x/sys`
  are patched. The core keeps exactly two direct dependencies.

### Fixed

- Two processes resuming one approved run, or a live loop racing an
  operator's cancel, could both act on it. Run and step status
  transitions are compare-and-set inside the transaction that makes them:
  one caller wins, the other gets `ErrRunState`, and a cancel is never
  overwritten.
- A crash between committing a step and committing the run it ends could
  drop an approved request or execute it twice. A step and the run it
  ends commit together, as do a resume's policy evaluation, its move to
  RUNNING, and the tool start it approved.
- A resumed approval could execute arguments the tool's current schema
  rejects. `Resume` checks them first and ends the step as
  `invalid_decision` if they fail.
- An older grant of a step paused again could still be used. Only a
  step's latest approval can be a grant; with it pending, `Resume` returns
  `ErrRunState` and changes nothing, and repeated calls do not pile up
  approvals.
- A row that would not decode made its run unreadable. `ListSteps` and
  `ListApprovals` return it with `DecodeError` set; the run can be listed,
  shown, and cancelled; a driver asked to continue it fails it as
  `internal_error` without rewriting the row; such an approval cannot be
  decided.
- An approval hash could be computed over a stand-in when a request did
  not encode. The hash is now an error, the run fails as
  `internal_error`, and a stored approval carrying such a hash is refused
  by `Approve` and `Resume` with `ErrApprovalHash`.
- A tool's outcome was lost when an operator cancelled the run while the
  tool ran. It is appended as a `step.tool_finished` event marked `late`,
  after `run.finished`; the run stays cancelled.
- `ReconcileWaiting` pauses the interrupted call through the ordinary
  approval path only when a tool call was executing and its arguments
  still pass the tool's schema; otherwise the run fails as
  `reconcile_conflict`.
- `Store`'s transaction helper rolls back on panic; consumer JSON that is
  malformed, invalid UTF-8, has a repeated key, or escapes an unpaired
  surrogate is recorded as an invalid decision or a tool error instead of
  panicking or being stored lossily.
- Migrations run inside one immediate transaction, so concurrent first
  opens are safe, and a database a newer build migrated is refused with
  `ErrSchemaVersion`. A path containing `?`, `#`, or `%` opens the file it
  names.
- Lists, and the choice of the granted approval, follow insertion order;
  RFC3339Nano text does not sort.
- A `ServedError`'s usage is charged to the run; lost connections are
  matched by error, not text; concurrent calls in one step reserve
  against the limits before dispatch; a request with no output cap is
  refused before dispatch under a token or cost limit.
- Work already done is recorded on a context that outlives the caller's
  cancellation, and the cancellation is returned afterwards.
- A body cut off mid-read is charged as ambiguous in every adapter; the
  Anthropic path used to retry it for free.
- Provider adapters no longer retry caller cancellation, a host that does
  not resolve, certificate failures, or a malformed URL; a reply the
  provider served but the adapter could not use returns the usage it
  reported; a body over 16 MiB is refused by name.
- `render.Truncate` could return more than `n` bytes; `render.Decide`
  capped some reasons after appending to them. Both now stay within their
  bound.
- `trace.JSONL` dropped an event it could not encode; it now marks it, and
  `WriterWithErr` and `JSONLWithErr` surface write failures.
- MCP: the shared default HTTP client is never used; duplicate tool
  names, runaway pagination, and HTTP bodies over 16 MiB are errors; the
  content cap counts encoded bytes; oversized structured content falls
  back to the text blocks; a connection error includes the tail of the
  server's stderr; a failed call names its server; `Close` is safe twice
  and during a call.
- `Approval` JSON omits `expires_at` and `decided_at` when they are zero.

### Changed

- **`Decision` has three new fields for input that was not usable JSON:
  `InvalidArgs`, `InvalidResult`, and `InvalidBase64`** (JSON
  `invalid_args`, `invalid_result`, `invalid_base64`). A decision whose
  arguments or result did not parse is stored with the bytes there and
  `Args` or `Result` empty, where earlier releases stored
  `{"invalid_json": ...}` in `Args`. `render.Args` renders such a decision
  exactly as before, so replay keys are unchanged.
- **`Step` and `Approval` carry `DecodeError`** (`decode_error` in
  `Approval` JSON), set when a stored column did not decode.
- **`render.Decide` names, in the recorded reason, the tool uses a reply
  asked for but that were not executed**, each name capped at
  `render.NameBytes`, and caps the composed reason at `ReasonBytes`. It
  fails on a context-window overflow instead of treating it as a
  truncation. repo-steward's `TestDecide_MapsFirstToolUse` pins the old
  reason and must be updated when repo-steward re-pins.
- **Timestamps carry a date and a zone.** `trace.Writer` lines use
  `2006-01-02 15:04:05.000 -0700` and `cmd/agentrt` uses
  `2006-01-02 15:04:05 -0700`, where both printed a bare clock reading.
- **`cmd/agentrt` no longer creates or migrates a database**: a mistyped
  `-db` is an error. Use the `agentrt` built from the same release as the
  consumer; an older or newer one refuses the database and says which
  side is newer. Read commands open read-only.
- **`approve` and `reject` require `-approval <id>`, `-yes`, or `y` at an
  interactive prompt**, print the approval before deciding, reject a
  trailing argument, and refuse an empty `-by` when `$USER` is unset.
- **`show` lists every pending approval**, paged with the steps, and its
  `-json` output adds `approvals_total`.
- **Resume of a run whose loop is alive in another process is refused**
  with `ErrRunLeased`, which names the owner and expiry and matches
  `ErrRunState`. After a real crash, a resume waits until the dead
  process's lease expires, 30 seconds by default.
- **Retries honour `Retry-After`.** All three adapters set
  `TransientError.RetryAfter` from 408, 409, 425, 429, and 5xx responses,
  in both header forms; a negative value or one over an hour is absent.
  The accounting caller waits at least that long, and fails as
  `model_unavailable` without sleeping when the wait exceeds
  `ModelConfig.MaxRetryAfter` (one minute by default), the context's
  deadline, or the run's time limits.
- Operator errors are typed: `agentrt.ErrRunState` and
  `agentrt.ErrNotPending`, and in `view`, `ErrRunNotWaiting`,
  `ErrApprovalDecided`, and `ErrDatabaseLocked`. `view.PendingApproval`
  names at most 20 candidates when a run has several pending approvals,
  and says how many more.
- `Limits.ApprovalTTL` bounds only how long an approval may stay pending,
  as it always did.
- The number and order of `Config.Now` readings on every path is pinned by
  `TestDriver_ClockReadingsAreStable`; the sequence is unchanged.
- The Anthropic adapter's `StrictSubset` treats keys under `properties`
  and `$defs` as names, carries top-level keywords so a `$ref` resolves,
  and refuses what strict mode cannot express; output for casework's
  twelve schemas is unchanged. Cache writes are priced at the rate of
  their class, and the price table is dated 2026-09-29.
- The recent-results window in `render` counts rendered steps rather than
  raw steps.
- `driver.go` is split into `loop.go`, `step.go`, `approval.go`, and
  `resume.go` as pure moves.
- CI runs weekly as well as on push, cancels superseded branch runs,
  and has a `lint` job (gofmt, staticcheck, govulncheck, a tidy check on
  the core); `release-check.yml` builds each nested module with no
  workspace against the core version it pins.

### Added

- `Limits.GrantTTL`: how long an approved grant may wait for `Resume`,
  measured from its decision. Zero, the default, is no expiry; a run
  stored before this release reads it as zero. A grant older than that is
  expired at `Resume`, and the run is cancelled as `approval_expired`
  with nothing executed.
- Bounded store reads for front ends: `Store.ListRunsPage`,
  `GetRunCapped`, `ListStepsPage`, `ListApprovalsPage`, `ListEventsPage`,
  `PendingApprovalIDsOf`, and `MaxPageText`, and in `view`, `RunsPage`,
  `DetailPage`, and `RunDetailPage`.
- `agentrt.InterruptedSideEffect`, the approval kind of an interrupted
  side effect; ADR 6 records the decision.
- `agentrt.ErrInsecureMode`, `ErrUnsafeSchema`, `ErrSymlink`,
  `ErrApprovalChanged`, `ErrUnusableUsage`, `ErrRunLeased`, `ErrLeaseLost`,
  `ApproveShown`, `RejectShown`, `Config.LeaseTTL`, `Config.LeaseOwner`,
  `DefaultLeaseTTL`, `ModelConfig.MaxRetryAfter`, `DefaultMaxRetryAfter`,
  `TransientError.RetryAfter`, and `Store.PendingApprovalIDs`.
- `view.MaxFieldBytes`, `BoundValue`, `BoundRaw`, `BoundApproval`,
  `ApproveShown`, and `RejectShown`; `trace.WriterWithErr` and
  `JSONLWithErr`; `render.Args` and `render.NameBytes`.
- `providers.RefuseRedirects`, `CheckEndpoint`, `Loopback`,
  `LoopbackOnly`, `ErrNotLoopback`, `UsageCount`, `CheckUsage`, and
  `ErrUnusableUsage`.
- `mcp.Server.ConnectTimeout` (30 seconds by default,
  `mcp.DefaultConnectTimeout`), which bounds connecting, initializing,
  the tool listing, and Load's checks without shortening the session.
- Both model examples handle SIGINT and SIGTERM, close what they opened,
  and print the resume command when the run was recorded.
- Golden tests pin the canonical JSON encoder, the observation content
  hash, and the approval hash byte for byte; `render.Messages` is checked
  against the previous implementation over 3000 random runs.
- Benchmarks for whole driver runs, rendering, and the run listing
  (`bench_test.go`, `render/bench_test.go`, `view/bench_test.go`), with
  the tables in `docs/performance.md`.
- ADR 4 and the `mcp` package doc say what pinning does not protect
  against.

### Removed

- `Store.CreateRun`, `StatusCreated`, and `EventRunInterrupted`, which
  neither consumer used.
- The `crash` CI job: no file carried its build tag, so it ran the unit
  suite a second time.

### Performance

- **A step's cost no longer grows with the steps before it.** The loop
  reloaded and decoded every prior step on every iteration: 400 steps
  with 8 KiB observations took 978 ms and allocated 2.2 GiB. The loop now
  loads a run's steps and approvals once and keeps them; the same run
  took 116 ms and 135 MiB before durable commits (below), and time per
  step is flat from 10 steps to 400. `view.Runs` finds pending approvals
  in one query, `GetApproval` reads one row, and the loop's statements
  are prepared once at open.
- **Writers are slower per step: every commit is synced.** A store opened
  for writing uses `synchronous=FULL` instead of `NORMAL`, because in WAL
  mode `NORMAL` can lose the last commits on power loss, and the last may
  be the `step.tool_started` of a tool that ran. That is one `fsync` per
  transaction, four per step. Measured with `BenchmarkDriver` on an Apple
  SSD, medians of three: 10 small steps went from 219 to 305 µs per step
  (+40%), 100 small steps from 224 to 294 µs (+31%), 400 steps with
  8 KiB observations from 295 to 385 µs (+30%); with the sync put back to
  `NORMAL` the fixed code runs as fast as before. On a disk where `fsync`
  waits for the medium it is milliseconds per commit, and a step's cost
  is dominated by it. `docs/performance.md` has the full table.
- Bytes allocated per run grew about 60 bytes per prior step per step,
  2% at 100 steps and 11% at 400, from the new `Step` and `Decision`
  fields the loop copies.

## [v0.2.1] - 2026-09-25

Nested modules `mcp`, `providers/ollama`, `providers/anthropic`,
`providers/openai` released at v0.1.1, pinning this core release.

### Fixed

- The audit summary for a reply cut off by the output cap, or a reply
  with no tool call, now says so instead of reporting an unknown decision
  kind.

### Added

- Roadmap item 8, open-source hygiene: `CONTRIBUTING.md`, `SECURITY.md`,
  issue forms, runnable `Example` functions verified by `go test`, and a
  stated pre-1.0 compatibility promise.

### Changed

- The core's `go` directive moved to 1.26, with CI on Go 1.26 and 1.27.
  No API change.

## [v0.2.0] - 2026-09-25

Nested modules `mcp`, `providers/ollama`, `providers/anthropic`,
`providers/openai` released at v0.1.0, pinning this core release.
Roadmap items 1 and 2.

### Added

- Core, dependency-free: `providers` (transport classification, bounded
  body reads, price lookup that refuses an unpriced paid model, dollar
  parsing); `render` (recorded steps to model messages with a
  recent-results window and a content cap, hooks reproducing both existing
  consumers byte for byte, and `Decide` with the deliberately invalid
  kinds `truncated` and `no_tool_call`); `trace` (stderr and JSON Lines
  observers); `view` (run, step, and pending-approval read models with
  matchable sentinels); `NeedApproval` and `DecodePresentation` for
  policies; `cmd/agentrt` with `runs`, `show`, `events`, `approve`,
  `reject`, `cancel`.
- Nested modules, each tagged separately: `providers/ollama`,
  `providers/anthropic`, `providers/openai` implementing `agentrt.Model`
  under one contract; `mcp` puts an MCP server's tools behind the policy
  with operator classification, a pinned manifest of schema and
  description hashes, hint cross-checks that can only refuse, and
  per-parameter denial and fixing. ADR 4 records why classification is the
  operator's.
- `examples/live` runs a local model and pauses for an operator;
  `examples/mcp` runs against the stock MCP filesystem server.

### Changed

- A run returned at pause now carries the store's model totals.

Both consumers, repo-steward and casework, deleted their own copies of the
extracted code, and their recorded replays passed unchanged.

## [v0.1.2] - 2026-09-23

### Fixed

- The replay recorder's provider raw bytes were HTML-escaped and
  re-indented on disk; they now round-trip byte-identical.
- Identical requests overwrote each other's recording; they are now
  recorded in sequence and replayed in order, and an exhausted sequence
  returns `ErrNotRecorded` instead of the wrong recording.

## [v0.1.1] - 2026-09-22

### Added

- Operator `cancel` for a run that is not terminal, ending it as
  `operator_cancelled`. Found necessary by the first real publication run
  of repo-steward: an approval had been granted, the resume failed, and
  nothing could close the run.

## [v0.1.0] - 2026-09-20

First tagged release. A small Go runtime for tool-using agents: policy
evaluated outside the model, hash-bound durable approvals with resume,
step, failure, loop, call, token, cost, and time limits, an accounting
model caller with retries, record and replay models, and an audit log
written with the state. See `docs/architecture.md` and `docs/status.md`.

[Unreleased]: https://github.com/joeylking/agent-runtime/compare/v0.3.0...HEAD
[v0.3.0]: https://github.com/joeylking/agent-runtime/compare/v0.2.1...v0.3.0
[v0.2.1]: https://github.com/joeylking/agent-runtime/releases/tag/v0.2.1
[v0.2.0]: https://github.com/joeylking/agent-runtime/releases/tag/v0.2.0
[v0.1.2]: https://github.com/joeylking/agent-runtime/releases/tag/v0.1.2
[v0.1.1]: https://github.com/joeylking/agent-runtime/releases/tag/v0.1.1
[v0.1.0]: https://github.com/joeylking/agent-runtime/releases/tag/v0.1.0
