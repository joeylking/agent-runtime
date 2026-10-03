# Charge for what you cannot see

An agent built on agent-runtime calls an AI model at every step, and every
call costs money and time. A run can be capped: so many model calls, so
many tokens, so much estimated spend, so much elapsed or active time. A
cap is only as good as the counting behind it, and the counting has a
blind spot that most loops ignore. Sometimes a request is sent and no
answer comes back. The provider may have served it and billed for it, and
the client cannot tell. This essay is about how the runtime counts that
case, and why it counts it high.

## One door to the model

The agent, the consumer's code that decides each step, never holds the
model directly. It receives a `ModelCaller`, an accounting wrapper that is
its only handle to the model ([`model.go`](../../model.go),
[`TestModel_UsageAndCostRecorded`](../../model_test.go)). Every attempt
goes through the same sequence.

1. **Check the limits before anything is sent.** The wrapper adds the
   run's recorded totals, every attempt of this step still in flight, and
   a projection for this request, and refuses with `ErrLimit` if the sum
   would pass a cap. A refused request is never dispatched
   ([`TestModel_CallLimitStopsBeforeDispatch`](../../model_test.go),
   [`TestModel_TokenAndCostLimitsProjected`](../../model_test.go)).
   Concurrent requests within one step reserve their projections before
   dispatch, so they cannot all pass a check that only one of them fits
   ([`TestModel_ConcurrentCallsReserveAgainstTheLimits`](../../accounting_test.go)).
2. **Record the attempt before sending it.** A row in the `model_calls`
   table is written with status `dispatched`, in a transaction that also
   checks the run is still held by this process
   ([`TestLease_NoModelCallAfterTheLeaseIsLost`](../../lease_test.go)).
3. **Send it, then record what came back,** and add the charge to the
   run's totals in the same transaction as the record.

The projection is the request's estimated input, one token for every
three bytes of the serialized request, plus its output cap, the most
tokens the model is allowed to produce. A request with no output cap has
no upper bound, so under a token or cost limit it is refused before
dispatch with a message saying to set one
([`TestModel_UncappedRequestRefusedUnderAProjectedLimit`](../../accounting_test.go)).
A retry is a new attempt: a new row, checked against the limits again.

## Three kinds of outcome

An attempt ends in one of three ways.

- **Answered.** The provider returned a usable reply with usage counts.
  The attempt is charged what the provider reported.
- **Failed before service.** A refused connection or an error status. The
  attempt is recorded as an error and counted as a call.
- **Unknown.** The request went out and no answer arrived: the per-call
  timeout fired, the connection was lost while the request was being
  written or answered, or the response body was cut off partway through. The provider may have
  done the work and billed it.

The third case is the subject of the title. The runtime marks such an
attempt `ambiguous` and charges it the conservative estimate: the
request's estimated input tokens and its full output cap, priced like any
other usage ([`TestModel_AmbiguousAttemptChargedConservatively`](../../model_test.go)).
In that test a model that never answers is tried twice, and both attempts
appear as ambiguous calls with nonzero cost in the run's totals. A caller
that gives up with a request in flight gets the same treatment: the
attempt is recorded and charged before the cancellation is returned
([`TestModel_CancelledContextStillRecordsTheCall`](../../accounting_test.go)).

Why the upper bound and not zero, or an average? The two errors are not
symmetric. Undercounting lets a run slip past the cap it was given, which
is the failure a cap exists to prevent. Overcounting ends a run somewhat
early and tells the operator why. A limit that is meant to hold has to
err on the side that keeps it holding.

The provider adapters cooperate. `providers.ClassifyTransport` returns a
timeout or a body cut off mid-read as it is, unwrapped, so the accounting
wrapper charges it as ambiguous; a host that does not resolve, a
certificate that does not verify, and a malformed URL are permanent; any
other transport error is worth retrying
([`providers/providers.go`](../../providers/providers.go),
[`TestClassifyTransport_EveryClass`](../../providers/providers_test.go)).
The wrapper recognises a lost connection by the error's type, never by
its text ([`TestIsConnectionLost_MatchesErrorsNotText`](../../store_internal_test.go)).
The Anthropic adapter turns off the official SDK's own retries, because a
retry inside an SDK is a call the books never see
([`TestGenerate_ErrorsAreClassifiedAndNotRetriedBySDK`](../../providers/anthropic/anthropic_test.go)).

## Served, but not usable

A reply can arrive and still be useless: it will not decode, or it carries
a tool call whose arguments are not JSON. The provider served that request
and billed it. An adapter reports this as a `ServedError` carrying the
usage the provider reported, and the wrapper charges that usage, records
the error, and does not retry, because asking again would be billed again
for the same reply
([`TestModel_ServedErrorIsChargedAndNotRetried`](../../accounting_test.go)).

## Usage you cannot believe

The usage counts come from the provider, and the runtime does not take
them on faith. A negative count, or more cached input tokens than input
tokens, would lower the run's totals and so disarm the very limits they
are checked against. Such usage is replaced by the conservative estimate,
the reply is discarded, the attempt is not retried, and the run ends as
`model_unavailable`. The adapters apply the same rule to a count that is
not an integer or too large to hold, and report it as a `ServedError`
wrapping `ErrUnusableUsage`
([`TestModel_UnusableUsageIsChargedTheEstimate`](../../security_test.go),
[`TestCheckUsage_NegativeOrMoreCachedThanInputIsUnusable`](../../providers/providers_test.go)).
Whoever reported nonsense about the bill is not believed about the rest
of the reply either.

Totals are added with saturating arithmetic, in Go and in the SQL that
updates the run's row. A huge count fills a total to its maximum instead
of wrapping around to a negative number that would pass every check. The
next limit check then refuses the run
([`TestModel_TotalsSaturateRatherThanWrap`](../../security_test.go)).

## Waiting as asked

A transient failure, such as a rate limit or a server error, is retried
up to `ModelConfig.MaxRetries` times, which is zero unless the consumer
sets it ([`TestModel_TransientRetryThenSuccess`](../../model_test.go)).
Providers often say how long to wait in a `Retry-After` header, and the
adapters carry that value on the error. The wrapper waits at least that
long, and at least its own backoff
([`TestModel_RetryAfterIsHonoured`](../../retry_test.go)). Retrying early
is how a client gets throttled harder.

Some waits cannot fit. A provider that asks for longer than
`ModelConfig.MaxRetryAfter`, a minute by default, ends the run as
`model_unavailable` rather than being retried early. So does a wait that
would pass the caller's deadline or reach the run's elapsed or active time
limit. In each case the run ends without sleeping, because a sleep that
cannot be followed by a permitted call only wastes the time
([`TestModel_RetryWaitThatCannotFitIsUnavailable`](../../retry_test.go)).

## What this does not claim

- The estimate of one token per three bytes is meant to err high for
  ordinary text. It is not a bound proven for every tokenizer and every
  language.
- Cost is computed from the consumer's price table. A model missing from
  the table costs nothing, so a spend cap does not bind for it. The
  shared price lookup refuses an unpriced paid model
  ([`TestPriceFor_UnpricedModelRefused`](../../providers/prices_test.go)),
  but a consumer that bypasses it gets a free model.
- The totals are the runtime's books, not the provider's invoice. Charging
  unknown attempts high means the totals can exceed what was actually
  billed, by design.
- A process that dies during a model call leaves the attempt's row
  recorded as `dispatched`. That attempt is not added to the run's totals,
  and nothing charges it when the run is resumed. The record shows the
  attempt; the totals do not include it.
