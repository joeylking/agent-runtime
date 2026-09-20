# 1. A small custom runtime rather than an agent framework

Decision: build a library that owns the tool loop's controls and nothing
else.

Why: the loop itself is fifty lines with any model SDK. What is not
available off the shelf, and what the first consumer needs, is policy
enforced outside the model, a durable pause with the approval bound to the
exact request, hard limits enforced by the driver, an audit log that is
written with the state, and a loop that runs against a scripted agent in
tests. Frameworks bundle those with orchestration and prompt machinery
this project does not want.

Alternatives: a Python graph framework; a thin model-SDK loop with the
controls in the application.

Tradeoffs: the runtime must be kept small on purpose; every public type is
justified by a consumer requirement, and speculative abstractions are
refused.

Revisit when: a second consumer with different needs exists, or the first
consumer stops needing the pause, policy, and limit machinery.
