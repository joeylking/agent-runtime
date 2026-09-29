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

Everything below is on the `audit-fixes` branch, not yet released. Several
entries are breaking behaviour changes for anyone scripting against
`cmd/agentrt`'s text/JSON output or relying on the previous (looser)
adapter environment handling — read the **Changed** section before
upgrading a consumer.

### Security

- A recorded string from the model or an MCP server was printed to the
  terminal as-is, so a tool result or model reply could clear the screen or
  forge the look of an approval prompt. `cmd/agentrt` now sanitises every
  such field, including C1 control characters, before printing it.
- An MCP stdio server used to inherit this process's entire environment,
  including any provider keys held in it. It now gets a minimal base
  environment plus only the variables you name in `Env`/`InheritEnv`.
- The OpenAI adapter read `OPENAI_API_KEY` from the environment and sent it
  to whatever `BaseURL` was configured, including plain `http://`. It now
  reads the environment key only when `BaseURL` is empty or the default
  OpenAI endpoint, and refuses to send any key over `http://` to a host
  that is not loopback.
- The Anthropic adapter honoured the SDK's own environment chain
  (`ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, config profiles). It now
  takes nothing from the environment but the key, resolves the key and
  base URL itself, and refuses to build without a key or a price for the
  model.
- SDK errors from the Anthropic client carried the outbound request,
  including its key header, and could escape into logs or error output;
  they no longer do. Both adapters' `Config`/`Model` now print with the
  key redacted, and response bodies are read under a bound.
- An MCP call that supplied a value for a parameter the operator had fixed
  used to be silently overridden with the operator's value, so an approval
  could look like it covered a call it did not. The call now fails instead
  of being overridden.
- A `Deny` or `Fixed` parameter name that is misspelled, or that appears
  only inside a schema branch (`oneOf`/`anyOf`/`allOf`), used to leave that
  parameter reachable by the model. The tool is now refused at load.
- `examples/live` and `examples/mcp` used to write their database to a
  fixed, predictable path in the shared temporary directory. They now use
  a per-user directory created with owner-only (0700) permissions.
- `golang.org/x/text` bumped past GO-2026-5970 in every module (the
  affected symbol was not called here, but the module is required
  everywhere); `modernc.org/sqlite` and `golang.org/x/sys` also patched.
  The core still keeps exactly two direct dependencies.

### Fixed

- Two processes resuming the same approved run, or a live loop racing an
  operator cancel, could both act on it. Run and step status transitions
  are now compare-and-set inside the transaction that makes them, so
  exactly one caller wins and a cancel is never silently overwritten by a
  loop that has not yet noticed it.
- A crash between committing a step and committing the run it ends could
  leave an approved request either dropped or executed twice on the next
  resume. A step and the run it ends now commit together, as do a resume's
  policy evaluation, its run/step commit, and the tool start it approved.
- `Store`'s transaction helper now rolls back on panic instead of leaving
  one open; consumer data that is not usable JSON (malformed, invalid
  UTF-8, a repeated key) is recorded as an invalid decision or a tool error
  instead of panicking mid-transaction.
- Migrations now run inside one immediate transaction, fixing a race on
  concurrent first opens, and a database a newer build already migrated is
  refused (`ErrSchemaVersion`) rather than silently reused.
- `ReconcileWaiting` now pauses an interrupted call through the ordinary
  approval path, or fails as a conflict, instead of leaving it in an
  ambiguous state.
- Run and step lists, and which granted approval a resume picks, now use
  insertion order rather than sorting RFC3339Nano text, which does not
  sort correctly across all representable values.
- A `ServedError`'s usage is now charged to the run; lost connections are
  now matched by error rather than by substring; limit check-then-reserve
  is now serialised so concurrent calls within one step reserve correctly;
  an uncapped request under a projected token or cost limit is now refused
  before dispatch instead of after.
- Work already completed is now recorded on a context that outlives the
  caller's cancellation, so a cancelled caller still gets an accurate audit
  trail instead of a gap.
- `render.Truncate` could return more than `n` bytes when the cut fell
  inside a multi-byte rune; it now never exceeds `n`.
- The MCP tool pin was computed after the tool list round-tripped through
  `float64` JSON decoding, so a numeric change beyond float64 precision
  could go undetected. It is now computed from the raw `tools/list` bytes.
- `trace.JSONL` used to silently drop an event it could not encode; it now
  marks the event instead and surfaces write failures rather than losing
  them.
- Provider adapters no longer retry caller cancellation, DNS-not-found,
  certificate failures, or a malformed URL: none of these succeed on
  retry, and retrying them only spent the caller's time and call budget.
- A reply the provider served but the adapter could not use (no choice, no
  content) now still returns the usage the provider reported, instead of
  losing it.
- A response body over the size bound is now refused by name instead of
  failing partway through decoding.

### Changed

- **`cmd/agentrt` no longer creates or migrates a database.**
  `agentrt.OpenExisting` opens only what is already there: a mistyped
  `-db` path is now an error, not a fresh empty database. Creating and
  migrating the database remains the consumer's job, via
  `agentrt.OpenStore`.
- **`approve` and `reject` now require explicit consent**: `-approval
  <id>`, `-yes`, or `y` typed at an interactive prompt. Run
  non-interactively with none of those given and they refuse rather than
  guess. Both commands now print the approval they are about to decide
  before acting on it, reject a trailing positional argument as a usage
  error, and refuse an empty `-by` when `$USER` is also unset.
- Read commands (`runs`, `show`, `events`) now open the database
  read-only, and never write to the file even under an interrupted
  process.
- **Trace and event timestamps now carry a date and a UTC offset**
  (`2006-01-02 15:04:05 -0700` in the aligned text format), not a bare
  clock reading, which was ambiguous across midnight and across the
  operator's and the recorder's time zones. Anything parsing the previous
  format needs updating.
- `show` now lists every approval a run is waiting on, not just one.
- **`render.Decide` names, in the recorded reason, the tool calls a
  truncated or multi-tool-call reply did not execute**, and now caps every
  reason it produces rather than only some branches. It also fails outright
  on a context-window overflow instead of treating it as an ordinary
  truncation.
- **Status-related operator errors are now typed sentinels** —
  `agentrt.ErrRunState`, `agentrt.ErrNotPending`, and in `view`,
  `view.ErrNoPendingApproval` / `view.ErrNotPending` — instead of an
  untyped `fmt.Errorf`, so a caller can match them with `errors.Is`.
- **The number and order of `Config.Now` clock readings on every driver
  path is now pinned by a test** (`TestDriver_ClockReadingsAreStable`).
  The sequence itself is unchanged, but a consumer that keys its own
  deterministic records on it — as casework does — can rely on that
  sequence not moving silently in a future release without the release
  notes saying so.
- MCP: the shared default HTTP client is never reused across servers;
  duplicate tool names, runaway pagination, and oversized HTTP response
  bodies are now errors instead of being silently accepted; the content
  cap now counts encoded bytes; oversized structured content now falls
  back to the text blocks rather than changing the result's shape; and a
  connection error now includes the tail of the server's stderr.
- The recent-results window in `render` now counts rendered steps rather
  than raw steps.
- Anthropic's price table is dated 2026-09-29.

### Added

- ADR 4 and the `mcp` package doc now say plainly what pinning a tool set
  does not protect against: a pinned server can still return untrusted
  content, and pinning only detects a changed tool surface, not a
  malicious one.
- Golden-hash tests pin the canonical JSON encoder and the observation
  content hash as byte-identical to before this batch of changes; 3000
  random `render.Messages` comparisons against the previous implementation
  and casework's twelve schemas' `StrictSubset` output are pinned by
  golden files.
- Benchmarks for whole driver runs, rendering, and the run listing are now
  committed (`bench_test.go`, `render/bench_test.go`,
  `view/bench_test.go`); `docs/performance.md` documents what was measured
  and what was deliberately left alone.

### Removed

- The `crash` CI job. No file in the repository carries the `crash` build
  tag, so the job ran exactly the same suite as `unit` a second time; it
  is gone from `.github/workflows/ci.yml` rather than kept as a silent
  duplicate.

### Performance

- **A run's cost no longer grows with the number of steps before it.** The
  driver used to reload and decode every prior step on every loop
  iteration, so a run cost quadratic work: 400 steps with 8 KiB
  observations each took 978 ms and allocated 2.2 GiB. The same run now
  takes 116 ms and 135 MiB, and time per step is flat from 10 steps to
  400. No schema change; see `docs/performance.md` for the full tables.
- The agent and the policy each now receive their own copy of the cached
  steps and approvals, so neither can reach the driver's state or the
  other's view — closing that off without giving up the speedup.
- `render` copies an observation's content only when it renders it in
  full, checked against the previous implementation over 3000 random
  runs; `view.Runs` finds pending approvals in one query instead of one
  per run, `GetApproval` reads one row, and the loop's hot statements are
  now prepared once at open instead of per call.

### Run lease, retries, and connect timeout

- **A running run is now leased, and this is the first schema migration
  since v0.1.** Migration 5 adds `lease_owner` and `lease_expires_at` to
  `runs`. `OpenStore` applies it and keeps every existing run; a database
  written by v0.2.1 migrates with its runs intact. The operator command
  never migrates, so use the `agentrt` binary built from the same release
  as the consumer: an older or newer one refuses the database and says
  which side is newer.
- **Resume of a run whose loop is alive in another process is refused**
  with `ErrRunLeased`, which names the owner and the expiry and matches
  `ErrRunState` with `errors.Is`. Before, the second process took the run
  over. After a real crash, a resume is refused until the dead process's
  lease expires, 30 seconds by default. Set a stable `Config.LeaseOwner`
  so a restarted process resumes its own runs at once, or a shorter
  `Config.LeaseTTL`.
- A loop that loses its lease stops before its next tool call or model
  call and returns `ErrLeaseLost`. A tool call already in flight cannot be
  recalled; its outcome is recorded only if the loop still holds the
  lease, otherwise the step is left for the new owner's reconciliation.
  Taking over another owner's expired lease writes a `lease.taken_over`
  event. The ordinary path of a run emits no new events. An operator's
  Cancel needs no lease. See ADR 5.
- **Retries honour `Retry-After`.** All three adapters carry the provider's
  value on `TransientError.RetryAfter`, from 408, 409, 425, 429, and 5xx
  responses, in both header forms; a negative value or one over an hour
  is treated as absent. The accounting caller waits at least that long.
  It fails as `model_unavailable` without sleeping when the wait exceeds
  `ModelConfig.MaxRetryAfter` (one minute by default), the context's
  deadline, or the run's time limits. Plain backoff waits are now checked
  against the time limits too.
- **`mcp.Server.ConnectTimeout`** bounds connect, initialize, and the tool
  listing, 30 seconds by default, without shortening the session that
  `Load` returns. A phase that times out is named in the error, with the
  tail of a stdio server's stderr. A failed tool call now names its
  server.
- A body cut off mid-read is ambiguous in every adapter: the request was
  received and may have been billed, so the attempt is charged
  conservatively. The Anthropic path used to treat it as transient and
  charge nothing.
- `Approval` JSON omits `expires_at` and `decided_at` when they are zero,
  instead of emitting `0001-01-01T00:00:00Z`.
- Decision and tool JSON that escapes an unpaired surrogate is refused at
  the boundary, like duplicate keys and invalid UTF-8.
- Both model examples handle SIGINT and SIGTERM, close what they opened,
  and print the resume command when the run was recorded.
- Removed, unused by either consumer: `Store.CreateRun`, `StatusCreated`,
  `EventRunInterrupted`.
- `driver.go` is split into `loop.go`, `step.go`, `approval.go`, and
  `resume.go`, as pure moves.

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

[Unreleased]: https://github.com/joeylking/agent-runtime/compare/v0.2.1...HEAD
[v0.2.1]: https://github.com/joeylking/agent-runtime/releases/tag/v0.2.1
[v0.2.0]: https://github.com/joeylking/agent-runtime/releases/tag/v0.2.0
[v0.1.2]: https://github.com/joeylking/agent-runtime/releases/tag/v0.1.2
[v0.1.1]: https://github.com/joeylking/agent-runtime/releases/tag/v0.1.1
[v0.1.0]: https://github.com/joeylking/agent-runtime/releases/tag/v0.1.0
