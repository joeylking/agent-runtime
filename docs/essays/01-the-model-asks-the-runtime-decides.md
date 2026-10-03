# The model asks, the runtime decides

An agent is a program that lets an AI language model take actions. The
program gives the model a list of tools it may request, such as reading a
file, running a command, or opening a pull request. The model picks one
and supplies its arguments, the program carries it out and shows the model
the result, and then it asks the model again. agent-runtime is a Go
library that runs this loop for one agent. This essay is about the
decision the library was built around: whether a requested action runs is
never the model's choice.

## An instruction is a request

The usual way to make an agent safe is to tell it the rules. The system
prompt says never delete files, never push to main, ask before sending
anything. A capable model follows those instructions most of the time, and
nothing makes it follow them every time. Its choice is a prediction, so
the same input can produce a different action on another run, and it
cannot reliably tell instructions from content. Text inside a file, a web
page, a dependency's changelog, or a command's output can redirect it.
This is called prompt injection, and no known technique prevents it inside
the model.

So an instruction to the model is a request. It shapes what the model
usually does. A bound is something else: a property of code that holds
whatever the model concludes. [ADR
3](../decisions/0003-policy-outside-the-model.md) records the decision in
one line, "the model never decides its own permissions", and gives its
condition for revisiting as "never; this is the premise of the project".

## What happens to every request

When the model asks for a tool, the runtime does the same things in the
same order ([`loop.go`](../../loop.go)):

1. It records the decision exactly as the agent returned it, before
   checking anything, so the record shows what was asked even when the
   request was malformed
   ([`TestDriver_InvalidArgumentsBecomeObservation`](../../driver_test.go)).
2. It validates the arguments against the tool's declared JSON Schema. A
   request that does not fit never reaches the policy or the tool.
3. It asks the policy.
4. It acts on the policy's answer.

The policy is code written by whoever builds the agent, called the
consumer. It has one method, `Evaluate(ctx, request, view)`
([`agentrt.go`](../../agentrt.go)), and returns one of four outcomes:
allow, deny, abort the run, or require approval from a person, called the
operator.

The runtime ships one policy, `SideEffectPolicy` in
[`policy.go`](../../policy.go). Every tool declares, when it is
registered, the kind of effect it has: read only, a local change, a remote
change, or destructive. The default policy allows the first two, sends
remote changes to the operator, and denies destructive tools. A tool that
declares any other kind is refused when the driver is built
([`TestNewDriver_RejectsBadTools`](../../driver_test.go)), and a kind the
policy has no entry for is denied rather than allowed.

Real consumers write more than that. The policy of
[repo-steward](https://github.com/joeylking/repo-steward/blob/main/internal/policy/policy.go),
which upgrades a Go dependency, decides by the phase of the run, whether
the target is eligible, whether a write touches a protected path, how far
the change has grown against its scope limits, and whether the repair
budget is spent. Its package comment says it "has no model input": its
facts come from the consumer's own session through an interface.

## What the policy sees, and what the model never holds

The policy receives the request, which is the run and step it belongs to,
the tool's registered specification, and the validated arguments, together
with a read-only view of the run: the run's row, every prior step, and
every approval. The agent receives a different set of things: the run, the
prior steps, the approvals, the tool specifications, and a handle for
calling the model (`StepInput` in [`agentrt.go`](../../agentrt.go)). It
gets no handle to the policy and none to the database. The model, in turn,
sees only what the agent writes into its prompt from those steps. So there
is no path from the model to the policy: the model produces text, the
agent turns the text into a decision, and the runtime hands the decision
to a policy the agent was never given.

Both handouts are copies. A change the agent makes to the steps it was
handed, or the policy to its view, reaches neither the driver nor the
other
([`TestDriver_ConsumersCannotCorruptEachOther`](../../cache_test.go)). An
innocent bug in one cannot quietly change what the other decides.

## A denial is data

When the policy denies a request, the runtime does not raise an error to
the agent or stop the run. It writes the step with an observation of kind
`policy_denied`, whose content names the tool and the policy's reason, in
the same database transaction as the policy decision. The tool is never
called ([`TestDriver_PolicyDenyIsObservation`](../../driver_test.go)).

The next time the agent decides, that step is part of the history it
renders. The `render` package turns it into an error result attached to
the model's own tool call
([`TestMessages_DeniedAndTruncatedNudges`](../../render/render_test.go)).
The model learns that the action was refused and why, and can choose
something else.

A denial also counts as a failed step, and enough failures in a row end
the run. The runtime's own example shows it: an agent that asks only for a
destructive tool is denied, and its run ends as `repeated_tool_failures`
([`ExampleDefaultPolicy`](../../example_test.go)). Abort is the stronger
answer. The run ends at once as `policy_abort` with nothing executed
([`TestDriver_PolicyAbortEndsRun`](../../driver_test.go)). Require
approval pauses the run on a durable approval bound to the exact request
([`TestDriver_RequireApprovalPausesRun`](../../driver_test.go)), which
[the next essay](02-an-approval-is-a-hash-not-a-yes.md) covers.

## Failing closed

A policy is code, and code has bugs. The runtime treats every way a
policy's answer can be unusable as a reason to stop, never as permission.
This is what failing closed means.

- An outcome the runtime does not recognise, including an empty one,
  fails the run as `internal_error` with nothing executed. The loop and
  the resume path share one function that acts on a policy's answer,
  `apply` in [`loop.go`](../../loop.go), so an outcome means the same in
  both, and the test runs the case through each
  ([`TestPolicy_UnknownOutcomeFailsLoopAndResume`](../../integrity_test.go)).
- A policy that returns an error fails the run the same way
  ([`loop.go`](../../loop.go)).
- An approval whose JSON is not usable fails the run without executing
  and without recording an approval
  ([`TestDriver_UnusablePolicyJSONFailsWithoutExecuting`](../../integrity_test.go)).
  Usable means one well-formed value in valid UTF-8, with no repeated
  object key, no unpaired escaped surrogate, and no more than 256 levels
  of nesting, because anything else would be stored or hashed lossily
  ([`hash.go`](../../hash.go),
  [`TestCheckJSON_RejectsWhatWouldHashLossily`](../../hash_test.go)).

The model's side is held to the same rule. Arguments that are not usable
JSON become an `invalid_decision` observation, with the original bytes
kept beside the decision as a string
([`TestDriver_UnusableJSONDecisionsAreRecordedInvalid`](../../integrity_test.go)).
Nothing on that path reaches a tool.

## Why not ask the model to confirm

ADR 3 names two alternatives and rejects both: asking the model to confirm
risky actions, and giving the model a permissions tool to call. Each puts
the decision back inside the thing being bounded. A model that has been
talked into deleting a directory can be talked into confirming the
deletion, and a permissions tool is one more tool an injected instruction
can reach. Code the model cannot address is the only place a rule holds
regardless of what the model read.

The cost is that the policy needs facts the model would otherwise infer,
such as which paths are protected or which phase the run is in. The
consumer supplies them through code of its own, and the `testkit` package
lets it check a policy as a table of requests and expected outcomes,
through the runtime's own validation first, with no model
([`TestCheckPolicy_AgreeingTablePasses`](../../testkit/testkit_test.go)).
repo-steward's policy tests are written this way, with an assertion that
an outcome never happens for a kind of effect
([`TestNever_HoldsAndFails`](../../testkit/testkit_test.go)).

## What this does not claim

- It does not stop prompt injection. Untrusted content will reach the
  model and will sometimes steer it. If the policy allows a kind of
  action, an injected instruction can use it. The bound is exactly as
  tight as the policy.
- It is not a sandbox. The agent, the tools, the policy, and the runtime
  share one process, so a tool can do whatever the process can. "No
  handle" describes the interface the agent is given, not isolation from
  a malicious consumer ([`SECURITY.md`](../../SECURITY.md)).
- A tool's kind of effect is a claim made when it is registered, by the
  consumer or, for a tool from an MCP server, by the operator. The
  runtime does not verify it.
- Schema validation says the arguments have the right shape. Path
  traversal, command construction, and authorization inside a tool are
  the tool's own responsibility.
- Two lines are verified by reading the code rather than by a test of
  their own: `SideEffectPolicy` denying a kind it has no entry for, and
  the loop failing a run whose policy returned an error. Every other claim
  above names its test.
