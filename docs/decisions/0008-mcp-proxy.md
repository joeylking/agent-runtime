# 8. A stdio MCP proxy, one run per call

Decision: the gate gets a first form outside Go: `agentrt-proxy`, a local MCP
proxy over stdio, in the nested module `proxy`. An MCP host starts it as its
MCP server; it connects as a client to the real servers through the `mcp`
module, which pins their tools and takes each tool's class from the operator,
and it routes every `tools/call` through a `Gate`, which validates, evaluates
policy, pauses on a hash-bound durable approval, executes the upstream call
itself, and records. Each tool call is its own run. A call that needs an
approval answers in prose that it is pending, and the model collects the
approval by calling again with the same arguments; a status tool reports what
waits. Approvals are decided only outside the host. An identical mutating
call within a window is refused. The policy is a configuration file with no
language. It is a small, deliberately scoped experiment: the Go library
remains the full form, and the proxy adds one export to the core, the
canonical JSON encoder its hashes already use.

Why: [ADR 7](0007-own-the-effect-not-the-loop.md) left open whether a loop in
another language could have the gate's controls. A design round on
2026-10-05 answered it for the commonest such loop, an MCP host: an
inventory of the code, research on how MCP hosts behave, an independent
design, and a measurement of how models react to a pending result. A proxy
needs no change in the host and no code from the operator, and everything
the runtime's strongest claims depend on stays in the runtime: the gate
executes the call, so an approval runs once and an interrupted call waits
for an operator.

**One run per call.** MCP gives a server no notion of a conversation: a host
may start one proxy per session, one per call, or several at once, and
passes no session identifier. A run that spanned calls would have to guess
which calls belong together, and it would have one step in flight at a time,
so one pending approval would block every other call, and a rejection, which
ends a run, would end everything after it. So each call is a run of its own,
named by a session from the configuration. The cost is real: the runtime's
step, failure, loop, and time limits can say nothing about a conversation,
and no policy can see what the model did before. The rules that do span calls
are the proxy's, kept in an index of its calls in the same database: a call
that matches a waiting run collects it, one that matches a running one is
told it is in progress, one whose earlier attempt was cut off waits for an
operator, and a rejected or expired request is not asked for again within
the window.

**Pending, then re-call, with a status tool.** A measurement with two small
local models over 96 runs found that after a prose pending result the models
stopped and reported, did not switch tools, and did not claim success; told
the request was approved, they re-called the same tool with identical
arguments in 48 of 48 runs. Merely asked for an update, they did not re-call:
they looked for a status tool and passed the approval id. A JSON-shaped
pending result made them poll and ask the user to approve. So the pending
answer is prose with every instruction in it, the re-call collects, and
`gate_status` reports pending, approved, rejected, and expired approvals
without executing anything. The measurement is small and of two local
models; a host or model that behaves otherwise is held by the gate all the
same, because collecting executes the recorded request and nothing else.
A call is first held for a short while, so an operator who decides quickly
answers it in the same call.

**Approval outside the host.** The model must not be handed the way to
approve its own request, so nothing it sees names the operator command, the
database, the configuration, or a run id, and the proxy offers no tool that
approves. The operator decides with `agentrt` on the same database or
through the webhook approver, and learns of a pending approval from the
proxy's stderr and `agentrt runs`.

**The duplicate rule.** A host that lost a response retries blindly, and
for a payment that is a second payment. A call to a tool that is not
read-only whose identical request executed within the window is refused,
with the earlier outcome, so the model can use it. It refuses a deliberate
repeat as well; the window is per tool and can be turned off. "Identical"
is the approval hash's rule, the runtime's canonical JSON, so what the
duplicate rule and a collection match is what an approval binds.

**Unknown outcomes wait for an operator.** A mutating call cut off by a
crash, timed out, or left without an answer from the server may have taken
effect. It is never run again on the policy alone, inside the window or
after it, and not after an operator rejected running it again: only an
operator's approval of an `interrupted_side_effect` approval naming the
earlier attempt runs it, and while one waits, other mutating calls to the
tool are blocked. An error the server returned is an answer, and is not
unknown.

**No policy language.** The policy maps side-effect classes, with a per-tool
override, to allow, require approval, or deny. Conditions on arguments and
rules over history are what the Go library is for, and the proxy keeps its
policy behind one internal interface so that an external evaluator could be
added later. Nothing more is built.

**The scope.** The proxy governs only calls that go through it. A host that
gives the model a shell, file access, or tools of its own lets the model
bypass the proxy and approve its own request with the operator command or by
editing the database. The proxy is for one person who is both the host's
user and the operator, and does not separate them. It never sees model
calls or the user's prompt. [docs/proxy.md](../proxy.md) states all of this
first, and so does the command's help.

This is not a consumer's demonstrated need, which [ADR 1](0001-custom-runtime.md)
asks for: it is an experiment on the question ADR 7 left open, built as a
nested module. Its one addition to the core is `agentrt.CanonicalJSON`,
the encoder approval hashes already use, exported so the proxy matches
requests by the hash's own rule rather than a copy of it; it adds no
abstraction, so the rule that the runtime's abstractions need a consumer is
not touched. Two helpers `examples/mcp` has were added to the `mcp` module, the
ownership check and the manifest and rules readers, because the proxy reads
the same files under the same rule. The example keeps its own copies until
an `mcp` release carries them, so it still builds against the tags it pins.

Alternatives: one run per named session, refused above: one pending approval
blocks every call, and a rejection ends the session. Holding a call open
until the operator decides, which the hold does for a few seconds only:
hosts time out tool calls, and a host that gives up has its call cancelled
while the operator is still reading. MCP elicitation, which asks the host's
user, refused because the host's user is who the model talks to, and the
point is a decision the model cannot reach; sampling and `input_required`
were refused for the same reason. A network daemon in front of several
hosts, refused because it needs authentication, a listener, and a service's
operation, which the project is not, for no gain to one person on one
machine.

Tradeoffs: there are no limits or policy over a conversation, and no model
accounting. The duplicate rule refuses deliberate repeats. Results are capped
at 64 KiB and flattened to text or structured content. The scope relies on
the host: a host with its own tools is outside it. The proxy reads the
runtime's `runs` table directly in one place, by primary key, to decide
whether a call that claimed a request ever began its run. Approval ids are
shown to the model, so it can ask about them.

Revisit when: a host passes a stable conversation identifier, which would let
a run span a conversation; a consumer needs conditions on arguments without
writing Go; or a second form, such as a different transport, is asked for.
