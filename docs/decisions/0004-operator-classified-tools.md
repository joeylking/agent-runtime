# 4. Tools from someone else's server are classified by the operator

Decision: a tool reached over MCP is registered only when an operator has
named its side-effect class; the tool set is pinned by a hash over each
tool's name, description, and input schema; and the server's own
annotations may refuse a registration but never relax one.

Why: policy is evaluated on a side-effect class, so whoever supplies the
class decides what the runtime will allow. An MCP server is code this
project did not write, reached over a pipe or a socket, and often chosen by
whoever is being helped rather than by whoever is accountable; the
specification itself says a client must never base decisions on tool
annotations. The model cannot supply the class either, because the tool
list is model input and the model is the thing being bounded. That leaves
the operator, who writes one line per tool and is the person an audit log
names. Descriptions are model input for the same reason, so they are
pinned too: a server that rewords a description between the review and the
run has rewritten the instructions the model reads, which is an injection
vector rather than a cosmetic edit, and that tool is refused until the
manifest is reviewed again. An annotation is still worth recording. When
the server calls a tool destructive and the operator called it a local
mutation, one of them is wrong, and refusing until the operator says which
costs nothing; the converse is not evidence, because a server calling its
own tool read-only is exactly what a hostile one would do.

The pin is taken over the bytes the server sent, not over the SDK's
decoding of them, because that decoding holds every number as a float64 and
a bound moved from 9007199254740993 to 9007199254740992 would hash the
same. A number float64 holds without loss is written as before, so a
manifest pinned before this change still loads. A listing that names a tool
twice is refused rather than collapsed to one entry, and a listing is
bounded at MaxToolPages pages and MaxTools tools.

The operator's Deny and Fixed rules remove a parameter from what the model
sees, and that is only sound when nothing else in the schema refers to it.
So a hidden name must be a top-level property, and a tool whose allOf,
anyOf, oneOf, not, if/then/else, dependentRequired, dependentSchemas,
dependencies, or $defs mentions one, or that has patternProperties at all,
is refused rather than partly rewritten. A call that sets a Fixed
parameter fails as a denied one does: the fixed value never silently
replaces the model's, which would let an approval show one value while the
server receives another.

A stdio server is third-party code, so it does not inherit the operator's
environment. It gets PATH, HOME, and TMPDIR (and the few variables Windows
needs to start a process), plus the variables the operator names or sets;
a token reaches only the server it was handed to. A streamable HTTP
server's responses are capped per body, and the client is never the shared
http.DefaultClient.

What this does not protect: the pin covers what a server presents, not
what it does, so a server that behaves differently after Load, or
differently from its description, is not caught. The tool set is fixed at
Load and tools/list_changed notifications are ignored until the next Load.
Annotations and output schemas are outside the hashes: an annotation can
refuse a registration but its change alone does not, and an output schema
is not checked. Nothing sandboxes a stdio server beyond its environment.

Alternatives: trusting annotations from a server the operator trusts;
asking the model to classify the tools it is about to use; pinning the
schema and letting the description float.

Tradeoffs: an operator classifies every tool before the first run, and a
server upgrade that rewords a description stops the tools it reworded. A
tool with no rule is unavailable rather than read-only, so a forgotten
line is a missing capability, which is the failure this project prefers.
The same preference sets the other defaults: a server that needs a
variable fails until the operator names it, a tool whose schema cannot be
restricted soundly is refused rather than approximated, and a manifest
pinned from a schema holding a number float64 could not represent is
refused once, at the first Load after the pin began reading raw bytes, and
pinned again.

Revisit when: servers ship signed, versioned tool metadata an operator
could delegate to, or a consumer needs a server whose tool set is
discovered per run rather than reviewed before it.
