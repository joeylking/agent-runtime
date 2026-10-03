# Essays

Short pieces on the design ideas behind agent-runtime, for engineers
deciding whether to trust an agent to touch a real system. Each one stands
on its own, assumes no knowledge of this repository, links every claim to
the code or the test that makes it true, and ends with what it does not
claim.

1. [The model asks, the runtime decides](01-the-model-asks-the-runtime-decides.md):
   why permissions live in a policy written as code outside the model, and
   how that policy fails closed.
2. [An approval is a hash, not a yes](02-an-approval-is-a-hash-not-a-yes.md):
   an approval bound to the exact request and to what the operator was
   shown, and checked again before it is used.
3. [Charge for what you cannot see](03-charge-for-what-you-cannot-see.md):
   how limits on model calls, tokens, and spend hold when a request's
   outcome is unknown.
4. [The tables are the state, the events are the explanation](04-the-tables-are-the-state.md):
   an audit log written in the same transaction as the state, and what such
   a record can prove.
5. [When the runtime does not know, a person decides](05-when-the-runtime-does-not-know.md):
   crashes, the run lease, and why an interrupted side effect waits for an
   operator instead of running twice.
6. [Tools from someone else's server](06-tools-from-someone-elses-server.md):
   MCP tools classified by the operator, pinned by hash, and the limit of
   what pinning covers.

The same essays are in the [wiki](https://github.com/joeylking/agent-runtime/wiki/Essays).
Where a test is named, [docs/status.md](../status.md) lists it beside the
control it verifies.
