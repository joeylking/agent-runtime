# 6. An interrupted side effect waits for an operator

Decision: when `Resume` takes over a run and finds a step that was
executing a tool that is not `ReadOnly`, and the consumer has no
`Config.Reconcile`, the run does not continue. It pauses on that step's
recorded request through the ordinary pause, as an approval of kind
`interrupted_side_effect` whose presentation says the call started and its
outcome is unknown. Approving and resuming runs the same request again,
once, on the same step, after the same schema check and policy
re-evaluation as any grant; rejecting ends the run with the tool not
called again. A step whose tool is no longer registered, or whose
arguments no longer pass the tool's schema, cannot wait on an approval,
and the run fails as `reconcile_conflict`. A step interrupted while
executing a `ReadOnly` tool, or while deciding, continues as before. With
`Config.Reconcile` the consumer decides, as it always did.

Why: before this, a run without a reconciliation marked the step
interrupted and went back to the agent, which saw that its last call had
no result and, reasonably, asked for it again in a new step. The runtime
never re-ran the step itself, but the effect was the same: a payment,
a push, or a message sent twice, with nothing in the record to say the
second was a repeat of a call whose outcome nobody knew. The runtime
cannot tell whether a call that started took effect; only the consumer's
own records or a person can. A consumer that wrote no reconciliation has
said nothing about how to find out, so the decision goes to the operator,
who is shown exactly the call that was in flight and told its outcome is
unknown. A `ReadOnly` call is exempt because running it again changes
nothing.

Alternatives: documenting at-least-once delivery, so that an interrupted
side effect may be requested again by the agent and every tool must be
idempotent. Refused, because idempotency is a property of the outside
system a tool talks to, not something the runtime or most consumers can
guarantee: a payment API without idempotency keys, a git push, or an
email cannot be made safe by documentation, and a consumer that never
read the note would find out from a duplicate. It would also make the
runtime's own claim, that an action the policy gated runs once per
approval, false in exactly the case an operator most needs it. Also
considered: failing the run outright, which is safe but leaves the
operator no way to finish a run whose call they have checked by hand;
and requiring every consumer to supply `Reconcile`, which breaks every
consumer that has none for a case most of them never meet.

Tradeoffs: a consumer without `Reconcile` that relied on a resumed run
carrying on by itself after a crash now sees such a run wait for an
operator, and an operator must decide something the runtime cannot
check. An approved re-run is still a second execution of a call that
may have taken effect: the approval says so, and the choice is the
operator's. The pause goes through the consumer's policy again on resume,
so a policy that wants a different approval for the call pauses the run
a second time.

Revisit when: tools can declare that they are idempotent, or report
whether a given call took effect, so the runtime can decide without a
person.
