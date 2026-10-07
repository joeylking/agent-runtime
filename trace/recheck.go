package trace

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
)

// maxRecheckField is how many bytes of one value a re-check line shows.
const maxRecheckField = 240

// WriteRecheck writes a re-check report as text for a terminal: a header
// line with the run, its status, how many of the evaluations re-checked
// differ, what is known of the policy's identity, and which specs the
// requests carried; then one line per evaluation, the ones that differ
// first, each marked DIFFERENT, same, or skipped, with the step, the
// event's seq, the tool, and what the evaluation came to; then the
// report's notes. Every value the record or a policy chose is sanitized
// and cut at 240 bytes, so each evaluation is one line however it was
// recorded. The JSON form is the report's own MarshalJSON.
func WriteRecheck(w io.Writer, rep agentrt.RecheckReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s %s: %d of %d re-checked evaluations differ; %s; %s specs\n",
		field(rep.RunID), field(string(rep.RunStatus)), rep.Differences(), rep.Rechecked(), identity(rep), rep.SpecSource)
	for _, e := range rep.Evaluations {
		if e.Result == agentrt.RecheckDifferent {
			recheckLine(&b, e)
		}
	}
	for _, e := range rep.Evaluations {
		if e.Result != agentrt.RecheckDifferent {
			recheckLine(&b, e)
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(&b, "note: %s\n", field(n))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func identity(rep agentrt.RecheckReport) string {
	switch rep.Identity {
	case agentrt.IdentityChanged:
		var recorded []string
		seen := map[string]bool{}
		for _, e := range rep.Evaluations {
			if (e.Result == agentrt.RecheckSame || e.Result == agentrt.RecheckDifferent) && e.PolicyID != rep.PolicyID && !seen[e.PolicyID] {
				seen[e.PolicyID] = true
				recorded = append(recorded, `"`+field(e.PolicyID)+`"`)
			}
		}
		return fmt.Sprintf("policy changed: recorded %s, given \"%s\"", strings.Join(recorded, ", "), field(rep.PolicyID))
	case agentrt.IdentityUnchanged:
		return `policy unchanged: "` + field(rep.PolicyID) + `"`
	}
	return "policy identity unknown"
}

func recheckLine(b *strings.Builder, e agentrt.RecheckedEvaluation) {
	mark := "skipped  "
	switch e.Result {
	case agentrt.RecheckDifferent:
		mark = "DIFFERENT"
	case agentrt.RecheckSame:
		mark = "same     "
	}
	tool := field(e.Tool)
	if tool == "" {
		tool = "-"
	}
	if e.OnResume {
		tool += " (on resume)"
	}
	fmt.Fprintf(b, "%s step %d seq %d %s: ", mark, e.StepIndex, e.Seq, tool)
	switch e.Result {
	case agentrt.RecheckSame:
		b.WriteString(field(string(e.Recorded.Outcome)))
	case agentrt.RecheckDifferent:
		now := "none"
		if e.Rechecked != nil {
			now = field(string(e.Rechecked.Outcome))
		}
		why := e.Detail
		if why == "" && e.Rechecked != nil {
			why = e.Rechecked.Reason
		}
		fmt.Fprintf(b, "%s -> %s", field(string(e.Recorded.Outcome)), now)
		if why != "" {
			b.WriteString(": " + field(why))
		}
	default:
		b.WriteString(field(e.Detail))
	}
	b.WriteByte('\n')
}

// field is s sanitized for one terminal line and cut at maxRecheckField
// bytes on a character boundary.
func field(s string) string {
	s = Sanitize(s)
	if len(s) <= maxRecheckField {
		return s
	}
	n := maxRecheckField
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
