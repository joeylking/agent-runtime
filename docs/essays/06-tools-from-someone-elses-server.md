# Tools from someone else's server

The Model Context Protocol, MCP, is a standard way for a program to offer
tools to an AI model. A server, often a small program started on the same
machine or reached over HTTP, lists its tools, each with a name, a text
description, and a JSON Schema for its inputs, and then runs whichever
tool the client calls. There are servers for filesystems, databases,
issue trackers, and browsers, and installing one gives an agent a large
set of new actions at once.

agent-runtime runs one agent under a policy: ordinary code, outside the
model, that decides whether each requested action is allowed, denied, or
held for a person's approval. Its `mcp` module puts an MCP server's tools
behind that policy. This essay is about the decisions that took, recorded
in [ADR 4](../decisions/0004-operator-classified-tools.md), and about the
limit none of them removes.

## Who says what a tool does

The runtime's policy decides on a tool's kind of effect: read only, a
local change, a remote change, or destructive. So whoever supplies the
kind decides what the runtime will allow. For a tool from an MCP server
there are three candidates.

The server could say. MCP lets a server attach annotations to each tool,
such as `readOnlyHint` and `destructiveHint`. But the server is code this
project did not write, reached over a pipe or a socket, and often chosen
by whoever is being helped rather than by whoever is accountable. The MCP
specification itself says a client must never base decisions on these
annotations.

The model could say. But the tool list is input to the model, and the
model is the thing being bounded.

That leaves the operator, the person who runs the agent and whom the
audit log names. The operator writes one rule per tool naming its kind. A
tool with no rule is not registered at all, and the load report says so
([`mcp/load.go`](../../mcp/load.go),
[`TestLoad_UnclassifiedToolIsNotRegistered`](../../mcp/mcp_test.go)). A
forgotten line is a missing capability, which is the failure this project
prefers to an unexpected one.

The demo shows the shape. Its rules file classifies four read tools of the
stock filesystem server as read only and `write_file` as a local change,
and leaves every other tool the server offers unclassified, so the model
never sees them ([`examples/mcp/rules.json`](../../examples/mcp/rules.json)).

## The pin

A rule written against a tool is only meaningful while the tool stays the
one the operator reviewed. So the tool set is pinned. `Pin` connects,
lists the tools, and records for each one a hash of its description and a
hash of its input schema, with its annotations as received. The operator
reviews this manifest and stores it
([`TestPin_RecordsWhatTheServerPresents`](../../mcp/mcp_test.go)). `Load`
lists the tools again at every start and refuses any tool whose schema
or description no longer matches its pin, and only that tool
([`TestLoad_SchemaChangeRefusesOnlyThatTool`](../../mcp/mcp_test.go),
[`TestLoad_DescriptionChangeRefusesTheTool`](../../mcp/mcp_test.go)). A
pinned tool the server stopped offering is refused, and a new tool the
manifest does not pin is reported and ignored
([`TestLoad_ToolTheServerDroppedIsRefused`](../../mcp/mcp_test.go),
[`TestLoad_UnpinnedToolIsReportedAndIgnored`](../../mcp/mcp_test.go)).

Pinning the description may look fussy. It is the point. A description is
text the model reads as instructions about when and how to use the tool.
A server that rewords it between the review and the run has rewritten the
model's instructions, which is an injection route, not a cosmetic edit.
The operator may also replace a description with their own, and the model
then sees only that.

The pin is taken over the bytes the server sent, not over the MCP
library's decoding of them. That decoding holds every number as a 64-bit
float, so a schema bound moved from 9007199254740993 to 9007199254740992
would decode to the same value and hash the same. Reading the raw bytes
sees the change
([`TestPin_SeesANumberChangeBeyondFloat64`](../../mcp/listing_test.go),
[`TestCanonicalSchema_KeepsWhatFloat64WouldLose`](../../mcp/listing_test.go)).
A listing that names one tool twice is refused rather than collapsed, and
a listing is bounded in pages and tools
([`TestPin_ToolListedTwiceFails`](../../mcp/listing_test.go),
[`TestPin_ListingIsBounded`](../../mcp/listing_test.go)).

## Hints that can only refuse

The server's annotations are still recorded, because they are evidence of
a disagreement. If the server calls a tool destructive and the operator
called it a local change, one of them is wrong, and refusing until the
operator says which costs nothing. So a contradiction refuses the tool,
unless the rule explicitly allows the mismatch, and the load report
records that override
([`TestLoad_HintMismatchIsRefusedAndOverridable`](../../mcp/mcp_test.go)).
The demo does exactly this for `write_file`, whose server marks it
destructive.

The converse is never evidence. A server calling its own tool read-only
is what a hostile server would do, so a hint can dispute a classification
but never relax one. Missing annotations contradict nothing, because
their defaults are the permissive values
([`TestLoad_AbsentAnnotationsNeverMismatch`](../../mcp/mcp_test.go)).

## Narrowing what the model can send

An operator can deny a parameter or fix its value. Either way the
parameter disappears from the schema the model is shown, and a call that
supplies it anyway fails rather than being quietly rewritten, so an
approval never shows one value while the server receives another
([`TestDeny_RemovedFromTheSchemaAndRefusedAtCall`](../../mcp/tool_test.go),
[`TestFixed_ACallThatSetsAFixedParameterFails`](../../mcp/tool_test.go)).
Hiding a parameter is only sound when nothing else in the schema refers
to it, so a tool whose combinators or definitions mention a hidden name
is refused rather than partly rewritten
([`TestRules_HiddenNamesMustBeSoundToHide`](../../mcp/tool_test.go)).
A schema must also be self-contained: one that refers to a file or a URL
is refused before anything could read or fetch it
([`TestLoad_RefusesASchemaThatReachesOutsideItself`](../../mcp/security_test.go)).

Once registered, a server's tool passes the same validation, policy,
limits, approvals, and audit log as any other tool
([`TestDriver_ApprovalNamesThePrefixedTool`](../../mcp/driver_test.go)).
Its results are untrusted input to the model, an error from the server
becomes a tool error the agent must work around, and content is capped
([`TestCall_IsErrorBecomesAnError`](../../mcp/tool_test.go),
[`TestCall_OversizedContentIsTruncated`](../../mcp/tool_test.go)).

## A minimal environment, and what it does not withhold

A server started as a child process is third-party code, so it does not
inherit the operator's environment. It gets `PATH`, `HOME`, and `TMPDIR`
(and the few variables Windows needs to start a process), plus the
variables the operator names or sets for it. A token reaches only the
server it was handed to
([`mcp/transport.go`](../../mcp/transport.go),
[`TestChildEnv_IsMinimalAndExplicit`](../../mcp/transport_test.go),
[`TestStdio_ServerGetsOnlyTheEnvironmentItIsGiven`](../../mcp/transport_test.go)).
On Unix it runs in a process group of its own, and closing the connection
kills the group, descendants included
([`TestStdio_CloseKillsTheServersDescendants`](../../mcp/proc_unix_test.go)).
A server reached over HTTP has its redirects refused and each response
body capped
([`TestHTTP_RedirectIsNotFollowed`](../../mcp/security_test.go),
[`TestLimitedTransport_FailsABodyPastTheLimit`](../../mcp/transport_test.go)).

The environment withholds variables, not files. The server still runs as
the operator, so credential files under the home directory, such as
`~/.ssh` or `~/.aws`, anything else the operator's account can read, any
directory on `PATH` the operator can write, and the network all remain as
reachable to the server as to the operator
([`SECURITY.md`](../../SECURITY.md)).

## The honest limit of pinning

The pin covers what a server presents, not what it does. Every check in
this essay is about the tool list: its names, descriptions, schemas, and
hints. A server that behaves differently after `Load`, or differently from
its description, is not caught by any of them. The defence against that
is the rest of the runtime: the operator's classification, the policy
evaluated on it, the approvals bound to each request, and the audit log.

## What this does not claim

- Pinning does not verify behaviour. A tool that does something other
  than its description says passes every check.
- The tool set is fixed at `Load`. A server's notice that its tools
  changed is ignored until the next `Load`.
- Annotations and output schemas are outside the hashes. A changed
  annotation alone does not refuse a tool, although one that now
  contradicts the operator's classification does, and an output schema is
  not checked.
- Nothing sandboxes a stdio server beyond its environment.
- The kind of effect is the operator's claim about the tool. The runtime
  enforces policy on that claim and does not test it.
