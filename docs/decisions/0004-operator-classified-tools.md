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

Alternatives: trusting annotations from a server the operator trusts;
asking the model to classify the tools it is about to use; pinning the
schema and letting the description float.

Tradeoffs: an operator classifies every tool before the first run, and a
server upgrade that rewords a description stops the tools it reworded. A
tool with no rule is unavailable rather than read-only, so a forgotten
line is a missing capability, which is the failure this project prefers.

Revisit when: servers ship signed, versioned tool metadata an operator
could delegate to, or a consumer needs a server whose tool set is
discovered per run rather than reviewed before it.
