package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Markdown renders the file: its provenance, one section per mode and
// model with every outcome over its denominator, a row per trial, and the
// run-level notes. Both consumers write a Markdown file beside the JSON.
func (f *File) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Results: %s\n\nStarted %s", f.Taxonomy.Name, f.StartedAt.UTC().Format("2006-01-02T15:04:05Z"))
	if f.Commit != "" {
		fmt.Fprintf(&b, " at commit %s", f.Commit)
	} else {
		b.WriteString(", no commit recorded")
	}
	b.WriteString(".")
	if opts := options(f.Options); opts != "" {
		fmt.Fprintf(&b, " Options: %s.", opts)
	}
	b.WriteString("\n\n")
	for _, s := range f.Summarize() {
		f.section(&b, s)
	}
	for _, n := range f.Notes {
		fmt.Fprintf(&b, "%s\n\n", n)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (f *File) section(b *strings.Builder, s Summary) {
	fmt.Fprintf(b, "## %s\n\n", Column{Mode: s.Mode, Model: s.Model}.name())
	fmt.Fprintf(b, "%d trials over %d scenarios, %d judged", s.Trials, s.Scenarios, s.Judged)
	if s.Excluded > 0 {
		fmt.Fprintf(b, ", %d excluded and in no denominator", s.Excluded)
	}
	b.WriteString(".")
	if s.Disqualified() {
		fmt.Fprintf(b, " **Disqualified: %d unsafe outcome(s).**", s.Unsafe)
	}
	b.WriteString("\n\n| Outcome | Class | Trials | Denominator |\n|---|---|---:|---|\n")
	for i, c := range s.Counts {
		who := "judged trials"
		if c.Over != "" {
			who = "trials expecting " + c.Over
		}
		fmt.Fprintf(b, "| %s | %s | %d | %d %s |\n", f.Taxonomy.Outcomes[i].label(), c.Class, c.N, c.Of, who)
	}
	fmt.Fprintf(b, "| Unsafe, any | %s | %d | %d judged trials |\n\n", Unsafe, s.Unsafe, s.Judged)
	fmt.Fprintf(b, "- policy denials: %d\n", s.PolicyDenials)
	keys := make([]string, 0, len(s.Totals))
	for k := range s.Totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "- %s: %d\n", k, s.Totals[k])
	}
	fmt.Fprintf(b, "- model calls: %d, tokens %d in / %d out\n", s.ModelCalls, s.InputTokens, s.OutputTokens)
	fmt.Fprintf(b, "- cost: $%.4f\n", float64(s.CostMicros)/1e6)
	fmt.Fprintf(b, "- mean steps per judged trial: %.1f\n", s.MeanSteps())
	fmt.Fprintf(b, "- wall time: %s\n\n", duration(float64(s.Wall)))
	b.WriteString("| Scenario | Repeat | Reached | Outcome | Steps | Tool calls | Policy denials | Model calls | Tokens in/out | Cost (USD) | Wall |\n|---|---:|---|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, t := range f.Trials {
		if t.Mode != s.Mode || t.Model != s.Model {
			continue
		}
		outcome := t.Outcome
		if outcome == "" {
			outcome = "_excluded: " + t.Excluded + "_"
		}
		fmt.Fprintf(b, "| %s | %d | %s | %s | %d | %d | %d | %d | %d/%d | %.4f | %s |\n", t.Scenario, t.Repeat, orDash(t.Reached), outcome, t.Steps, t.ToolCalls, t.PolicyDenials, t.ModelCalls, t.InputTokens, t.OutputTokens, float64(t.CostMicros)/1e6, duration(float64(t.Wall)))
	}
	b.WriteString("\n")
}

// options renders a file's options as sorted key=value pairs.
func options(o map[string]json.RawMessage) string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		var v bytes.Buffer
		if json.Compact(&v, o[k]) != nil {
			v.Write(o[k])
		}
		parts = append(parts, k+"="+v.String())
	}
	return strings.Join(parts, ", ")
}

// duration rounds to the millisecond under a second and to the second
// above it, which is what each consumer's runs are measured in.
func duration(ns float64) string {
	d := time.Duration(ns)
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func num(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.1f", v)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// naturalLess orders scenario ids with their digit runs compared as
// numbers, so S2 sorts before S10 and S4 before S4M.
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := digits(a), digits(b)
		if da > 0 && db > 0 {
			na, nb := strings.TrimLeft(a[:da], "0"), strings.TrimLeft(b[:db], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[da:], b[db:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}
