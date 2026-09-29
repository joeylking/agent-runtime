# 5. A running run is leased to the process executing it

Decision: a run in RUNNING carries a lease, an owner id and an expiry, in
its own row. The lease is taken in the transaction that moves the run to
RUNNING, released in the one that pauses or finishes it, and renewed by a
heartbeat on the wall clock for as long as the call executing the run
lives. Every write the loop makes to a RUNNING run requires its lease. A
Resume of a RUNNING run whose lease is live and another owner's returns
`ErrRunLeased` and changes nothing; one whose lease has expired, or that
has none, takes it over as an interrupted run.

Why: a run found RUNNING is either being executed or was left behind by a
process that died, and nothing in the row told the two apart. Resume
assumed the second, so a second process could take over a run whose loop
was alive. The compare-and-set guards stopped the live loop at its next
write, but its tool call in flight still completed, and the newcomer could
enter the same step through reconciliation while it did. A lease is the
smallest record that answers the question: a live owner renews it, a dead
one cannot.

The heartbeat is time-based, not per step, because a tool or model call
can outlast any TTL a crash should be detected within. It writes one
statement outside any transaction, on a connection of its own for a
file-backed store: on the store's one shared connection it waited behind
whatever held it, and a consumer holding `DB()` with rows open starved
it until another process took a live run over. Renewal is compare-and-set
on the owner; when it fails, or the lease expires unrenewed, the loop
stops before its next side effect, a tool call or a model request, and
returns `ErrLeaseLost`. The writes that start those side effects check
the stored expiry against the wall clock as well as the owner, so a loop
stalled past its expiry starts nothing even when nobody has taken the run. A tool
call already in flight cannot be recalled. Its outcome is recorded only if
its loop still holds the lease when it returns; otherwise the step stays as
it was for the new owner, whose reconciliation treats it as interrupted,
which is the contract a crash already had. Lease times come from the wall
clock and never from `Config.Now`, because the number and order of `Now`
readings is a contract with consumers whose deterministic clocks stamp
their own records. Taking over a lease another owner let expire is written
as `lease.taken_over`, because that is the moment an operator will ask
about; acquiring, renewing, and releasing are not events, so the event
sequence of an ordinary run, which consumers' tests and replays compare,
is unchanged.

The owner stored is `Config.LeaseOwner` followed by a random suffix drawn
per `Driver`. A configured name alone identified nothing: two replicas
configured alike held one owner, so each took the other's live run as its
own, and one driver running a run in two calls did the same. With the
suffix no two drivers share an owner, and a driver also refuses a second
call on a run it is executing.

An operator's Cancel needs no lease: it is the operator's authority, it
releases the lease with the run, and a live loop stops at its next write
as before. A database written before the lease has runs with none. The
migration that adds the lease gives every RUNNING run one, `DefaultLeaseTTL`
long under a placeholder owner, because a process of the older version
may still be executing it; after that it is free to Resume. An older
version ignores leases altogether, so every process of one must be
stopped before a current one first opens the file.

Alternatives: an advisory file lock or SQLite's own locking, which do not
survive the process as a record and say nothing to a reader of the row;
a separate leases table, which is a second place a run's state lives;
renewing the lease in every write the loop makes, which misses exactly the
long calls a lease is for and adds a statement per step; stamping lease
times with `Config.Now`.

Tradeoffs: a process that dies holding a lease blocks Resume of its run
for up to one TTL, 30 seconds by default, including for the same process
restarted: there is no instant takeover by name, because a name is not
proof that the old process is gone. Lease expiry compares wall clocks, so
processes on different machines sharing a file need clocks agreeing to
well within the TTL, and SQLite on a network filesystem is unsupported.
The heartbeat is a goroutine, a second connection per file-backed store,
and a write every TTL/3 per running call.

Revisit when: runs are executed by more than one machine against a store
that is not one SQLite file, or a consumer needs to hand a live run from
one process to another without waiting for its lease.
