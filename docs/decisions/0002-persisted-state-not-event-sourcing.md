# 2. Persisted current state plus an audit log, not event sourcing

Decision: runs, steps, approvals, and model calls are the source of truth;
events are appended in the same transaction and never read for control
flow.

Why: resumption happens at most a few times per run and needs no replay
into alternative states. Ordinary table migrations are simpler and safer
than versioned event payloads with upcasters, and the current state is
visible with one query rather than after a fold. An interrupted run is
recovered from the rows, and the consumer reconciles its own journals.

Alternatives: event sourcing with a fold that rebuilds the run.

Tradeoffs: two writes per transition; a completeness test and a
consistency test keep the audit log honest.

Revisit when: a consumer needs projections or replay into branches.
