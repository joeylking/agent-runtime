# Implementation status

Status values: **Required** (planned, not implemented), **Implemented** (code
exists, reference given), **Verified** (a named test exercises it).

## Milestones 0 and 1

| Capability | Status | Reference |
|---|---|---|
| Run, Step, Decision, Observation, ToolSpec, PolicyDecision, Event types | Implemented | `agentrt.go` |
| SQLite store with migrations, runs, steps, events | Verified | `store.go`, `TestStore_ReopenFileBacked` |
| Events committed in the same transaction as state changes, delivered to the observer after commit | Verified | `store.go` `txn`, `TestDriver_CompletesAndPersists` |
| Step loop: decide, validate, policy, execute, observe | Verified | `driver.go`, `TestDriver_CompletesAndPersists` |
| Decision recorded verbatim before validation | Verified | `TestDriver_InvalidArgumentsBecomeObservation` |
| Tool argument schema validation before policy | Verified | `schema.go`, `TestDriver_InvalidArgumentsBecomeObservation` |
| Tool registration rejects missing schema, timeout, or side effect | Verified | `TestNewDriver_RejectsBadTools` |
| Side-effect policy: allow, deny, abort, require_approval | Verified | `policy.go`, `TestDriver_PolicyDenyIsObservation`, `TestDriver_PolicyAbortEndsRun`, `TestDriver_RequireApprovalPausesRun` |
| Pause in WAITING_FOR_APPROVAL with a hash-bound approval row | Verified | `TestDriver_RequireApprovalPausesRun`, `TestApproval_RecordedWithHash` |
| Approve, then Resume executes exactly the recorded request and continues the loop | Verified | `TestApproval_ApproveThenResumeExecutesRecordedRequest` |
| Reject cancels the run with `approval_rejected` | Verified | `TestApproval_RejectCancelsRun` |
| A tampered approval row is refused by hash | Verified | `TestApproval_TamperedRowRefused` |
| Policy re-evaluated on resume; a different required approval pauses again | Verified | `TestApproval_ResumeRepausesWhenPolicyWantsDifferentApproval` |
| Terminal tools complete the run with their result | Verified | `TestDriver_TerminalToolCompletesRun` |
| `ErrAbortRun` ends the run with `tool_abort` | Verified | `TestDriver_ToolAbortEndsRun` |
| Resume of a run found mid-step: step marked interrupted, consumer reconciliation runs, side effects never re-executed | Verified | `TestResume_InterruptedStepIsReconciledAndContinued` |
| Model interface with content blocks, tool uses, usage; the agent's only handle is the accounting wrapper | Verified | `model.go`, `TestModel_UsageAndCostRecorded` |
| Every attempt reserved as a row before dispatch; usage, latency, cost recorded; run totals maintained | Verified | `TestModel_UsageAndCostRecorded`, `TestModel_TransientRetryThenSuccess` |
| Call limit enforced before dispatch; token and cost limits enforced on totals plus a conservative projection | Verified | `TestModel_CallLimitStopsBeforeDispatch`, `TestModel_TokenAndCostLimitsProjected` |
| Transient errors retried with backoff, each attempt counted; non-transient errors not retried; exhaustion ends the run as model_unavailable | Verified | `TestModel_TransientRetryThenSuccess`, `TestModel_ExhaustedRetriesIsUnavailable` |
| Ambiguous attempts (timeout, connection lost) counted and charged at the conservative estimate | Verified | `TestModel_AmbiguousAttemptChargedConservatively` |
| Record and replay models keyed by canonical request and model name; unrecorded requests fail | Verified | `replay`, `TestReplay_RecordThenReplay` |
| Active time limit sums step durations and excludes approval waits; a resumed step's clock restarts | Verified | `TestLimits_ActiveTimeExcludesApprovalWait`, `TestLimits_ActiveTimeStopsRun` |
| Elapsed time limit is an absolute deadline including waits | Verified | `TestLimits_ElapsedTimeIncludesWaits` |
| Approval expiry cancels the run on approve or resume | Verified | `TestApproval_ExpiryCancelsRun` |
| Structured reconciliation outcomes | Verified | `TestResume_ReconcileOutcomes` |
| Step limit | Verified | `TestDriver_StepLimit` |
| Consecutive failure limit, reset on success | Verified | `TestDriver_ConsecutiveFailuresEndRun`, `TestDriver_FailureStreakResetsOnSuccess` |
| Loop detection on (tool, arguments, observation); changing results are progress | Verified | `TestDriver_IdenticalCallAndResultStops`, `TestDriver_RepeatedCallWithChangingResultAllowed`, `TestDriver_LoopStreakResets` |
| Tool timeout and panic become observations | Verified | `TestDriver_ToolTimeout`, `TestDriver_ToolPanicIsObserved` |
| Driver-supplied run and step identity passed to tools | Verified | `TestDriver_CompletesAndPersists` |
| Observation content hash for later no-progress detection | Implemented | `hash.go` |
| File-backed store survives close and reopen | Verified | `TestStore_ReopenFileBacked` |

## Required for later milestones

| Capability | Milestone |
|---|---|
| Approval expiry | 3 |
| Active and elapsed time limits | 3 |
| Structured reconciliation outcomes (completed, continue, waiting, conflict) for publication recovery | 3 |
| Model interface, wrapper with usage, cost, retries, ambiguous accounting, token and cost limits | 2 |
| Active and elapsed time limits | 2 |
| Blob table for large tool outputs | 1 |
| OpenTelemetry exporter, MCP adapter | not planned for MVP |
