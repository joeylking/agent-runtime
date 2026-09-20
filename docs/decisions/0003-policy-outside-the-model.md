# 3. The model never decides its own permissions

Decision: every tool request passes schema validation and then a policy
evaluated by the runtime with a read-only view of the run. The agent
receives outcomes as observations and has no handle to the policy.

Why: repository content, dependency sources, and command output are
untrusted input to the model. Whatever the model concludes from them, the
set of actions it can take is bounded by code the model cannot influence:
side-effect classes, protected paths, scope limits, budgets, and approvals
bound by hash to the exact request.

Alternatives: asking the model to confirm risky actions; a permissions
tool the model calls.

Tradeoffs: policy needs domain facts, so the consumer supplies them
through an interface that is testable without a model.

Revisit when: never; this is the premise of the project.
