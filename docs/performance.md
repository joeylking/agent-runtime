# Performance

What a run costs the runtime, what was changed to bring that down, why
each change is safe, and what was measured and left alone. The numbers
come from the benchmarks in `bench_test.go`, `render/bench_test.go`, and
`view/bench_test.go`, run with `-benchmem -count=5` on an Apple M5 Max
against a file-backed WAL store in `b.TempDir()`, and compared with
benchstat (p=0.008, n=5, for every row below).

```
GOWORK=off go test -run '^$' -bench . -benchmem -count=5 ./ ./render ./view
```

## Before and after

### Driver: whole runs of a scripted agent

A run of n steps is n-1 allowed tool calls with distinct arguments, then
`complete`. The tool answers either with its arguments ("small") or with
about 8 KiB of JSON.

| Run | time per step | | bytes per run | | allocations per run | |
|---|---|---|---|---|---|---|
| | before | after | before | after | before | after |
| 10 steps, small | 329 µs | 214 µs | 478 KiB | 437 KiB | 10.1k | 7.6k |
| 10 steps, 8 KiB | 435 µs | 290 µs | 2.50 MiB | 1.59 MiB | 10.4k | 7.9k |
| 100 steps, small | 405 µs | 211 µs | 12.6 MiB | 8.1 MiB | 249k | 77k |
| 100 steps, 8 KiB | 919 µs | 285 µs | 150 MiB | 21 MiB | 253k | 80k |
| 400 steps, small | 667 µs | 229 µs | 156 MiB | 83 MiB | 3.02M | 310k |
| 400 steps, 8 KiB | 2,445 µs | 290 µs | 2,244 MiB | 135 MiB | 3.06M | 320k |

A 400-step run with 8 KiB observations took 978 ms and now takes 116 ms.
The cost of a step no longer grows with the number of steps before it.

### render

`Messages` renders the last decision of a run; the whole run renders
every decision, as a model agent does once per step.

| Render | time | | bytes | | allocations | |
|---|---|---|---|---|---|---|
| | before | after | before | after | before | after |
| last decision, 10 steps, small | 1.94 µs | 1.06 µs | 6.6 KiB | 4.5 KiB | 60 | 24 |
| last decision, 10 steps, 8 KiB | 12.0 µs | 6.7 µs | 87 KiB | 53 KiB | 60 | 24 |
| last decision, 100 steps, small | 18.0 µs | 9.8 µs | 63 KiB | 46 KiB | 603 | 204 |
| last decision, 100 steps, 8 KiB | 99.0 µs | 13.8 µs | 862 KiB | 94 KiB | 603 | 204 |
| last decision, 400 steps, small | 78.0 µs | 38.3 µs | 299 KiB | 180 KiB | 2,550 | 1,104 |
| last decision, 400 steps, 8 KiB | 376 µs | 36.4 µs | 3,495 KiB | 228 KiB | 2,551 | 1,104 |
| whole run, 10 steps, small | 11.4 µs | 6.7 µs | 39 KiB | 26 KiB | 339 | 152 |
| whole run, 10 steps, 8 KiB | 56.9 µs | 46.4 µs | 479 KiB | 386 KiB | 339 | 152 |
| whole run, 100 steps, small | 958 µs | 557 µs | 3.35 MiB | 2.25 MiB | 30.5k | 10.5k |
| whole run, 100 steps, 8 KiB | 4.88 ms | 0.97 ms | 42.8 MiB | 6.8 MiB | 30.6k | 10.5k |
| whole run, 400 steps, small | 15.3 ms | 8.0 ms | 54.9 MiB | 36.3 MiB | 493k | 207k |
| whole run, 400 steps, 8 KiB | 75.4 ms | 8.5 ms | 681 MiB | 54.9 MiB | 494k | 207k |

### view

| | time | | bytes | | allocations | |
|---|---|---|---|---|---|---|
| | before | after | before | after | before | after |
| `view.Runs`, 1000 runs, 500 waiting | 7.72 ms | 1.95 ms | 4.25 MiB | 2.49 MiB | 63.6k | 28.1k |

## What changed

### The loop keeps its steps

The loop reread every prior step and approval from the store at every
step, so a step cost O(n) rows, unmarshalled, and `Store.ListSteps` was
44% of CPU and 98% of the bytes allocated. A loop now loads its steps and
approvals once, when `Start` or `Resume` enters it, and keeps them
(`cache.go`). It still reads the run row at every step, because model
calls and an operator change it.

The kept copy is exact because it is never built from the driver's own
structs. A write records the columns it bound (`stepRow`, `approvalRow`
in `store.go`); once the transaction commits, those columns are decoded
by the same code `ListSteps` and `ListApprovals` use. JSON the store
compacts or escapes, and times it truncates to UTC text, therefore come
back exactly as a fresh load returns them. A write that fails, at any
statement or at commit, applies nothing. A row that cannot be applied
marks the copy stale, and the next step loads afresh. Nothing is kept
between loops.

Another process changing the run is unaffected. The loop's writes are
still compare-and-set, so an operator's `Cancel` makes the next write
fail, and the loop returns the cancelled run
(`TestCancel_LiveLoopStopsAtItsNextWrite`).

### The agent and the policy get copies

`StepInput` and `RunView` used to share one slice and its pointers: an
agent could change what the policy was about to see. With a kept copy,
it could also have changed what every later step saw, and what loop and
failure detection read.

The agent and the policy each have a mirror of the kept steps and
approvals with bytes of their own. A mirror copies a step only when that
step has changed since its last handout, so the bytes cost O(1) per step.
Every handout is a fresh slice of fresh `Decision`, `PolicyDecision`, and
`Observation` structs over the mirror, allocated as three arrays. What a
consumer does to a field, to a struct behind a pointer, or to a slice
element therefore never persists. The one thing that persists is a write
into the bytes of a `RawMessage` a consumer was handed, and only into
that consumer's own later views: never into the driver's copy, the other
consumer's view, or the store.

Three handouts were measured on the final code. Sharing the kept slice
is not sound. A deep copy on every handout, bytes included, is sound but
O(total bytes) per step. The mirror is what was chosen.

| Run | time per step | | | bytes per run | | |
|---|---|---|---|---|---|---|
| | shared | mirror | deep copy | shared | mirror | deep copy |
| 100 steps, small | 204 µs | 225 µs (+10%) | 220 µs (+8%) | 3.9 MiB | 8.1 MiB | 8.0 MiB |
| 100 steps, 8 KiB | 278 µs | 292 µs (+5%) | 381 µs (+37%) | 15.2 MiB | 20.9 MiB | 96.1 MiB |
| 400 steps, small | 201 µs | 234 µs (+16%) | 251 µs (+25%) | 15.4 MiB | 83.1 MiB | 84.4 MiB |
| 400 steps, 8 KiB | 275 µs | 292 µs (+6%) | 647 µs (+135%) | 61.0 MiB | 134.6 MiB | 1,375 MiB |

The mirror's struct copies are O(n) per step: about 400 bytes per prior
step, per consumer. They are what its bytes column adds over sharing, and
the price of isolation at the level of Go values. With them, the
400-step 8 KiB run still allocates 17 times fewer bytes than the loop did
before. `Resume` gives the policy and the reconciliation copies too.
`StepInput.Tools` is a copy of the slice.

A loop keeps three copies of its run's step content: its own and one
mirror per consumer.

### Unchanged fields are not encoded again

A step is written up to four times (started, decided, executing, done).
Each write marshalled the decision, the policy decision, and the
observation again. A write now reuses the text of the previous write of
the same step for any field whose pointer the driver has not replaced.
The driver never writes through those pointers; it assigns new ones. On
the cached side, a field whose text is unchanged keeps its decoded value.
The same step is not updated twice within one transition any more; the
correctness batch had already merged those writes.

### Guards read only the status

The guard at the start of each step transaction loaded and parsed the
whole run row, limits JSON included. Where only the status matters, it
now reads `status` alone (`requireStatus`), and since the run lease,
whether the writer holds it. The error is the same.

### Prepared statements

The loop's seven hot statements are prepared when a writer opens the
store: the run select, the status guard, `claimStep`, the step insert and
update, the active-time increment, and the event insert. They are
prepared at open because the store has one connection, and a transaction
holds it for the rest of its life. Stores from `OpenExisting` prepare
nothing.

| Run | time per step, unprepared | prepared |
|---|---|---|
| 10 steps, small | 273 µs | 235 µs (−13.9%) |
| 100 steps, 8 KiB | 344 µs | 302 µs (−12.2%) |
| 400 steps, small | 287 µs | 256 µs (−10.7%) |
| 400 steps, 8 KiB | 352 µs | 309 µs (−12.1%) |

Bytes rise by 2 to 15%, because `Tx.StmtContext` allocates a wrapper per
use. That cost was judged worth the 12% in time.

### render

`DefaultObservation` converted every observation's content to a string
before deciding whether to summarise it. It now converts only content it
renders in full. `Messages` sizes its message slice once, carves each
turn's blocks from one allocation per role (each capped at its own
blocks, so appending to one turn cannot reach the next), and names tool
uses with `strconv` instead of `fmt`. Output is byte-identical to the
previous implementation, which is kept in `render/oracle_test.go` as the
oracle.

### view and the operator command

`Store.GetApproval` loaded every approval of the run, decoding each
request, and filtered in Go; it is now one indexed row. `view.Runs` ran
`ListApprovals` for each waiting run. It now runs one query,
`Store.PendingApprovalIDs`, which returns only ids, for waiting runs,
through a join. The method is exported because `view` is another
package, and it returns ids because a listing needs nothing else. `show`
read the approvals twice, once in `view.Summary` and once itself;
`view.Detail` reads the run, its steps, and its approvals once each.

## Equivalence

- `TestDriver_CachedViewEqualsReloadedView` runs one scenario twice with a
  deterministic clock and ids. One run uses the kept steps; the other
  rereads the store and encodes every field at every step, the old path,
  reachable only from tests through `Driver.reload`. The scenario covers
  an allowed step, a denial, an invalid decision, a tool error, an
  approval pause, a resume, a step interrupted while deciding, and a
  second resume. The test compares every `StepInput`, every policy
  `ToolRequest` and `RunView`, every event, the number of clock readings,
  and every stored column.
- `TestDriver_CacheEqualsStoreAfterEveryWrite` checks, after each of that
  scenario's writes, that the kept steps and approvals, and a handout of
  them, equal a fresh load.
- `TestDriver_FailedWriteLeavesCacheUnchanged` covers a rolled-back
  write; `TestDriver_CacheDecodesRewrittenFields` covers a field rewritten
  with new text, and one lent unchanged.
- `TestDriver_ConsumersCannotCorruptEachOther`: one consumer writes
  through everything it is handed, bytes included. The other checks that
  each of its views equals the store, and the run's events equal a clean
  run's.
- `TestDriver_ClockReadingsAreStable` is unchanged and passes: the
  number and order of `Config.Now` readings are the same.
- `TestMessages_MatchesOracle` renders 3,000 random runs, covering every
  step shape, window, cap, and hook combination, against the previous
  implementation. `TestMessages_AppendingToATurnLeavesTheNextAlone`
  covers the shared allocations.
- `TestRuns_OneQueryMatchesPerRunSummaries` checks that each `view.Runs`
  row equals `view.Summary` for the same run. The runs are waiting on one
  approval, waiting on two, cancelled with one left pending, and
  finished.
- repo-steward's `TestModelMode_ReplaysRecordedRuns` and all eleven of
  casework's replay scenarios pass against this tree unmodified.

## Measured and left alone

- **Commit.** After these changes, `Tx.Commit` is about 85% of the loop's
  CPU. A step is four transactions (started, decided, policy and tool
  start, done), one per state transition with its events. Merging them
  would change what a crash leaves behind and when the observer sees each
  event, so they stay.
- **Content checks and hashes.** `checkJSON` and `contentHash` parse each
  observation, about 22% of the bytes the loop still allocates. Their
  cost is proportional to the content, not to the run's length. The
  canonical encoder is pinned byte for byte to the stored hashes, so it
  was not touched.
- **The handout copy.** It is now the largest allocation, O(n) small
  structs per step; see its table above.
- **Indexes.** Every statement the loop runs uses a primary key or an
  existing index. `PendingApprovalIDs` scans approvals by status. With
  1000 runs and 500 pending approvals it takes 0.36 ms without an index
  on `approvals(status)` and 0.39 ms with one. No index is warranted, and
  no schema change was made.
- **Resume.** `Resume` loads the steps once to find the step it acts on;
  the loop it enters loads them again. That is once per resume, not per
  step.
- **`ListRuns`** parses every run's limits JSON; it is most of the
  remaining 1.95 ms of `view.Runs` and linear in the runs listed.

## The run lease

The lease (ADR 5) was measured on the final code against the same
benchmarks run just before it, `-count=3`, medians:

| Run | time per step | | bytes per run | | allocations per run | |
|---|---|---|---|---|---|---|
| | before | after | before | after | before | after |
| 10 steps, small | 215 µs | 223 µs | 437 KiB | 442 KiB | 7,617 | 7,729 |
| 10 steps, 8 KiB | 297 µs | 298 µs | 1.59 MiB | 1.60 MiB | 7,882 | 7,995 |
| 100 steps, small | 218 µs | 214 µs | 8.09 MiB | 8.13 MiB | 76,753 | 77,748 |
| 100 steps, 8 KiB | 296 µs | 291 µs | 20.9 MiB | 21.0 MiB | 79,587 | 80,606 |
| 400 steps, small | 236 µs | 232 µs | 83.2 MiB | 83.3 MiB | 309,773 | 313,789 |
| 400 steps, 8 KiB | 300 µs | 294 µs | 134.6 MiB | 134.8 MiB | 320,292 | 324,287 |

Time per step moves by less than run-to-run noise, in both directions.
The heartbeat is time-based, a write every third of the TTL, so a
benchmark run shorter than ten seconds renews nothing. What a step pays is
in the guards: the status read compares the lease owner in SQL, returning
a boolean rather than the owner's text, and the step claim binds the
owner, whose boxed value is made once per driver. That is about ten
allocations and a few hundred bytes per step, 1.3% of allocations. A run
also pays one wall-clock read per tool call. Taking the lease is two
columns in the write that moves the run to RUNNING, and resuming an
interrupted run takes it in one extra transaction, once per resume.

## Security fixes: durable commits

A store opened for writing now syncs every commit (`synchronous=FULL`
instead of `NORMAL`), because in WAL mode `NORMAL` can lose the last
commits on power loss, and the last commit may be the `step.tool_started`
of a tool that ran. That costs one `fsync` of the WAL per transaction,
four per step. Measured with the command below, `-count=3`, medians, on
the same machine: before the fixes, after them, and after them with the
sync level put back to `NORMAL` to separate its cost from the rest.

```
GOWORK=off go test -run '^$' -bench BenchmarkDriver -benchmem -count=3 .
```

| Run | time per step | | | bytes per run | | allocations per run | |
|---|---|---|---|---|---|---|---|
| | before | after | after, `NORMAL` | before | after | before | after |
| 10 steps, small | 219 µs | 305 µs (+40%) | 220 µs | 442 KiB | 446 KiB | 7,729 | 7,720 |
| 10 steps, 8 KiB | 310 µs | 377 µs (+22%) | 300 µs | 1.60 MiB | 1.60 MiB | 7,998 | 7,984 |
| 100 steps, small | 224 µs | 294 µs (+31%) | 219 µs | 8.14 MiB | 8.66 MiB | 77,777 | 77,643 |
| 100 steps, 8 KiB | 293 µs | 377 µs (+29%) | 301 µs | 21.0 MiB | 21.5 MiB | 80,597 | 80,468 |
| 400 steps, small | 234 µs | 316 µs (+35%) | 241 µs | 83.3 MiB | 92.3 MiB | 313,789 | 313,353 |
| 400 steps, 8 KiB | 295 µs | 385 µs (+30%) | 303 µs | 134.8 MiB | 143.8 MiB | 324,266 | 323,784 |

All of the added time is the sync: with `NORMAL` the fixed code runs as
fast as before. It is about 20 µs per commit here, on an Apple SSD, where
`fsync` does not force the drive's own cache (SQLite's `fullfsync` is
off). On a disk where `fsync` waits for the medium it is milliseconds per
commit, and a step's cost would be dominated by it. That cost was taken
on purpose: a lost `step.tool_started` would let the next owner treat a
tool that ran as one that never started.

The bytes grew with the structs the handouts copy: `Step` gained
`DecodeError` and `Decision` gained the three `Invalid` fields, and the
agent's and the policy's mirrors copy O(n) of each per step. The extra is
about 60 bytes per prior step per step, 2% at 100 steps and 11% at 400.
