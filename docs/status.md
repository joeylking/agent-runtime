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
| Transport classification: a timeout is returned bare so the caller charges it as ambiguous; every other transport error is transient | Verified | `providers`, `TestClassifyTransport_TimeoutIsBareAndOthersAreTransient` |
| HTTP status classification: 408, 429, 529, and 5xx are transient, other non-2xx permanent, both carrying the status and a bounded body excerpt | Verified | `TestClassifyStatus_RetryableAndPermanent`, `TestClassifyStatus_BodyExcerptIsBounded` |
| An SDK adapter with only a status code and an error classifies the same way | Verified | `TestClassifyAPIError_StatusOnlyAdapter` |
| Provider response bodies are read under a 16 MiB bound | Verified | `TestReadBody_StopsAtTheLimit` |
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
| Cache reads are reported as cached input; cache writes, which have no counter, are folded into input at the 1.25 multiple they are billed at | Verified | `TestGenerate_FoldsCacheUsage` |
| Dated Claude price table keyed by the default prefixed name; local models recorded as free under theirs | Verified | `TestPrices_KeyedByTheDefaultName`, `TestFree_PricesThePrefixedName` in both local adapters |
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

## Not implemented

| Capability | Where it is planned |
|---|---|
| Blob table for large tool outputs | when a consumer's observations outgrow the step row |
| OpenTelemetry exporter over the events table | docs/roadmap.md item 5 |
| Evaluation vocabulary, test kit, approval channel | docs/roadmap.md items 3, 4, 6 |
