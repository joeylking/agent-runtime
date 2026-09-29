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
statement outside any transaction, so on the store's one connection it
waits its turn like every other write and never holds the connection
across anything else. Renewal is compare-and-set on the owner; when it
fails, or the lease expires unrenewed, the loop stops before its next side
effect, a tool call or a model request, and returns `ErrLeaseLost`. A tool
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

An operator's Cancel needs no lease: it is the operator's authority, it
releases the lease with the run, and a live loop stops at its next write
as before. A database written before the lease has runs with none, and a
RUNNING one among them is free to Resume.

Alternatives: an advisory file lock or SQLite's own locking, which do not
survive the process as a record and say nothing to a reader of the row;
a separate leases table, which is a second place a run's state lives;
renewing the lease in every write the loop makes, which misses exactly the
long calls a lease is for and adds a statement per step; stamping lease
times with `Config.Now`.

Tradeoffs: a process that dies holding a lease blocks Resume of its run
for up to one TTL, 30 seconds by default, unless it restarts with the same
`LeaseOwner`. Lease expiry compares wall clocks, so processes on different
machines sharing a file need clocks agreeing to well within the TTL. The
heartbeat is a goroutine and a write every TTL/3 per running call.

Revisit when: runs are executed by more than one machine against a store
that is not one SQLite file, or a consumer needs to hand a live run from
one process to another without waiting for its lease.
