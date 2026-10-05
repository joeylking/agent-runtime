package proxy

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/joeylking/agent-runtime/render"
	"github.com/joeylking/agent-runtime/trace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// What the model is told for every outcome but a result is prose with a
// stable prefix, so a host or a test can recognise it. The wording follows
// a measurement of local models over 96 runs (docs/decisions/0008): after
// a prose pending result they stopped and reported, and re-called with
// identical arguments once told it was approved, where a JSON-shaped one
// made them poll and ask the user to approve. Nothing here names the
// operator command, the database, the configuration, or a run id: the
// model must not be handed the way to approve its own request.
const (
	PrefixPending     = "PENDING_APPROVAL"
	PrefixDenied      = "DENIED"
	PrefixInvalid     = "INVALID"
	PrefixInterrupted = "INTERRUPTED"
	PrefixUnknown     = "UNKNOWN_OUTCOME"
	PrefixUnrecorded  = "UNRECORDED"
	PrefixInProgress  = "IN_PROGRESS"
	PrefixRejected    = "REJECTED"
	PrefixExpired     = "EXPIRED"
	PrefixDuplicate   = "DUPLICATE"
	PrefixBlocked     = "BLOCKED"
	PrefixUnavailable = "UNAVAILABLE"
)

// maxResult bounds server-chosen text the proxy quotes in prose: the cap
// the mcp package puts on a recorded result.
const maxResult = 64 << 10

// clean is text from a model, a server, or an operator as the model is
// shown it in the proxy's prose: escaped with trace.Sanitize, which also
// turns a newline into a space, and bounded. A result returned as the
// call's result is data and is passed back as recorded.
func clean(s string, n int) string {
	return render.Truncate(trace.Sanitize(s), n)
}

func textResult(isError bool, format string, args ...any) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: isError, Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func pendingText(approvalID string) *sdk.CallToolResult {
	return textResult(false, "%s: this request is waiting for an operator's approval (approval %s). Nothing has been executed. "+
		"Do not call this tool again until you have been told it was approved, do not try another tool to achieve the same thing, "+
		"and do not ask the user to approve it: the operator decides elsewhere. Tell the user it is waiting. "+
		"After approval, call this tool again with exactly the same arguments to run it and get the result.",
		PrefixPending, approvalID)
}

func interruptedText(approvalID string) *sdk.CallToolResult {
	return textResult(false, "%s: an earlier attempt of this exact request was cut off and its outcome is unknown; "+
		"an operator must decide whether it runs again (approval %s); nothing further has been executed. "+
		"Do not call this tool again until you have been told the operator decided, and do not try another tool to achieve the same thing. "+
		"Tell the user. If you are told it was approved, call this tool again with exactly the same arguments.",
		PrefixInterrupted, approvalID)
}

// unknownText tells the model a call to a tool that changes something may
// have taken effect without the proxy learning its outcome, and, with an
// approval, that an operator decides whether it runs again. errText is
// the recorded error, when this call is the one that ended so.
func unknownText(how, errText, approvalID string) *sdk.CallToolResult {
	e := ""
	if errText != "" {
		e = fmt.Sprintf(" (%s)", clean(errText, 500))
	}
	next := "An identical call will wait for an operator to decide whether it runs again."
	if approvalID != "" {
		next = fmt.Sprintf("An operator must decide whether it runs again (approval %s).", approvalID)
	}
	return textResult(false, "%s: an attempt of this exact request %s%s, so whether it took effect is unknown: it may have. "+
		"%s Nothing further has been executed. "+
		"Do not call this tool again until you have been told the operator decided, and do not try another tool to achieve the same thing. "+
		"Tell the user. If you are told it was approved, call this tool again with exactly the same arguments.",
		PrefixUnknown, how, e, next)
}

// unrecordedText tells the model a call to a tool that changes something
// executed, since the server answered, but its answer could not be
// recorded, so neither it nor the gate knows what came of it.
func unrecordedText(reason string, window time.Duration) *sdk.CallToolResult {
	dup := ""
	if window > 0 {
		dup = fmt.Sprintf(" An identical call within %s is refused as a duplicate.", window)
	}
	return textResult(false, "%s: the server answered this request, so it executed, but its answer could not be recorded (%s). "+
		"What it did is not known here: it may have taken effect. Do not call this tool again to retry it.%s Tell the user.",
		PrefixUnrecorded, clean(reason, 500), dup)
}

func inProgressText() *sdk.CallToolResult {
	return textResult(false, "%s: this exact request is already being executed, or the outcome of an attempt of it is not yet known. "+
		"Nothing new was started. Wait a little, then call this tool again with exactly the same arguments to learn its outcome.",
		PrefixInProgress)
}

func deniedText(reason string) *sdk.CallToolResult {
	return textResult(true, "%s: the policy does not allow this request: %s. Nothing was executed.", PrefixDenied, clean(reason, 500))
}

// schemaURL is the address the runtime compiles a tool's schema under,
// which its validation errors quote. It names nothing the model can use,
// but it names the runtime, so it is cut from what the model is shown.
var schemaURL = regexp.MustCompile(` with '[a-z]+://tools/[^']*'`)

func invalidText(reason string) *sdk.CallToolResult {
	return textResult(true, "%s: the arguments do not match the tool's input schema: %s. Nothing was executed. Correct the arguments and call again.",
		PrefixInvalid, clean(schemaURL.ReplaceAllString(reason, ""), 1000))
}

func rejectedText(approvalID, note string, until time.Time) *sdk.CallToolResult {
	n := ""
	if strings.TrimSpace(note) != "" {
		n = fmt.Sprintf(" The operator's note: %q.", clean(note, 500))
	}
	return textResult(true, "%s: an operator rejected this exact request (approval %s).%s It was not executed. "+
		"An identical request is refused until %s; do not ask for it again, and tell the user.",
		PrefixRejected, approvalID, n, until.UTC().Format(time.RFC3339))
}

func cancelledText(note string, until time.Time) *sdk.CallToolResult {
	n := ""
	if strings.TrimSpace(note) != "" {
		n = fmt.Sprintf(" The operator's note: %q.", clean(note, 500))
	}
	return textResult(true, "%s: an operator cancelled this exact request.%s Nothing further was executed. "+
		"An identical request is refused until %s; do not ask for it again, and tell the user.",
		PrefixRejected, n, until.UTC().Format(time.RFC3339))
}

func expiredText(approvalID string, until time.Time) *sdk.CallToolResult {
	return textResult(true, "%s: the approval for this exact request (approval %s) expired before it was used. It was not executed. "+
		"An identical request is refused until %s; tell the user.",
		PrefixExpired, approvalID, until.UTC().Format(time.RFC3339))
}

// duplicateText tells the model when the earlier call ran and what came of
// it, so it can use that outcome instead of the call it repeated.
func duplicateText(at time.Time, window time.Duration, outcome string) *sdk.CallToolResult {
	return textResult(true, "%s: this exact request already executed at %s and is not executed again: "+
		"an identical call to this tool within %s of an earlier one is refused, whether it is a retry or meant as a new one. "+
		"Its outcome was: %s",
		PrefixDuplicate, at.UTC().Format(time.RFC3339), window, outcome)
}

func blockedInterruptedText(tool, approvalID string) *sdk.CallToolResult {
	return textResult(true, "%s: an earlier call to %s may have taken effect and its outcome is unknown (approval %s), "+
		"so no other call to %s that changes anything runs until an operator decides it. Nothing was executed. Tell the user.",
		PrefixBlocked, tool, approvalID, tool)
}

func blockedPendingText(max int) *sdk.CallToolResult {
	return textResult(true, "%s: %d requests are already waiting for an operator's approval, the most this session allows. "+
		"Nothing was executed and no approval was requested. Tell the user, and wait for the waiting requests to be decided.",
		PrefixBlocked, max)
}

func unavailableText(what string) *sdk.CallToolResult {
	return textResult(true, "%s: %s. Tell the user.", PrefixUnavailable, what)
}
