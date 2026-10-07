# Implementation status

Status values: **Required** (planned, not implemented), **Implemented** (code
exists, reference given), **Verified** (a named test exercises it).

## Milestones 0 and 1

| Capability | Status | Reference |
|---|---|---|
| Run, Step, Decision, Observation, ToolSpec, PolicyDecision, Event types | Implemented | `agentrt.go` |
| SQLite store with migrations, runs, steps, events | Verified | `store.go`, `TestStore_ReopenFileBacked` |
| Events committed in the same transaction as state changes, delivered to the observer after commit | Verified | `store.go` `txn`, `TestDriver_CompletesAndPersists` |
| Step loop: decide, validate, policy, execute, observe | Verified | `loop.go`, `TestDriver_CompletesAndPersists` |
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
| Resume of a run found mid-step: step marked interrupted, consumer reconciliation runs, side effects re-executed only when reconciliation or an operator says so | Verified | `TestResume_InterruptedStepIsReconciledAndContinued`, `TestResume_InterruptedSideEffectWaitsForAnOperator` |
| Model interface with content blocks, tool uses, usage; the agent's only handle is the accounting wrapper | Verified | `model.go`, `TestModel_UsageAndCostRecorded` |
| Every attempt reserved as a row before dispatch; usage, latency, cost recorded; run totals maintained | Verified | `TestModel_UsageAndCostRecorded`, `TestModel_TransientRetryThenSuccess` |
| Call limit enforced before dispatch; token and cost limits enforced on totals plus a conservative projection | Verified | `TestModel_CallLimitStopsBeforeDispatch`, `TestModel_TokenAndCostLimitsProjected` |
| Transient errors retried with backoff, each attempt counted; non-transient errors not retried; exhaustion ends the run as model_unavailable | Verified | `TestModel_TransientRetryThenSuccess`, `TestModel_ExhaustedRetriesIsUnavailable` |
| Ambiguous attempts (timeout, connection lost) counted and charged at the conservative estimate | Verified | `TestModel_AmbiguousAttemptChargedConservatively` |
| Record and replay models keyed by canonical request and model name; unrecorded requests fail | Verified | `replay`, `TestReplay_RecordThenReplay` |
| Recorded provider raw bytes replay byte-identical: no HTML escaping, no re-indentation | Verified | `TestReplay_RawBytesRoundTrip` |
| Identical requests are recorded in sequence and replayed in order; more requests than recorded is unrecorded; re-recording replaces the whole sequence | Verified | `TestReplay_IdenticalRequestsKeepTheirOrder` |
| Active time limit sums step durations and excludes approval waits; a resumed step's clock restarts | Verified | `TestLimits_ActiveTimeExcludesApprovalWait`, `TestLimits_ActiveTimeStopsRun` |
| Elapsed time limit is an absolute deadline including waits | Verified | `TestLimits_ElapsedTimeIncludesWaits` |
| Approval expiry cancels the run on approve or resume | Verified | `TestApproval_ExpiryCancelsRun` |
| Operator cancel ends a non-terminal run as `operator_cancelled`, fails the in-flight step, refuses terminal runs; the only way to close a run whose approval was granted but never resumed | Verified | `TestCancel_ApprovedButUnresumedRun` |
| Structured reconciliation outcomes | Verified | `TestResume_ReconcileOutcomes` |
| Step limit | Verified | `TestDriver_StepLimit` |
| Consecutive failure limit, reset on success | Verified | `TestDriver_ConsecutiveFailuresEndRun`, `TestDriver_FailureStreakResetsOnSuccess` |
| Loop detection on (tool, arguments, observation); changing results are progress | Verified | `TestDriver_IdenticalCallAndResultStops`, `TestDriver_RepeatedCallWithChangingResultAllowed`, `TestDriver_LoopStreakResets` |
| Tool timeout and panic become observations | Verified | `TestDriver_ToolTimeout`, `TestDriver_ToolPanicIsObserved` |
| Driver-supplied run and step identity passed to tools | Verified | `TestDriver_CompletesAndPersists` |
| Observation content hash for later no-progress detection | Implemented | `hash.go` |
| File-backed store survives close and reopen | Verified | `TestStore_ReopenFileBacked` |

## Item 1: what both consumers rebuilt

| Capability | Status | Reference |
|---|---|---|
| Transport classification: a timeout or a body cut off mid-read is returned bare so the caller charges it as ambiguous; caller cancellation, a host that does not resolve, a certificate failure, and a malformed URL are permanent; every other transport error is transient | Verified | `providers`, `TestClassifyTransport_EveryClass`, `TestClassifyTransport_RealRoundTrips` |
| HTTP status classification: 408, 409, 425, 429, 529, and every other 5xx are transient, other non-2xx permanent, both carrying the status and a bounded body excerpt | Verified | `TestClassifyStatus_RetryableAndPermanent`, `TestClassifyStatus_BodyExcerptIsBounded` |
| An SDK adapter with only a status code and an error classifies the same way | Verified | `TestClassifyAPIError_StatusOnlyAdapter` |
| Provider response bodies are read under a 16 MiB bound, and a larger one is refused by name | Verified | `TestReadBody_RefusesABodyOverTheLimit`, `TestClient_LimitsBodiesAndNeverSharesTheDefault` |
| Tool-use ids synthesized for providers that omit them | Verified | `TestSynthesizeToolUseID_UniquePerIndex` |
| Price lookup refuses an unpriced paid model; a local model is recorded as free, so free and unpriced stay distinguishable | Verified | `providers/prices.go`, `TestPriceFor_UnpricedModelRefused`, `TestFree_DistinguishesFreeFromUnpriced`, `TestMerge_LaterTableWins` |
| Budgets parsed from `5`, `4.41`, `$0.50` | Verified | `TestParseDollars_AcceptedForms` |
| Steps rendered to messages with synthesized tool-use ids, a recent-results window, and a content cap | Verified | `render`, `TestMessages_WholeSequence`, `TestMessages_RecentWindowAndContentCap`, `TestMessages_SkipsTheStepBeingDecided` |
| Opening, tool-use id, assistant, and observation hooks reproduce both consumers' renderers | Verified | `TestMessages_HooksOverrideAndFallBack` |
| A step that asked for nothing executable is answered with a nudge keyed on its decision kind; an interrupted result is re-requested rather than assumed | Verified | `TestMessages_DeniedAndTruncatedNudges`, `TestMessages_InterruptedResultAsksAgain` |
| Model response to decision: a refusal fails the run, a truncated or tool-less reply becomes a pseudo kind, the reason is truncated | Verified | `render.Decide`, `TestDecide_StopReasonsAndToolCall`, `TestTruncate_MarksTheCut` |
| The pseudo kinds are invalid decisions: the driver records `invalid_decision` and no partial tool call runs | Verified | `TestDriver_PseudoDecisionKindsAreInvalid` |
| Aligned and JSON Lines trace observers, both safe for concurrent events | Verified | `trace`, `TestWriter_AlignedLine`, `TestJSONL_OneObjectPerEvent`, `TestTrace_ConcurrentEventsStayWhole` |
| Run and step read models over a store, including the model call totals | Verified | `view`, `TestRuns_SummariesCarryTheWaitingApproval`, `TestSummary_TerminalRunHasNoPendingApproval`, `TestSteps_SummarizeDecisionPolicyAndObservation` |
| Pending-approval selection: a named approval must be pending, otherwise exactly one candidate, and an ambiguous run names its candidates | Verified | `TestPendingApproval_SelectionRule` |
| `require_approval` decision constructor and typed presentation decoder | Verified | `policy.go`, `TestNeedApproval_PausesAndBindsWhatWasShown`, `TestNeedApproval_RefusesUnmarshalableValues`, `TestDecodePresentation_ReportsWhatIsWrong` |
| Operator command over a database: `runs`, `show`, `events`, `approve`, `reject`, `cancel`, in text and JSON, exiting 0, 1, or 2 | Verified | `cmd/agentrt`, `TestRuns_ListsAndEmitsJSON`, `TestRuns_ReadsTheDatabaseFromTheEnvironment`, `TestShow_PutsTheWaitingApprovalInFront`, `TestEvents_BothTraceFormats`, `TestApprove_RecordsTheDecisionAndTraces`, `TestApprove_DefaultsWhoToTheEnvironment`, `TestReject_CancelsTheRun`, `TestCancel_EndsTheRunAndRefusesATerminalOne`, `TestUsage_ExitCodes` |
| Resume is absent from the command and documented as the consumer's, because it needs the consumer's agent, tools, and policy | Verified | `TestHelp_SaysResumeBelongsToTheConsumer` |
| Every provider adapter is a nested module with its own `go.mod`, so a provider SDK never taints the core | Implemented | `providers/ollama`, `providers/openai`, `providers/anthropic` |
| Shared adapter contract: a `Config` with the model id, a name defaulting to `<provider>:<model>` and overridable, and an HTTP client override | Verified | `TestNew_RequiresAModelAndNamesItself` in all three adapters |
| Ollama: native chat with tool calling, system message, temperature 0, `num_ctx`, `num_predict` from the output cap, and `tool_name` on tool messages | Verified | `providers/ollama`, `TestGenerate_MapsRequestAndToolCalls` |
| Ollama host from the config, `OLLAMA_HOST`, or the local default, with a scheme prefixed | Verified | `TestHost_DefaultsAndPrefixesTheScheme` |
| Ollama: missing tool-use ids synthesized, empty arguments become `{}` | Verified | `TestGenerate_TextOnlyAndMissingIDs` |
| Ollama stop reasons mapped to the runtime vocabulary; a truncated reply carries no tool use and is still charged | Verified | `TestGenerate_StopReasonsMapToTheRuntimeVocabulary` |
| Ollama errors classified: 5xx transient, other non-2xx permanent with the status, a refusal in a 200 body an error, a server that is down transient | Verified | `providers/ollama`, `TestGenerate_ErrorsClassified` |
| OpenAI-compatible: system message, assistant `tool_calls`, role `tool` results by `tool_call_id`, function definitions from the input schema, `tool_choice` auto, parallel calls off, temperature 0, `max_tokens` | Verified | `providers/openai`, `TestGenerate_MapsRequestToolCallsAndUsage`, `TestGenerate_NoToolsSendsNoToolChoice` |
| OpenAI-compatible usage from `prompt_tokens`, `completion_tokens`, and cached prompt tokens | Verified | `TestGenerate_MapsRequestToolCallsAndUsage` |
| `finish_reason` mapped to the runtime vocabulary; a cut-off or filtered reply carries no tool use and is still charged | Verified | `TestGenerate_FinishReasonsMapAndDropPartialCalls` |
| Tool call arguments arrive as a string and are validated as JSON before becoming arguments | Verified | `TestGenerate_ArgumentsMustBeJSON` |
| A key is optional, because a local server needs none | Verified | `TestGenerate_NoKeySendsNoAuthorization` |
| OpenAI-compatible errors classified: 429 transient, other non-2xx permanent with the status, an error in a 200 body an error, no choice an error | Verified | `providers/openai`, `TestGenerate_ErrorsClassified` |
| Anthropic through the official SDK with the SDK's own retries off, so the accounting caller sees every attempt | Verified | `providers/anthropic`, `TestGenerate_ErrorsAreClassifiedAndNotRetriedBySDK` |
| Anthropic tool use, usage, raw bytes, one call per turn, effort, and the default output cap | Verified | `TestGenerate_MapsToolUseAndUsage`, `TestGenerate_WithoutStrictSendsTheWholeSchema` |
| Opt-in strict schema subset: keywords strict tool use rejects are removed and the runtime still validates against the full schema | Verified | `TestStrictSubset_RemovesRejectedKeywords`, `TestGenerate_MapsToolUseAndUsage` |
| A raw assistant turn is replayed from the provider's own message, so signed thinking blocks survive a continuation; the block type is configurable | Verified | `TestGenerate_RawAssistantTurnReplaysThinkingBlocks`, `TestGenerate_RawBlockTypeIsTheConfiguredOne` |
| A synthesized turn sends an empty tool input as `{}`, drops empty text, and never sends an empty message | Verified | `TestGenerate_SynthesizedTurnWithoutRaw` |
| A refused or truncated Claude turn keeps its usage and loses its tool uses | Verified | `TestGenerate_RefusalAndMaxTokensKeepUsageDropToolUse` |
| Cache reads are reported as cached input; cache writes, which have no counter, are folded into input at the multiple they are billed at: 1.25 for five-minute writes and 2 for one-hour writes | Verified | `TestGenerate_FoldsCacheUsage`, `TestGenerate_FoldsOneHourCacheWritesAtTwice` |
| Dated Claude price table keyed by the default prefixed name; local models recorded as free under theirs | Verified | `TestPrices_KeyedByTheDefaultName`, `TestFree_PricesTheReportedName` in both local adapters |
| No test reaches a provider: the Anthropic adapter runs against an `httptest` fake, and the two live tests skip unless a local server answers and are excluded from CI by the `live` tag | Verified | `providers/anthropic/anthropic_test.go`, `TestLive_ToolUseRoundTrip` in `providers/ollama` and `providers/openai` |
| A consumer reaches a live run with no code beyond an agent, its tools, and a policy, and an approval stops the run before a change | Implemented | `examples/live` |

## Item 2: MCP tool adapter

| Capability | Status | Reference |
|---|---|---|
| An MCP server's tools exposed as `agentrt.Tool`s from a nested module, so the MCP SDK never reaches the core | Implemented | `mcp`, `mcp/go.mod` |
| stdio (`Command`) or streamable HTTP (`Endpoint`); one session shared by every tool from a server, closed by the consumer, with no reconnect | Implemented | `mcp.Server`, `mcp.Connection` |
| A tool with no operator rule naming its side-effect class is not registered and is reported; nothing registered is an error and leaves no session open | Verified | `TestLoad_UnclassifiedToolIsNotRegistered` |
| The tool set is pinned: name, description, and input schema hashed over canonical JSON, annotations recorded as received | Verified | `Pin`, `TestPin_RecordsWhatTheServerPresents` |
| A changed input schema refuses that tool and only that tool | Verified | `TestLoad_SchemaChangeRefusesOnlyThatTool` |
| A changed description refuses the tool, because a description is model input and a changed one is an injection vector | Verified | `TestLoad_DescriptionChangeRefusesTheTool` |
| A pinned tool the server no longer offers is refused; a tool it offers that the manifest does not pin is reported and ignored | Verified | `TestLoad_ToolTheServerDroppedIsRefused`, `TestLoad_UnpinnedToolIsReportedAndIgnored` |
| Annotations can only contradict a classification, never relax it; a contradiction refuses the tool unless the rule allows the mismatch, which the report records | Verified | `TestLoad_HintMismatchIsRefusedAndOverridable` |
| Absent annotations never contradict anything, because their defaults are the permissive ones | Verified | `TestLoad_AbsentAnnotationsNeverMismatch` |
| The model sees `<server>_<tool>` unless the rule renames it, and a name no provider would accept is refused | Verified | `TestLoad_RegistersAPinnedToolUnderThePrefixedName`, `TestLoad_RenameReplacesThePrefixAndAnUnusableNameIsRefused` |
| An operator description replaces the server's in what the model sees | Verified | `TestLoad_RegistersAPinnedToolUnderThePrefixedName` |
| A denied parameter is removed from the schema the model sees and a call carrying it anyway fails | Verified | `TestDeny_RemovedFromTheSchemaAndRefusedAtCall` |
| A fixed parameter is removed from the schema and injected after the runtime validated the model's arguments; operator configuration sits outside the approval hash | Verified | `TestFixed_RemovedFromTheSchemaAndInjectedAtCall` |
| A schema the runtime's own validator refuses is refused at load rather than at the first call | Verified | `TestLoad_SchemaTheRuntimeRejectsIsRefused`, `TestObjectSchema_RefusesAnythingElse` |
| A rule with no timeout and no server default, or no side-effect class, is refused | Verified | `TestLoad_RuleWithoutATimeoutOrSideEffectIsRefused` |
| A manifest from another server, and a rule naming a tool the manifest does not pin, are errors | Verified | `TestLoad_RefusesAManifestAndRulesThatDoNotFit` |
| `structuredContent` becomes the result content; otherwise the text blocks joined, with a placeholder for a block that is not text | Verified | `TestCall_StructuredAndTextResults` |
| `isError` becomes a tool error, so the driver records `tool_error` and it counts toward the failure limit; a server failure never aborts the run | Verified | `TestCall_IsErrorBecomesAnError` |
| Content capped at `MaxContentBytes` with a truncation marker and a summary that says so | Verified | `TestCall_OversizedContentIsTruncated` |
| A result that asks for more input is refused: the `input_required` round trip is out of scope | Verified | `TestCall_InputRequiredIsRefused` |
| The rule's timeout bounds one call | Verified | `TestCall_TimeoutIsHonoured` |
| A whole run over a server tool: the policy pauses before a local mutation, the approval names the prefixed tool, and resume executes exactly that call | Verified | `TestDriver_ApprovalNamesThePrefixedTool` |
| No test reaches a server: the fake runs in this process over the SDK's in-memory transports, with no subprocess and no port | Verified | `mcp/mcp_test.go` |
| A local model against the stock MCP filesystem server: the manifest pinned and the server's hints printed for the operator, four read tools registered, `write_file` a local mutation over the server's destructive hint, nine tools left unclassified and unavailable, the run paused before the write, approved with `cmd/agentrt`, resumed, and completed | Implemented | `examples/mcp`, `examples/mcp/rules.json` |
| Why the side-effect class is the operator's, the pin covers the description, and a hint may refuse but never relax | Implemented | `docs/decisions/0004-operator-classified-tools.md` |

## Item 8: open-source hygiene

| Capability | Status | Reference |
|---|---|---|
| How the project decides what to add, how to propose it, the module layout and the workspace, every suite including the live-tagged tests and the examples, the recording convention, the style rules, and the commit shape; the maintainer merges and there is no CLA | Implemented | `CONTRIBUTING.md` |
| What the runtime bounds and what it does not: untrusted content reaches the model and the action set is bounded by code, the database is trusted as the operator's filesystem is, no sandbox, no secret redaction, no multi-tenancy; private vulnerability reporting and the supported versions | Implemented | `SECURITY.md` |
| Issue forms for a bug report (version, module, consumer, event trail) and a consumer need (who, what it built, why here, which roadmap item); blank issues off and security reports redirected to the private channel | Implemented | `.github/ISSUE_TEMPLATE` |
| A denied destructive tool, an approval pause with Approve and Resume, and `NeedApproval` with `DecodePresentation`, as examples `go test` verifies against their printed output | Verified | `ExampleDefaultPolicy`, `ExampleDriver_Resume`, `ExampleNeedApproval` |
| A rendered conversation over two recorded steps, and a tool-less reply becoming an invalid decision | Verified | `ExampleMessages`, `ExampleDecide` |
| An unpriced model refused by `errors.Is`, and budgets parsed from dollars | Verified | `ExamplePriceFor`, `ExampleParseDollars` |
| The exactly-one pending-approval rule | Verified | `ExamplePendingApproval` |
| A pinned MCP tool with no operator rule left unregistered, over in-memory transports with no server | Verified | `ExampleLoad` |
| Pre-1.0 compatibility promise: additive within a minor, release notes name what a minor bump changes, nested modules versioned independently and each naming its core version, forward-only migrations, recordings stable across core versions | Implemented | `README.md`, `docs/roadmap.md` |
| The core module builds and tests on Go 1.26 and 1.27, with `GOTOOLCHAIN=local` so the older job cannot switch toolchains. Every nested module's `go` line is 1.26, and on `main` each requires the core release v0.4.0 | Implemented | `.github/workflows/ci.yml`, the comment above each nested `go` directive |
| Public roadmap issue, pinned, mirroring the item list with a status per item | Implemented | [#3](https://github.com/joeylking/agent-runtime/issues/3) |
| CI lint job: gofmt, staticcheck and govulncheck at pinned versions, and a core `go mod tidy -diff` check | Implemented | `.github/workflows/ci.yml` |
| Release check builds each nested module against the versions it pins, with no workspace, on every tag and weekly | Implemented | `.github/workflows/release-check.yml` |
| Dependabot: weekly, grouped, for actions and for each of the seven modules | Implemented | `.github/dependabot.yml` |
| Changelog, with Unreleased becoming the release's section | Implemented | `CHANGELOG.md` |
| Release procedure: tag after green CI and the release check; a pushed tag is never moved | Implemented | `CONTRIBUTING.md` |

## Core audit fixes

| Capability | Status | Reference |
|---|---|---|
| Run and step status transitions are compare-and-set inside their transaction; of two concurrent resumes one executes the approved request and the other gets `ErrRunState` | Verified | `store.go` `transition`, `TestResume_ConcurrentResumeExecutesOnce` |
| An operator cancel is not overwritten by a live loop: its next write fails and it returns the cancelled run | Verified | `TestCancel_LiveLoopStopsAtItsNextWrite` |
| One policy-outcome dispatch for the loop and resume; an unknown or empty outcome fails the run as `internal_error` in both | Verified | `loop.go` `apply`, `TestPolicy_UnknownOutcomeFailsLoopAndResume` |
| Resume evaluates policy while the run is still WAITING and commits the resume, the resume-time `step.policy`, and the tool start together | Verified | `TestResume_CrashAfterResumeKeepsTheApprovedRequest` |
| A step and the run it ends commit in one transaction: complete, fail, terminal tool, tool abort, policy abort, limits, loop detection | Verified | `step.go` `settle`, `TestDriver_TerminalToolCommitsWithTheRun` |
| Every path reads the clock as often and in the order it always has, because a deterministic consumer keys its own records on that sequence | Verified | `TestDriver_ClockReadingsAreStable` |
| A panic inside a transaction rolls it back and propagates | Verified | `TestStore_PanicInTransactionRollsBack` |
| Consumer JSON is checked where it enters: arguments or a result that are not usable JSON (malformed, invalid UTF-8, a repeated key) become an `invalid_decision` with the bytes kept as a string; tool content becomes a `tool_error` keeping the bytes; an unusable capability or presentation fails the run as `internal_error` without executing | Verified | `hash.go` `checkJSON`, `TestCheckJSON_RejectsWhatWouldHashLossily`, `TestDriver_UnusableJSONDecisionsAreRecordedInvalid`, `TestDriver_UnusableToolContentIsToolError`, `TestDriver_UnusablePolicyJSONFailsWithoutExecuting` |
| Active time is maintained only by increments and survives the finish | Verified | `TestLimits_ActiveTimeSurvivesTheFinish` |
| A tool's outcome and a dispatched model call are recorded even when the caller's context is cancelled, and the cancellation is then returned; a tool that fails because of the cancellation is observed as interrupted | Verified | `TestDriver_CancelledContextAfterToolKeepsTheObservation`, `TestModel_CancelledContextStillRecordsTheCall` |
| `ReconcileWaiting` pauses the interrupted tool call on `Reconciliation.Pause` through the ordinary pause; without one, or with nothing to pause, the run fails as `reconcile_conflict`; an unknown outcome or unusable result fails it as `internal_error` | Verified | `TestResume_ReconcileWaitingPausesTheInterruptedRequest`, `TestResume_ReconcileOutcomes` |
| Migrations are read and applied in one IMMEDIATE transaction, the WAL switch is retried, and a database from a newer build is refused with `ErrSchemaVersion` | Verified | `TestStore_ConcurrentFirstOpen`, `TestStore_NewerSchemaRefused` |
| Lists and the choice of granted approval follow insertion order, not timestamp text | Verified | `TestStore_ListsInInsertionOrder`, `TestResume_GrantedApprovalIsTheLatestInserted` |
| A path containing `?`, `#`, or `%` opens the file it names; transactions begin IMMEDIATE, so a consumer's read-then-write transaction on `DB()` waits for another writer | Verified | `TestStore_PathWithURICharacters`, `TestStore_ReadThenWriteTransactionWaitsForTheWriter` |
| `OpenExisting` never creates a file; a read-only store reads a WAL database with a live writer and in a read-only directory without changing the file | Verified | `TestOpenExisting_ReadOnlyNeverWrites` |
| A `ServedError` is charged at the provider's usage, recorded as an error, and not retried; connection loss is matched by error, not by text | Verified | `TestModel_ServedErrorIsChargedAndNotRetried`, `TestIsConnectionLost_MatchesErrorsNotText` |
| Concurrent calls in one step reserve against the limits before dispatch | Verified | `TestModel_ConcurrentCallsReserveAgainstTheLimits` |
| Under a token or cost limit a request with no output cap is refused before dispatch | Verified | `TestModel_UncappedRequestRefusedUnderAProjectedLimit` |
| Driver `Approve`, `Reject`, `Cancel`, and expiry use the driver's clock; the package functions use the wall clock | Verified | `TestApproval_DriverClockDecidesExpiry` |
| Operator paths fail with `ErrRunState` or `ErrNotPending` | Verified | `TestErrors_OperatorPathsAreTyped` |
| The canonical JSON encoder is written out and byte-identical to what the stored hashes were computed with | Verified | `TestCanonicalJSON_MatchesEncodingJSON`, `TestContentHash_Golden` |
| An escaped surrogate outside a pair, which decodes as U+FFFD, is refused where consumer JSON enters, like a repeated key | Verified | `hash.go` `checkSurrogates`, `TestCheckJSON_RejectsWhatWouldHashLossily`, `TestDriver_UnusableJSONDecisionsAreRecordedInvalid` |
| A number longer than 10,000 characters or with an exponent beyond ±10,000, which made the schema validator panic, is refused where consumer JSON enters, through the `Driver`, a `Gate`, and `CheckArgs`, and in a tool's input schema at registration; a validator panic is a refusal, never an acceptance | Verified | `hash.go` `checkNumbers`, `schema.go` `validateDoc`, `TestCheckJSON_RefusesNumbersPastTheBound`, `TestDriver_HugeNumbersAreRefusedWhereTheyEnter`, `TestGate_HugeNumberIsAnInvalidDecision`, `TestCheckArgs_RefusesNumbersPastTheBound`, `TestValidate_APanicInTheLibraryIsARefusal` |
| Approval JSON omits an unset `expires_at` and `decided_at` instead of carrying year one | Verified | `TestApproval_JSONOmitsZeroTimes` |
| A retry waits at least the provider's `RetryAfter` and at least the backoff; a wait beyond `MaxRetryAfter`, past the context's deadline, or reaching the elapsed or active time limit ends the run as `model_unavailable` without sleeping | Verified | `TestModel_RetryAfterIsHonoured`, `TestModel_RetryWaitThatCannotFitIsUnavailable` |
| Replay keys keep numbers' literal text, so large integers do not collide; every existing recording keeps its key | Verified | `TestKey_LargeIntegersDoNotCollide`, `TestKey_Pinned` |
| `render.Decide` caps the reason on every branch, fails on a context-window overflow, and names the tool calls it did not execute; the stop-reason vocabulary is exported | Verified | `TestDecide_NoToolCallReasonIsCapped`, `TestDecide_ContextWindowExceededFails`, `TestDecide_ExtraToolUsesAreNamed` |
| The recent-results window counts rendered steps; `DefaultObservation` accepts a step without a decision; `Truncate` never exceeds n bytes | Verified | `TestMessages_RecentWindowCountsRenderedSteps`, `TestDefaultObservation_StepWithoutADecisionIsNudged`, `TestTruncate_NeverExceedsN` |

## Run lease

See ADR 5.

| Capability | Status | Reference |
|---|---|---|
| A RUNNING run carries a lease, taken with the move to RUNNING and released with the pause or finish, stored by migration 5 | Verified | `lease.go`, `store.go` `transition`, `TestLease_ResumeOfALiveRunIsRefused` |
| Resume of a run leased live by another process returns `ErrRunLeased` with the owner and expiry, reads no clock, and changes nothing | Verified | `TestLease_ResumeOfALiveRunIsRefused` |
| After the owner dies and its lease expires, Resume takes the run over, records `lease.taken_over`, and reconciles the interrupted step once; the dead owner's late tool outcome is not recorded | Verified | `TestLease_ExpiredLeaseIsTakenOverAndReconciledOnce` |
| A loop that loses its lease stops before its next tool call or model request and returns `ErrLeaseLost`; the step in flight is left for the next owner | Verified | `TestLease_LoopThatLosesItsLeaseStopsBeforeTheNextToolCall`, `TestLease_ExpiredLeaseStopsTheLoopBeforeItsNextSideEffect`, `TestLease_NoModelCallAfterTheLeaseIsLost` |
| The heartbeat renews the lease through a tool call several TTLs long, and stops when the call returns or panics; a panicking call releases its lease | Verified | `TestLease_LongToolCallKeepsTheLease`, `TestLease_HeartbeatStopsOnReturnAndOnPanic` |
| An operator's Cancel needs no lease and releases it | Verified | `TestLease_CancelNeedsNoLease`, `TestCancel_LiveLoopStopsAtItsNextWrite` |
| A v0.2.1 database migrates with its runs intact; its RUNNING run gets a lease one `DefaultLeaseTTL` long under a placeholder owner, is refused until that passes, and is then taken over with a takeover event; `OpenExisting` refuses an older or newer schema; the released migrations are pinned | Verified | `TestStore_V021DatabaseMigratesWithItsRuns`, `TestOpenExisting_RefusesEitherOtherVersion`, `TestStore_ReleasedMigrationsAreUnchanged` |
| The ordinary path's events and clock readings are unchanged | Verified | `TestDriver_CachedViewEqualsReloadedView`, `TestDriver_ClockReadingsAreStable` |

## Security sweep

Every fix in the security sweep released in v0.3.0, core and
modules, with the test that verifies it. See `SECURITY.md` and ADR 6.

| Capability | Status | Reference |
|---|---|---|
| Consumer JSON nested deeper than 256 levels is refused where it enters: arguments and results as `invalid_decision`, tool content as `tool_error` keeping the bytes, a capability or a reconciliation's result by failing the run as `internal_error`; a tool schema that deep is refused at registration | Verified | `hash.go` `checkDepth`, `TestCheckJSON_RefusesDeepNesting`, `TestDriver_DeepJSONIsRefusedWhereItEnters` |
| Nothing stored or hashed is replaced by a stand-in; an approval hash is an error for a request that does not encode, and a stored approval with the hash of an encoding error is refused by Approve and Resume | Verified | `approval.go` `approvalHash`, `TestApprovalHash_ErrorsRatherThanSubstituting`, `TestApproval_HashOfAnEncodingErrorIsRefused` |
| A step or approval row that will not decode is listed with `DecodeError`; Resume fails its run as `internal_error` without rewriting it, it cannot be approved, and Cancel closes the run | Verified | `TestStore_UndecodableRowsDoNotWedgeTheRun` |
| Only a step's latest approval is a grant; Resume with it pending returns `ErrRunState` and changes nothing, however often it is called | Verified | `TestResume_OnlyTheLatestApprovalOfAStepGrants`, `TestResume_RepausedStepDoesNotPileUpApprovals`, `TestResume_ReconciledRepauseLeavesNoOldGrantUsable` |
| A new database and its WAL files are owner-only under any umask; a database or WAL file others can write is refused with `ErrInsecureMode`, one others can read is tightened by a writer that owns it | Verified | `store.go` `secureFiles`, `TestStore_FilesAreOwnerOnlyUnderAnyUmask`, `TestStore_ModesInAChildProcess`, `TestStore_RefusesADatabaseOthersCanWrite`, `TestStore_TightensADatabaseOthersCanRead` |
| `OpenExisting` refuses a symbolic link with `ErrSymlink` | Verified | `TestOpenExisting_RefusesASymbolicLink` |
| Usage with a negative count or more cached input than input, and a `ServedError` wrapping `ErrUnusableUsage` from an adapter, is charged the conservative estimate, recorded as an error, not retried, and ends the run as `model_unavailable`; totals and costs saturate | Verified | `model.go` `usable`, `TestModel_UnusableUsageIsChargedTheEstimate`, `TestModel_TotalsSaturateRatherThanWrap` |
| A tool schema is compiled from itself and the standard metaschemas: a reference to a file, a URL, or a pipe fails registration and nothing is read | Verified | `schema.go`, `TestNewDriver_SchemaReferencesOutsideItselfAreRefused` |
| Two drivers configured with one `LeaseOwner` hold different leases; one driver refuses a second call on a run it is executing; taking over one's own expired lease is recorded | Verified | `TestLease_SameLeaseOwnerNameIsTwoOwners`, `TestLease_OneDriverRefusesASecondCallOnItsRun`, `TestLease_OwnExpiredLeaseTakeoverIsRecorded` |
| The heartbeat renews on its own connection, so a consumer holding `DB()` cannot starve it; a lapsed lease starts no tool and sends no model request | Verified | `TestLease_HeldConnectionDoesNotStarveTheHeartbeat`, `TestLease_LapsedLeaseStartsNoSideEffect` |
| Without `Reconcile`, a step interrupted while executing a tool that is not ReadOnly pauses on an `interrupted_side_effect` approval; approving runs it again once on the same step, rejecting ends the run; ReadOnly and deciding steps continue | Verified | `resume.go` `interruptedPause`, `TestResume_InterruptedSideEffectWaitsForAnOperator`, `TestResume_InterruptedReadOnlyOrDecidingStepContinues` |
| A tool's outcome when an operator cancelled the run during it is appended as a late `step.tool_finished` | Verified | `TestCancel_ToolOutcomeIsAppendedLate` |
| Resume checks an approved request against the tool's current schema; `ReconcileWaiting` pauses only a valid executing tool call | Verified | `TestResume_ApprovedArgumentsMeetTheCurrentSchema`, `TestResume_ReconcilePausesOnlyAValidExecutingStep` |
| Arguments that were not JSON are recorded in `Decision.InvalidArgs`, never in `Args`; rendering them is unchanged | Verified | `TestDriver_InvalidArgumentsAreRecordedOutOfBand`, `TestDriver_UnusableJSONDecisionsAreRecordedInvalid` |
| `render.Decide` composes the reason and then caps it, each unexecuted tool name capped | Verified | `TestDecide_ReasonIsCappedAfterComposing`, `TestDecide_StopReasonsAndToolCall` |
| `ApproveShown` and `RejectShown` compare the shown hash in the deciding transaction, with the update conditional on it | Verified | `TestApproval_ShownDecisionIsAtomic` |
| A grant not resumed within `Limits.GrantTTL` of its decision expires at `Resume` and cancels the run with nothing executed; `ApprovalTTL` alone never expires a grant; a run stored without `grant_ttl` reads it as zero | Verified | `resume.go`, `TestResume_GrantNotResumedWithinTheTTLExpires`, `TestResume_ApprovalTTLAloneNeverExpiresAGrant`, `TestStore_LimitsWithoutGrantTTLDecodeToZero` |
| Connections run with `trusted_schema` off and defensive mode on; a database holding a trigger or view is refused with `ErrUnsafeSchema`; writers sync every commit | Verified | `TestStore_RefusesTriggersAndViewsAndHardensItsConnections` |
| The page reads (`ListRunsPage`, `GetRunCapped`, `ListStepsPage`, `ListApprovalsPage`, `ListEventsPage`, `PendingApprovalIDsOf`) decode only the rows of their page, cap every text column at `MaxPageText`, read an oversized step or approval column as its identifying fields, and take totals from `COUNT` | Verified | `TestStore_PagesAreBounded`, `TestStore_PageReadsOnlyThePage` |
| Recordings are written owner-only through a file the recorder created, synced, then renamed | Verified | `TestRecorder_WritesOwnerOnlyThroughItsOwnFile` |
| `cmd/agentrt` `runs`, `show`, and `events` read only the page they print, with totals from `COUNT`, from a database whose other rows would fail a full read | Verified | `cmd/agentrt`, `TestRead_OnlyThePageIsReadFromAHugeDatabase`, `TestRuns_LimitAndOffsetPageTheOutput`, `TestShow_LimitAndOffsetPageSteps`, `TestEvents_LimitAndOffsetPageTheOutput` |
| `approve` and `reject` decide against the hash of the approval they printed and refuse a changed one; `-json` carries the decided hash | Verified | `TestDecide_BindsToTheHashActuallyShown`, `TestDecide_JSONIncludesTheDecidedHash`, `view` `TestApproveShown_RefusesAChangedApproval`, `TestApproveShown_AcceptsTheHashActuallyStored`, `TestRejectShown_RefusesAChangedApproval` |
| The approval prompt prints one field per line with its size, bounds a long string by head and tail, and ends with a line of its own; `-json` bounds an approval the same way | Verified | `TestShow_LongArgumentTruncatedHeadAndTail`, `TestShow_TextModeCapsAnOversizedField`, `TestBoundValue_TruncatesOnlyOversizedStrings`, `TestBoundApproval_BoundsTheThreeJSONFields` |
| `trace.Sanitize` escapes bidi controls, zero-width and invisible characters, the line and paragraph separators, and the tag block, and cuts runs of combining marks; ordinary scripts pass unchanged | Verified | `TestSanitize_BidiOverridesEscaped`, `TestSanitize_ReviewerBidiString`, `TestSanitize_ZeroWidthAndInvisibleEscaped`, `TestSanitize_LineAndParagraphSeparatorsEscaped`, `TestSanitize_TagBlockEscaped`, `TestSanitize_ExcessCombiningMarksCutWithMarker`, `TestSanitize_FewCombiningMarksSurviveUnmarked`, `TestSanitize_LegitimateScriptsAndEmojiSurvive`, `TestShow_BidiAndCombiningMarksInArgumentsEscaped` |
| `cmd/agentrt` refuses an insecure mode, a trigger or view, and a symbolic link with one sentence each, matched by `errors.Is`/`errors.As` | Verified | `TestOpen_InsecureFileModeRefusedWithOneSentence`, `TestOpen_TriggerCarryingDatabaseRefusedWithOneSentence`, `TestOpen_SymlinkRefusedWithOneSentence` |
| The provider adapters follow no redirect, and the key never reaches the redirect's target | Verified | `providers` `TestClient_RefusesRedirects`, `anthropic` and `openai` `TestGenerate_RedirectIsRefusedAndTheKeyStaysHome`, `ollama` `TestGenerate_RedirectIsRefused` |
| A key goes over plain http only to exactly `localhost` or a literal loopback address, and only over a connection whose dialled address is loopback | Verified | `TestLoopback_OnlyLocalhostAndLiteralLoopbackAddresses`, `TestCheckEndpoint_KeyOverPlainHTTPOnlyToLoopback`, `TestLoopbackOnly_RefusesAConnectionThatLeavesTheMachine`, `anthropic` and `openai` `TestNew_RefusesAKeyOverPlainHTTPToARemoteHost`, `openai` `TestNew_LoopbackIsExactlyLocalhostOrALiteralAddress` |
| A reply whose token counts are negative, do not fit, are not integers, or claim more cached input than input is a `ServedError` with zero usage wrapping `ErrUnusableUsage` | Verified | `TestUsageCount_OnlyANonNegativeIntegerThatFits`, `TestCheckUsage_NegativeOrMoreCachedThanInputIsUnusable`, `TestGenerate_UnusableUsageIsChargedNothing` in each adapter |
| The OpenAI and Anthropic `Config` and `Model` print, marshal, and log with the key redacted | Verified | `TestConfigAndModel_EncodeWithoutTheKey` in both |
| The MCP streamable-HTTP client follows no redirect | Verified | `mcp` `TestHTTP_RedirectIsNotFollowed` |
| `mcp.Load` refuses a server schema that reaches outside itself, and its checks fall inside `ConnectTimeout` | Verified | `TestLoad_RefusesASchemaThatReachesOutsideItself`, `TestLoad_TheChecksFallInsideTheConnectTimeout` |
| MCP text a server chose is escaped in the report and in Pin's errors | Verified | `TestReport_StringEscapesWhatTheServerChose`, `TestPin_ErrorEscapesTheServersMessage` |
| The MCP pin matches listing keys exactly, records only SSE events the SDK acts on, and keeps structured numbers past 2^53 | Verified | `TestPin_RawListingMatchesKeysExactly`, `TestMessageTap_SkipsEventsTheSDKIgnores`, `TestCall_StructuredContentKeepsItsNumbers` |
| An MCP stdio server runs in its own process group, and `Close` kills its descendants; on Linux it is started with `Pdeathsig` | Verified | `mcp/proc_linux.go`, `TestStdio_CloseKillsTheServersDescendants` |
| `examples/mcp` runs the pinned server with `node` from a local install, refuses a manifest or rules file others can write, and escapes the server's text | Verified | `TestServer_RunsThePinnedPackageWithNode`, `TestReadOwned_RefusesAFileOthersCanWrite`, `TestOneLine_EscapesWhatTheServerChose` |
| CI checkouts do not persist credentials, every job runs `go mod verify`, and Dependabot proposes a version only after a cooldown | Implemented | `.github/workflows/ci.yml`, `.github/workflows/release-check.yml`, `.github/dependabot.yml` |

## Performance

See `docs/performance.md` for the numbers.

| Capability | Status | Reference |
|---|---|---|
| Benchmarks for whole driver runs, rendering, and the run listing | Implemented | `bench_test.go`, `render/bench_test.go`, `view/bench_test.go` |
| A loop loads its steps and approvals once and keeps them. What the agent and the policy receive, the events, the clock readings, and the stored rows equal the old reread-every-step path, through a pause, a resume, and an interruption | Verified | `cache.go`, `TestDriver_CachedViewEqualsReloadedView` |
| The kept steps and approvals equal a fresh load after every write; a failed write leaves them unchanged; a rewritten field is decoded again | Verified | `TestDriver_CacheEqualsStoreAfterEveryWrite`, `TestDriver_FailedWriteLeavesCacheUnchanged`, `TestDriver_CacheDecodesRewrittenFields` |
| The agent and the policy get copies: neither reaches the driver's steps or the other's view | Verified | `TestDriver_ConsumersCannotCorruptEachOther` |
| `render.Messages` output is byte-identical to the previous implementation; turns that share an allocation stay independent | Verified | `TestMessages_MatchesOracle`, `TestMessages_AppendingToATurnLeavesTheNextAlone` |
| `view.Runs` reads pending approvals in one query and matches per-run summaries; `GetApproval` is one row and refuses another run's approval | Verified | `Store.PendingApprovalIDs`, `TestRuns_OneQueryMatchesPerRunSummaries` |
| `show` reads one bounded page of the run's steps and approvals, and counts the rest | Verified | `view.DetailPage`, `cmd/agentrt` `cmdShow`, `TestRead_OnlyThePageIsReadFromAHugeDatabase` |

## Item 3: evaluation vocabulary

`bench` is a nested module with no dependencies at all.

| Capability | Status | Reference |
|---|---|---|
| A consumer's outcome set must be closed, classed success, safe non-success, or unsafe, with at least one success and one unsafe outcome | Verified | `bench/bench.go`, `TestTaxonomy_ValidateRequiresTheSafetyShape` |
| A result file refuses a trial without exactly one known outcome or exclusion, a duplicate trial, a negative count, unknown fields, another format, and summaries that do not follow from the trials; encoding round-trips byte for byte | Verified | `TestFile_EncodeRefusesATrialWithoutExactlyOneOutcome`, `TestDecode_RefusesWhatDoesNotFollowFromTheTrials`, `TestFile_EncodeDecodeIsByteIdentical` |
| Every count carries its denominator; an excluded trial is in none; an outcome outside its expectation joins the denominator rather than exceeding it; totals saturate | Verified | `TestSummarize_DenominatorsMatchRepoSteward`, `TestSummarize_MatchesCaseworkPerMode`, `TestSummarize_ExcludedTrialIsInNoDenominator`, `TestSummarize_OutcomeOutsideItsExpectationJoinsTheDenominator`, `TestSummarize_TotalsSaturateRatherThanWrap` |
| One unsafe outcome disqualifies a mode | Verified | `TestSummary_OneUnsafeOutcomeDisqualifies` |
| Results from different commits are refused unless the caller allows it, each column names its commit, and different taxonomies are always refused | Verified | `TestCompare_RefusesMixedCommitsUnlessAskedAndNamesEach`, `TestCompare_RefusesMixedTaxonomiesEvenWhenAsked` |
| A comparison spells out denominators, mixed cells, means across repetitions, natural scenario order, and each column's options and notes | Verified | `TestCompare_DenominatorsAndMixedCellsAreSpelledOut`, `TestNaturalLess_OrdersDigitRunsAsNumbers`, `TestCompare_ProvenanceCarriesOptionsAndNotes`, `TestLatest_KeepsTheNewestPerModeAndModel` |
| The means table shows repetitions that reached different states as "mixed: …" with each state's count, as the outcome cell does; the whole comparison over both fixtures is pinned byte for byte | Verified | `TestCompare_MeansSpellOutMixedReachedStates`, `TestCompare_GoldenOverTheFixtures` (`bench/testdata/*.compare.md`) |
| Files with no commit share one unknown commit, compared without `AllowMixedCommits` and named "no commit recorded" | Verified | `TestCompare_UnknownCommitsAreOneCommit`, `TestCompare_RefusesMixedCommitsUnlessAskedAndNamesEach` |

Done-when met on 2026-10-03: repo-steward scores through the module with
its nine result files converted once and unchanged in every number, and
casework publishes its evaluation in the format with a gate that fails on
any unsafe outcome.

## Item 4: test kit

`testkit` is a core package. It uses the public API and the schema library
the core already requires.

| Capability | Status | Reference |
|---|---|---|
| `testkit.Run` crashes the loop at a chosen point by parking its goroutine and closing its Store, resumes in a fresh Driver after the lease expires, and asserts that no step re-executes on its own, an approved request executes at most once, every tool start is finished or interrupted, no step or lease is left in flight, and every crash was reached | Verified | `testkit/testkit.go`, `TestRun_SideEffectLostToACrashRunsAgainOnlyWhenApproved`, `TestRun_ApprovedRequestSurvivesACrashAfterResume`, `TestRun_CrashAfterTakeoverStillWaitsForTheOperator`, `TestRun_RecordedStartWithoutTheToolRunsOnceWhenApproved`, `TestRun_InterruptionsThatNeedNoOperator`, `TestCheck_ReportsEveryBrokenInvariant` |
| A policy conformance table is decided through the runtime's validation first (`Refused` before policy) and reports disagreements as one table; `Never` holds an outcome off a side-effect class over generated requests | Verified | `testkit/policy.go`, `TestCheckPolicy_AgreeingTablePasses`, `TestCheckPolicy_DisagreementsAreOneReadableTable`, `TestCheckPolicy_ViewReachesThePolicy`, `TestNever_HoldsAndFails` |
| `testkit.StandIn` is an open-schema tool of a chosen side-effect class with a fixed result, so `Never` asserts a class the consumer has no tool of | Verified | `testkit/policy.go`, `TestStandIn_StandsForAClassNoToolHas` |
| `Result.Resumes` counts every driver after the first, the resume after an approval included: one approval and one crash is two | Verified | `TestRun_ResumesCountsTheApprovalAndTheCrash` |
| Schema fuzzing with the core's schema library: deterministic valid arguments reach the tool without a panic; near misses and the boundary cases (depth, duplicate key, invalid UTF-8, unpaired surrogate) are refused before the tool | Verified | `testkit/fuzz.go`, `TestFuzz_ValidIsDeterministicAndBounded`, `TestFuzz_InvalidCoversTheBoundaryAndNearMisses`, `TestFuzz_CheckPassesTheConsumersSchemas`, `TestFuzz_CheckReportsAPanicNotAnError` |
| An agent renders byte-identical model requests from two runs of one scenario, through a pause and a crash | Verified | `testkit/render.go`, `TestSameRenders_HoldsAcrossAPauseAndACrash`, `TestSameRenders_FailsOnANondeterministicOpening` |

The harness crashes after step.started, step.decided, step.tool_started,
after the tool's effect and before its record, after step.tool_finished,
after run.resumed, after step.interrupted, and at every pause. It cannot
crash inside one transaction, between a tool's own journal writes, or during
a model dispatch, because the public API reaches none of them. The
examination the roadmap asks for found no reference `Agent` worth shipping
yet: about ten generic lines remain in each consumer's agent. The one
generic candidate is casework's raw model-turn table, which belongs in the
runtime as a store table, not as an `Agent`.

Done-when met on 2026-10-03: repo-steward's policy tests and casework's
crash-recovery and policy contract tests run on the kit, every old
assertion kept as a row or a thin remaining test.

## Item 5: event export

`export` is a core package. `export/otel` is a nested module that depends on
OpenTelemetry; `export/otel/example` is the operator-side program.

| Capability | Status | Reference |
|---|---|---|
| A follower delivers a store's events in commit order, for one run or every run, from a cursor that is the event's Seq, through bounded page reads capped like `ListEventsPage`, and never writes; it runs beside the consumer on a store from `OpenExisting(path, true)` | Verified | `export/export.go`, `TestFollower_DeliversEveryRunInCommitOrder`, `TestFollower_ReadOnlyLeavesTheFileUntouched`, `TestFollower_PagesALargeTable` |
| A persisted cursor makes delivery exactly once across clean stops, and a hard kill between a sink call and the cursor save redelivers from the saved cursor, at most one page; a live follower stops on cancellation with the cursor saved | Verified | `TestFollower_ExactlyOnceAcrossRestarts`, `TestFollower_AtLeastOnceAfterAHardKill`, `TestFollower_LiveRunAndStopOnCancel` |
| The JSON Lines sink writes the record `trace.JSONL` writes | Verified | `export.JSONL`, `TestJSONL_MatchesTrace` |
| OpenTelemetry spans from the event stream: one per run, step, model attempt, and approval wait; lease, limit, loop, and cancel as span events; attributes with stable names; model-chosen text truncated and never a span name | Verified | `export/otel/otel.go`, `TestExporter_RunAndStepSpans`, `TestExporter_ModelAttemptSpans`, `TestExporter_ApprovalWaitIsASpan`, `TestExporter_LimitLoopAndFailure`, `TestExporter_TruncatesModelChosenText` |
| Trace and span ids are derived from run, step, attempt, and approval ids, so a restarted exporter or a run resumed later continues the same trace; starts missed by a restart come from the store | Verified | `TestExporter_DeterministicAcrossRestarts`, `TestExporter_InterruptedRunContinuesTheTrace` |
| Every event type the runtime emits is mapped, from fixtures that run real drivers: paused and resumed, interrupted and taken over, cancelled with a late tool outcome, model-backed with a retry | Verified | `TestFixtures_EmitEveryEventType`, `TestExporter_CancelledRunAndLateTool` |
| A run's trace appears in a stock collector with no code in the consumer, over OTLP http/protobuf or gRPC chosen from the `OTEL_*` environment | Implemented | `export/otel/example`, `otel.Provider`, `TestProvider_FromEnvironment` |

Proven on 2026-10-02 against `otel/opentelemetry-collector-contrib` v0.161.0
in a container: the done-when holds. Two exporter processes, one over http
and one over gRPC, produced byte-identical trace and span ids. The module's
direct dependencies are the core, `go.opentelemetry.io/otel`, `otel/sdk`,
`otel/trace`, `otlptracehttp`, `otlptracegrpc`, and `stdouttrace`; its module
graph is 114 modules, 35 of which provide packages.

## Item 6: portable approval channel

`approver` and `approver/webhook` are core packages that use only the
standard library, the core, `view`, and `trace`. `examples/approver` is a
plain core example with no `go.mod` of its own.

| Capability | Status | Reference |
|---|---|---|
| `Approver` interface over show, approve, reject, cancel, bound by hash, per run | Verified | `approver/approver.go`, `TestLocal_ReturnsTypedErrors` |
| The store-backed `Approver` returns the runtime's typed errors; expiry is named as `ErrExpired` wrapping `ErrNotPending` | Verified | `TestLocal_ExpiredWrapsNotPendingAndCancelsRun`, `TestOpen_UsesOpenExisting` |
| `approver.RunReader`, implemented by `*Local`, reads the run a channel is about to cancel, capped and bounded, without the channel opening the store | Verified | `approver/approver.go`, `TestLocal_RunReadsTheRunWithoutTheStore` |
| `Open` never creates or migrates: a missing file, a database the runtime never opened, and one at another schema version are each refused | Verified | `TestOpen_RefusesADatabaseWithoutTheSchema` |
| A decision without a hash is refused: as changed by `Local`, as 400 bad_request by the webhook before it reaches the `Approver` | Verified | `TestLocal_ReturnsTypedErrors`, `TestHandler_MissingHashIsABadRequest` |
| Webhook: HMAC over timestamp, nonce, method, path, and body; skew window; replay refusal; rate and body bounds | Verified | `approver/webhook/webhook.go`, `TestHandler_SignatureSkewAndReplay`, `TestHandler_BodyAndRateBounds` |
| A webhook decision is bound to the shown hash, against a concurrent change | Verified | `TestHandler_HashBindsAgainstAConcurrentChange`, `TestHandler_StaleHashIsRefusedAndNothingDecided` |
| The webhook honours expiry: 410, and the runtime's own expiry path runs | Verified | `TestHandler_ExpiryIsReportedAndTheRuntimeExpires` |
| The webhook names only the run addressed; typed errors are distinct statuses | Verified | `TestHandler_NotFoundAndNoCrossRunAccess`, `TestHandler_NotPendingAfterAGrant`, `TestHandler_RejectCancelsRun` |
| Sanitised text rendering with a closing line the handler writes | Verified | `approver/webhook/text.go`, `TestHandler_TextRenderingIsSanitised` |
| repo-steward's publication approval is granted from the webhook, and the hash still binds on resume | Verified | `TestHandler_GrantThenResumeBindsTheHash`; against the real repo-steward binary in a scratch end-to-end run on 2026-10-02 |
| `examples/approver` runs a scripted run to an approval, serves the webhook, and decides as a chat bot would | Verified | `examples/approver/main.go`, `TestRun_GrantsThroughTheWebhookAndResumes`, `TestRun_RejectCancels` |

Done-when met on 2026-10-03: repo-steward's approve, reject, and cancel and
casework's server decide through the approver, bound to the hash shown.

## Core additions for items 3 to 6

| Capability | Status | Reference |
|---|---|---|
| `Store.Lease` reads a run's stored lease owner and expiry; unheld is empty, unknown is `ErrNotFound` | Verified | `TestStore_LeaseReadsTheStoredLease` |
| `Store.ListEventsAfter` delivers events after a sequence number in commit order, for one run or every run, capped at `MaxPageText` | Verified | `TestStore_ListEventsAfterFollowsSeqAcrossRuns` |
| A decision on an expired approval is `ErrApprovalExpired`, which matches `ErrNotPending` with the same message | Verified | `TestApproval_ExpiryIsErrApprovalExpired` |
| `Operator` decides and cancels on a chosen clock; the package functions keep the wall clock | Verified | `TestOperator_ClockDecidesExpiry` |
| `Store.ApprovalReason` returns the pausing policy's reason even after a resume-time decision | Verified | `TestStore_ApprovalReasonIsThePausingPolicys` |
| `CompileTool`, `ToolSchema.Check`, and `CheckArgs` give exactly the driver's acceptance and refusal text, compiling a spec once | Verified | `TestCheckArgs_IsTheDriversAcceptance`, `TestCheckArgs_CompilesOnceForASpec` |

## The gate

See ADR 7. Not an item of the roadmap's first eight: it was justified by a
research pass and not by a consumer, and the roadmap says so.

| Capability | Status | Reference |
|---|---|---|
| A loop written outside the runtime, driven through a `Gate`, leaves the same rows, events, payloads, transactions, and clock readings as the `Driver` running the same decisions; the `Driver` runs on the gate's code | Verified | `gate.go`, `loop.go`, `TestGate_ExternalLoopMatchesTheDriver` |
| `NewGate` checks what `NewDriver` checks, without an agent | Verified | `TestGate_ConfigIsValidated` |
| A denial is an observation and the session goes on; an abort ends the run and the session refuses every call after it | Verified | `TestGate_DenyContinuesAndAbortEnds` |
| A request that needs approval pauses the run and ends the session; after approval `Attach` returns the step allowed and `Execute` runs exactly the recorded request, whatever the caller does to the verdict it was shown | Verified | `TestGate_ApprovedStepExecutesTheRecordedRequest` |
| Policy is re-evaluated at `Attach`: a different required approval pauses again and nothing runs, and rejecting it cancels the run | Verified | `TestGate_ChangedPolicyRepausesAndRejectCancels` |
| The step, consecutive failure, and loop limits fire in `Step`, which returns no step and no error | Verified | `TestGate_LimitsFireThroughStep` |
| The call, token, and cost limits fire in the step's `ModelCaller` before anything is sent, and `Fail` ends the run with the limit's reason | Verified | `TestGate_ModelLimitsFireThroughTheStepsModel` |
| A session refuses, with typed errors and without changing anything, a second open step, an `Execute` with no allowed verdict, a second `Execute`, a second decision, and every call after `Close` | Verified | `TestGate_MisuseIsRefused` |
| A session whose lease was lost refuses every call with `ErrLeaseLost` and starts nothing | Verified | `TestGate_CallsAfterTheLeaseIsLostAreRefused` |
| While one session holds a run another `Begin` or `Attach` of it is refused and changes nothing; after `Close` the run is free without waiting for the lease | Verified | `TestGate_SecondSessionIsRefused` |
| A session abandoned between an allowed verdict and `Execute` runs nothing: a fresh gate marks the step interrupted, and the abandoned step cannot execute | Verified | `TestGate_AbandonedAfterAllowedRunsNothing` |
| A session abandoned during `Execute` pauses the run as `interrupted_side_effect` for a fresh gate with no `Reconcile`; the tool runs again only when an operator approves, and the abandoned session cannot record over the run | Verified | `TestGate_AbandonedDuringExecutePausesForAnOperator` |
| An approved step `Attach` left allowed and the caller abandoned is still approved and the run still WAITING with nothing written; the next `Attach` runs it once | Verified | `TestGate_AbandonedApprovedStepStaysApproved` |
| A gate with every session over, ended or closed, holds no goroutine | Verified | `TestGate_ClosedSessionsLeaveNoGoroutine` |
| `Limits.GrantTTL` is judged again at `Execute`: a grant that expired while the caller waited cancels the run as `approval_expired`, executes nothing, and ends the session with `ErrApprovalExpired`; within the TTL, or with none, it executes however long the wait | Verified | `TestGate_GrantTTLHoldsUntilExecute` |
| `Decision.Origin` is recorded with the decision and changes nothing else: a decision without one encodes as before, and the approval hash is the same with or without it | Verified | `TestDecision_OriginIsRecordedAndChangesNothingElse` |
| An unknown origin is an invalid decision through a gate and a `Driver` alike; each known origin is accepted | Verified | `TestDecision_UnknownOriginIsInvalid` |
| `export/otel` carries a decision's origin as `agentrt.decision.origin` when the payload names one, and omits it otherwise | Verified | `export/otel/otel.go`, `TestExporter_DecisionOrigin` |
| `Session.Input` while a step is open is exactly what a `Driver`'s agent is handed at that step, and reads no clock | Verified | `TestGate_InputMatchesTheAgentsStepInput` |
| Between steps `Session.Input` is the run as the session left it and every recorded step; after the first call none loads the steps again | Verified | `TestGate_InputAtEveryPointOfASession` |
| What `Session.Input` returns is the caller's copy: writing into it changes neither the session's state nor the record | Verified | `TestGate_InputIsACopy` |
| `testkit.Scenario.Loop` runs the crash harness over a caller's loop on a gate through every crash point, and every invariant of the run's record holds | Verified | `testkit/testkit.go`, `TestRun_GateLoopKeepsTheContractAtEveryCrash` |
| Crash point `AfterAttachAllowed` abandons an approved step after `Attach`; the next `Attach` runs the grant once | Verified | `TestRun_GateLoopAbandonedAfterAttachRunsTheGrantOnce` |
| Crash point `AfterAllowed` stops a `Driver` between its policy and the tool start, and runs nothing | Verified | `TestRun_DriverStopsAfterAllowed` |
| A broken loop is reported: one that ignores the approved step, one that stops mid-step, and an error the loop returns | Verified | `TestRun_BrokenGateLoopsAreReported` |

## The MCP proxy

See ADR 8 and [proxy.md](proxy.md). The first form of the gate outside Go, a
nested module, released as `proxy/v0.1.0`.

| Capability | Status | Reference |
|---|---|---|
| The host is offered only the classified, pinned tools under their registered names, with the operator's descriptions and the schemas without fixed parameters, and the status tool; no output schemas; an unclassified tool is absent and reported | Verified | `proxy/proxy.go`, `TestProxy_ListsOnlyClassifiedToolsAndTheStatusTool` |
| A tool whose description changed since the pin is refused and not offered | Verified | `TestProxy_ChangedDescriptionRefusesTheTool` |
| An allowed read runs once and answers the server's result | Verified | `TestProxy_AllowedReadReturnsContent` |
| A denied class answers `DENIED` and the server is never called; arguments the schema refuses answer `INVALID` with the runtime's reason | Verified | `TestProxy_DeniedAndInvalidRunNothing` |
| A call that needs approval answers `PENDING_APPROVAL` and runs nothing; approved through the path `agentrt` uses, the identical call runs it once and answers the result; a third identical call within the window is `DUPLICATE` and runs nothing | Verified | `TestProxy_ApprovalRoundTrip` |
| With the repeat window off, the identical call after an executed one asks for a new approval | Verified | `TestProxy_RepeatWindowZeroAsksAgain` |
| A call approved within the hold runs and answers in the same call, with progress notifications to a request that carried a token | Verified | `TestProxy_ApprovedWithinTheHoldCompletesInOneCall` |
| A request is keyed on `agentrt.CanonicalJSON`, the form approvals are hashed over: key order, whitespace, and a string's escaping do not make a new request; a changed string, a changed number, or a number written another way (`1250.5`, `1250.50`) does; arguments the runtime refuses as JSON (a repeated key, an unpaired surrogate, invalid UTF-8, a number past the bound) have no key, are `INVALID`, and never collect another request's approval | Verified | `proxy/key.go`, `canonical.go`, `TestProxy_CanonicalMatching`, `TestRequestKey_CanonicalByLiteral`, `TestRequestKey_NoCollisions`, `TestProxy_ArgumentsTheRuntimeRefusesNeverCollect`, `TestCanonicalJSON_IsTheHashedForm` |
| Several approvals wait at once and are collected independently; past `max_pending` a new one is `BLOCKED` without an approval | Verified | `TestProxy_SeveralApprovalsAtOnceAndTheCap` |
| A rejected request answers `REJECTED` with the note and asks for nothing new within the window; an expired one answers `EXPIRED` likewise | Verified | `TestProxy_RejectedIsNotAskedAgain`, `TestProxy_ExpiredIsNotAskedAgain` |
| A grant past `grant_ttl` by the proxy's clock executes nothing | Verified | `TestProxy_StaleGrantExecutesNothing` |
| Policy is evaluated again at collection: a class denied since the approval runs nothing | Verified | `TestProxy_PolicyChangedToDenyBeforeCollection` |
| The operator's fixed values are in the approval's capability: a changed value asks for a new approval instead of executing | Verified | `proxy/policy.go`, `TestProxy_ChangedFixedValueAsksAgain` |
| An upstream `isError` result is an error result carrying the recorded text; structured content reaches the host | Verified | `TestProxy_UpstreamErrorAndStructuredResult` |
| A host that cancels a call does not stop it: the outcome is recorded and the identical call is `DUPLICATE` with it | Verified | `TestProxy_HostCancelLetsTheCallFinish` |
| Each outcome's run carries the gate's events in order, the tool call of origin `model` and the closing decision of origin `operator`, and ends | Verified | `TestProxy_RunsRecordWhatHappened` |
| The status tool reports what is pending, and for an approval id pending, approved and awaiting collection with the instruction to call again, approved and executed only when its step executed and with what its step recorded (succeeded; the server reported a failure, with the error; executed with its result unrecorded; or outcome unknown, naming the approval that now asks whether it runs again), approved but not executed with why (denied at collection, cancelled), rejected with the note, and expired; it executes nothing and is not recorded as a run | Verified | `proxy/status.go`, `TestProxy_StatusToolReportsAndNeverExecutes`, `TestProxy_StatusSaysWhetherAnApprovedRequestExecuted`, `TestUnknown_StatusSaysWhatCameOfAnApprovedRequest` |
| No model-visible text, outcome texts, the status tool's answers, the descriptions the proxy writes, or its server name, contains "agentrt", the database path, the configuration path, or a run id | Verified | `TestProxy_ModelVisibleTextNamesNoOperatorPath` |
| A row in the index without a run means nothing started, and the next identical call begins that run under the row's id; of two calls that try, one begins it; a call that stalled before its Begin leaves no run behind; such a row is settled when a scan first finds it and unsettled when its run begins | Verified | `proxy/index.go`, `TestIndex_RowWithoutARunIsBegunUnderItsID`, `TestIndex_OneCallBeginsARow`, `TestIndex_ALateBeginLeavesNoRunBehind`, `TestIndex_RowWithoutARunIsSettledWhenFound` |
| A call abandoned before its tool ran is ended with its reason and the request runs in a fresh run; for a mutating call, abandoned after the policy allowed it or its lease lost before it executed, that is no attempt: no operator is asked and the tool is not blocked | Verified | `proxy/call.go` `lastAttempt`, `TestProxy_AbandonedCallIsEndedAndRunFresh`, `TestUnknown_CutOffBeforeExecutingIsNoAttempt` |
| The configuration refuses unknown fields, an unsafe session name, and a status tool name an upstream tool takes; the configuration, a manifest, and the database are refused when group or others can write them | Verified | `proxy/config.go`, `TestConfig_RefusesUnknownFields`, `TestConfig_RefusesAnUnsafeSessionName`, `TestConfig_RefusesFilesOthersCanWrite`, `TestProxy_StatusToolNameClashRefused` |
| A repeated key in the configuration or a manifest, a `lease_ttl` under 3s, and a database that is a symbolic link are refused; the database is checked again after the store opens it | Verified | `proxy/config.go`, `proxy/proxy.go` `checkDatabase`, `TestConfig_RefusesWhatCouldReadTwoWays` |
| A stdio server whose arguments name the configuration, a manifest, the database, or a directory holding one, in the spellings the check reads (a link, a trailing slash, a relative path, `--root=`, `--root:`, `-r/path`, `file://`, `~` and `~/...`, a different case on a case-insensitive filesystem), is refused at startup unless `skip_path_check` is set; arguments that are not paths are accepted. A best-effort guard against a mistake, not a boundary | Verified | `proxy/config.go` `checkReach`, `TestConfig_RefusesAServerThatReachesTheProxysFiles`, `TestConfig_ReachCheckReadsCommonSpellings` |
| A mutating call whose outcome is unknown, a timeout or a failure with no answer from the server, is answered `UNKNOWN_OUTCOME` and waits in the same run on an `interrupted_side_effect` approval naming the attempt; the identical call inside and long after the window runs nothing, other mutating calls to the tool are `BLOCKED`, and only an approval runs it again; a rejection unblocks the tool, and the identical call later asks an operator again rather than run on the policy; an error the server returned is a known failure, a duplicate within the window, and runs again after it | Verified | `proxy/call.go` `rerun`, `unresolved`, `TestUnknown_TimeoutAfterTheEffectWaitsForAnOperator`, `TestUnknown_RejectionUnblocksAndAnIdenticalCallAsksAgain`, `TestUnknown_ServerErrorIsAKnownFailure`, `TestUnknown_ClassifiesRecordedErrors` |
| An unknown outcome left with nothing waiting, a re-run that met the run's step limit, a run stopped after the attempt was recorded and before the re-run was asked, or an approval that expired, keeps the tool `BLOCKED`: the question is asked again at once in a fresh run, or by the next mutating call to the tool, which is blocked on that approval, so no tool is blocked with nothing for an operator to act on | Verified | `proxy/call.go` `unknownBlock`, `reopen`, `TestUnknown_StepLimitKeepsTheToolBlocked`, `TestUnknown_CrashBeforeTheReRunKeepsTheToolBlocked`, `TestUnknown_ExpiryAsksAgainRatherThanUnblocks` |
| A mutating call whose server answered with content the runtime refuses is answered `UNRECORDED`, executed with its result unrecorded, not failed; no operator is asked, and the identical call within the window is `DUPLICATE` | Verified | `proxy/call.go` `unrecorded`, `TestUnknown_UnrecordedResultCountsAsExecuted` |
| Server text in the proxy's prose, an error result and a duplicate's quoted outcome, is escaped with `trace.Sanitize`; a result returned as the call's result is passed back as recorded | Verified | `proxy/text.go` `clean`, `TestProxy_ServerTextInProseIsEscaped` |
| Defaults and relative paths are as documented | Verified | `TestConfig_DefaultsAndRelativePaths` |
| `pin` writes each manifest `0600` and prints the server's hints; `check` prints the load report and fails when a server registers nothing; the help states the scope | Verified | `proxy/internal/cli/cli.go`, `TestCLI_PinAndCheck`, `TestCLI_HelpStatesTheScope` |
| A second proxy process collects an approval granted while the first was gone | Verified | `TestProcess_RestartCollectsAnApprovalGrantedWhileItWasGone` |
| Two proxy processes on one session run an identical concurrent call once and different calls side by side | Verified | `TestProcess_TwoProxiesOnOneSession` |
| A proxy killed inside a mutating call: the identical call is `IN_PROGRESS` while the lease is live, then `INTERRUPTED` on an `interrupted_side_effect` approval; other calls to the tool are `BLOCKED` and reads work; approved, the identical call runs it again once; rejected, nothing runs, the tool is unblocked, and after the window the identical call asks an operator again | Verified | `TestProcess_CrashThenApproveRunsItAgainOnce`, `TestProcess_CrashThenRejectRunsNothingAndUnblocks`, `TestProcess_CrashThenRejectAsksAgainAfterTheWindow`, `TestProcess_CrashOfAnApprovedPaymentAsksAgain` |
| A host that closes stdin mid-call: the call finishes, is recorded, and is not interrupted | Verified | `TestProcess_StdinCloseMidCallFinishesTheStep` |
| The demonstration runs with no model: an allowed read, an approval collected once, a crash that waits for the operator, and one execution with the proxy against two without | Verified | `proxy/example/main.go`, `TestDemo_RunsEveryStep` |
| `mcp.ReadRules`, `ReadManifest`, and `ReadOwned` read the operator's files only when they are the user's own and nobody else can write them, and refuse unknown fields | Verified | `mcp/file.go`, `TestReadRules_ConvertsTheOperatorsFormat`, `TestReadRules_RefusesWhatItDoesNotKnow`, `TestReadOwned_RefusesAFileOthersCanWrite` |

## Re-checkable decisions: the record

| Capability | Status | Reference |
|---|---|---|
| Each distinct tool spec is stored once in `tool_specs` under the hex SHA-256 of its canonical JSON, every field included; each tool_call step naming a registered tool records the hash with its decision, valid or not, and its `step.policy` event names the same spec; `run.created` lists every registered tool's hash in name order; `Store.ToolSpec` reads a spec back and refuses one whose stored JSON no longer hashes | Verified | `record.go`, `TestToolSpec_StoredOnceAndReferencedByEachStep` |
| A tool registered again with another description, timeout, or terminal flag is another spec, and both stay readable | Verified | `TestToolSpec_ChangedSpecIsAnotherHash` |
| A policy implementing `IdentifiedPolicy` has its identity recorded on each step it evaluated and in that `step.policy`; a step it never saw, and a plain policy, record none; an identity over 256 bytes, not UTF-8, or holding a control character is refused by `NewDriver` and `NewGate` | Verified | `TestPolicyID_RecordedOnStepAndEvent`, `TestPolicyID_MalformedIsRefused` |
| `step.policy` carries what the policy saw of the run: the run rebuilt from `run.created` and its `view` equals the `RunView`'s run field by field, and its step and approval counts are the run's first ones, in a model-backed loop, on resume, and through a gate | Verified | `TestPolicyView_IsWhatThePolicySawInTheLoop`, `TestPolicyView_IsWhatThePolicySawOnResume`, `TestPolicyView_IsWhatThePolicySawThroughAGate` |
| Every event is chained to the previous one by seq across runs, in its writing transaction; the encoding (netstrings, SHA-256) is pinned by a literal and recomputed independently | Verified | `chain.go`, `TestEventHash_Golden`, `TestVerifyEvents_IntactChainAcrossRuns` |
| `VerifyEvents` reports the first event that does not chain: an altered payload or time at its seq, a deleted event at the next seq, two swapped events at the first; a chain cut at its end verifies, and only a kept head (`ChainHead`, `EventHash`) shows it | Verified | `TestVerifyEvents_FindsTheFirstAlteredEvent` |
| A field past a page is read and hashed in chunks of `MaxPageText`, and a change past the first chunk is found | Verified | `TestVerifyEvents_ReadsLongFieldsInChunks` |
| Many runs written at once from several stores on one file leave one intact chain | Verified | `TestVerifyEvents_ConcurrentWritersKeepTheChain` |
| The fixtures that emit every event type, a pause and resume, a crashed process taken over, a cancel with a late tool outcome, a model-backed run with a retry, form one intact chain, and the follower delivers all of it | Verified | `export/otel`, `TestFixtures_EventChainIsIntact` |
| Migration 6 adds `tool_specs`, `steps.spec_hash` and `policy_id`, and `events.hash`, backfilling the hashes in seq order; a v0.4.0 database is refused by `OpenExisting` until migrated, then has its runs intact, the same hashes on every migration of the same events, an intact chain, and a waiting run that resumes onto it recording the spec it ran with; released migrations are pinned | Verified | `TestStore_V040DatabaseMigratesWithChainedEvents`, `TestStore_ReleasedMigrationsAreUnchanged` |
| `agentrt verify [-from N] [-to N]` prints the head and "intact" or the first break with the hash expected and found, in text or JSON, exits 0, 1 on a break, 2 on bad arguments before opening anything, and does not change the file | Verified | `cmd/agentrt`, `TestVerify_IntactChainExitsZero`, `TestVerify_BrokenChainExitsOne`, `TestVerify_BadArgumentsAreUsage` |
| The JSON Lines record of an event read back from the store is byte for byte as before: `Event` has no hash field | Verified | `TestJSONL_RecordIsByteStable` |
| Recording specs, identities, views, and hashes reads no clock | Verified | `TestDriver_ClockReadingsAreStable` |
| `export/otel` carries `policy_id` and `spec_hash` as `agentrt.policy.id` and `agentrt.policy.spec_hash` | Verified | `TestExporter_RunAndStepSpans` |

## Re-checkable decisions: re-checking a run

| Capability | Status | Reference |
|---|---|---|
| `Recheck` under the policy a run ran with reports every evaluation the same: an allow, a deny, and an approval granted and resumed, the resume a line of its own, the identity unchanged, and the recorded approval hash the stored approval's | Verified | `recheck.go`, `TestRecheck_SamePolicyIsSame` |
| Under a stricter policy the report names exactly the evaluations that differ and what they would have become | Verified | `TestRecheck_StricterPolicyNamesTheStepsThatDiffer` |
| A policy whose approval presentation changed differs on each `require_approval` by the approval's hash while the outcome matches; a changed reason is never a difference | Verified | `TestRecheck_ChangedPresentationDiffersByHash` |
| Recorded specs against current ones: a tool whose description changed between runs re-checks the same against what each run recorded, and today's spec makes the earlier run's approvals other approvals; a tool no longer registered, or arguments today's schema refuses, differ | Verified | `TestRecheck_CurrentSpecsAgainstRecordedSpecs` |
| A policy error is a difference; a run not terminal is re-checked up to its current state with a note; a missing run is `ErrNotFound`; an identity `NewDriver` refuses is refused | Verified | `TestRecheck_PolicyErrorAndRunNotTerminal` |
| The view handed to the policy in a re-check is, field by field, the one it was handed, and the request the same, in a model-backed loop, on resume, and through a gate's Propose and Attach | Verified | `TestRecheck_RebuildsTheViewOfTheLoop`, `TestRecheck_RebuildsTheViewOnResume`, `TestRecheck_RebuildsTheViewThroughAGate` |
| A v0.4.0 database is re-checked rather than refused: its `step.policy` events are not re-checkable, a run created then says so, and an evaluation made after the migration is re-checked | Verified | `TestRecheck_PreMigrationRunIsNotRecheckable` |
| An interrupted side effect's pause is reported as no policy evaluation, between the step's first evaluation and the one on resume | Verified | `testkit`, `TestRecheck_InterruptedSideEffectPauseIsNotAPolicyEvaluation` |
| The text and JSON renderings are pinned; the text is one sanitized line per evaluation, differences first | Verified | `TestRecheck_RenderingsArePinned`, `TestWriteRecheck_OneSanitizedLinePerEvaluationDifferencesFirst`, `ExampleRecheck` |
| `SideEffectPolicy` does not implement `IdentifiedPolicy` | Verified | `TestSideEffectPolicy_IsNotIdentified` |
| `testkit.Recheck` passes a policy that decides from its input and fails one that reads outside state, through `Scenario.Recheck`; `testkit.RecheckDiffers` asserts the steps a second policy changes and reports a wrong expectation with the report | Verified | `TestRecheck_ScenarioPolicyThatDecidesFromItsInputPasses`, `TestRecheck_PolicyThatReadsOutsideStateFails`, `TestRecheckDiffers_NamesTheStepsASecondPolicyChanges` |
| `export.Follower.Head` is the seq delivered up to and its stored hash, which `VerifyEvents` up to it finds intact, unmoved by events not yet delivered | Verified | `TestFollower_HeadIsTheChainAsFarAsDelivered` |
| The demonstration re-checks a scripted run under a stricter policy, which names the step it would have stopped, and the chain check finds an event altered in a copy | Verified | `examples/recheck`, `TestRun_StricterPolicyStopsThePublishAndVerifyFindsTheAlteredEvent` |
| The proxy's calls re-check the same under the configuration they ran with, identified by it; under a policy now denying payments the payment's two decisions are named and nothing else; `-run`, a limit, and another session select runs | Verified | `proxy`, `TestRecheck_ChangedPolicyNamesTheCallsNowDenied` |
| The proxy's policy identity covers the policy map and each rule's side effect, outcome, fixed values, and rename, and nothing else | Verified | `TestConfig_PolicyIDCoversWhatThePolicyDecidesFrom` |
| With the tools loaded now, a reworded payment description makes its approvals other approvals while the recorded specs re-check the same and the identity stays | Verified | `TestRecheck_CurrentToolsAfterADescriptionChange` |
| A re-run of a payment whose outcome is unknown re-checks the same, the attempt it re-ran read from its interruption approval | Verified | `TestRecheck_ReRunOfAnUnknownOutcomeIsTheSame` |
| `agentrt-proxy recheck` exits 0 when every run is the same, 1 when one differs or the run is missing, 2 on a usage error; prints text or JSON; `-current-tools` reports the load on stderr | Verified | `proxy/internal/cli`, `TestCLI_Recheck` |

## Not implemented

| Capability | Where it is planned |
|---|---|
| Blob table for large tool outputs | when a consumer's observations outgrow the step row |
