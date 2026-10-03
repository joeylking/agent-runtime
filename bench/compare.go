package bench

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Column is one mode and model's trials from one file: a column of a
// comparison.
type Column struct {
	Mode  string
	Model string
	File  *File
}

// name is the mode, followed by the model when there is one.
func (c Column) name() string {
	if c.Model == "" {
		return c.Mode
	}
	return c.Mode + " " + c.Model
}

func (c Column) trials() []Trial {
	var out []Trial
	for _, t := range c.File.Trials {
		if t.Mode == c.Mode && t.Model == c.Model {
			out = append(out, t)
		}
	}
	return out
}

func (c Column) summary() Summary {
	return summarize(c.File.Taxonomy, c.Mode, c.Model, c.trials())
}

// Latest returns the newest column per mode and model across files, by
// start time and then by Source, sorted by mode and model. repo-steward's
// summarize keeps the newest result per mode and model this way.
func Latest(files []*File) []Column {
	type key struct{ mode, model string }
	best := map[key]Column{}
	for _, f := range files {
		for _, s := range f.Summarize() {
			k := key{s.Mode, s.Model}
			cur, ok := best[k]
			if !ok || f.StartedAt.After(cur.File.StartedAt) || (f.StartedAt.Equal(cur.File.StartedAt) && f.Source > cur.File.Source) {
				best[k] = Column{Mode: s.Mode, Model: s.Model, File: f}
			}
		}
	}
	out := make([]Column, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mode != out[j].Mode {
			return out[i].Mode < out[j].Mode
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// CompareOptions configures Compare.
type CompareOptions struct {
	// Order lists modes to put first, in this order; the rest follow by
	// mode and model. repo-steward puts baseline and scripted first;
	// casework reports baseline, scripted, replay.
	Order []string
	// AllowMixedCommits accepts columns from different commits. The
	// output then says, above the tables, that they are not comparable
	// as like for like. repo-steward's committed comparison mixes three
	// commits and says why in prose.
	AllowMixedCommits bool
	// LinkPrefix, when set, links each column's Source as
	// LinkPrefix+Source, as repo-steward links into results/.
	LinkPrefix string
}

var (
	// ErrMixedCommits is returned when the columns come from more than one
	// commit and CompareOptions.AllowMixedCommits is not set. Files with
	// no commit share one unknown commit, which is not any named one.
	ErrMixedCommits = errors.New("bench: results from different commits are not comparable")
	// ErrMixedTaxonomies is returned when the columns were scored by
	// different outcome sets. No option accepts this.
	ErrMixedTaxonomies = errors.New("bench: results scored by different taxonomies are not comparable")
)

// Compare renders columns as Markdown: a table of outcomes with their
// denominators per column, a per-scenario matrix, the means of every
// scenario's repetitions, and each column's provenance. It refuses
// columns from different commits unless o.AllowMixedCommits is set, and
// names each column's commit either way.
func Compare(cols []Column, o CompareOptions) (string, error) {
	if len(cols) == 0 {
		return "", errors.New("bench: nothing to compare")
	}
	tax := cols[0].File.Taxonomy
	commits := map[string]bool{}
	for _, c := range cols {
		if !c.File.Taxonomy.equal(tax) {
			return "", fmt.Errorf("%w: %s and %s", ErrMixedTaxonomies, tax.Name, c.File.Taxonomy.Name)
		}
		commits[c.File.Commit] = true
	}
	if len(commits) > 1 && !o.AllowMixedCommits {
		return "", fmt.Errorf("%w: %s", ErrMixedCommits, commitList(commits))
	}
	cols = ordered(cols, o.Order)
	var b strings.Builder
	if len(commits) > 1 {
		fmt.Fprintf(&b, "**These columns come from %d commits (%s) and are not comparable as like for like.** Each column names its commit.\n\n", len(commits), commitList(commits))
	}
	overview(&b, tax, cols, o)
	matrix(&b, cols)
	means(&b, cols)
	provenance(&b, cols)
	return b.String(), nil
}

func commitList(commits map[string]bool) string {
	var out []string
	for c := range commits {
		out = append(out, commitName(c))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func commitName(c string) string {
	if c == "" {
		return "no commit recorded"
	}
	return c
}

func ordered(cols []Column, order []string) []Column {
	rank := func(mode string) int {
		for i, m := range order {
			if m == mode {
				return i
			}
		}
		return len(order)
	}
	out := append([]Column(nil), cols...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i].Mode), rank(out[j].Mode)
		if ri != rj {
			return ri < rj
		}
		if ri < len(order) {
			return false
		}
		if out[i].Mode != out[j].Mode {
			return out[i].Mode < out[j].Mode
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func denominatorSentence(tax Taxonomy) string {
	over := map[string][]string{}
	var keys []string
	for _, o := range tax.Outcomes {
		if _, ok := over[o.Over]; !ok {
			keys = append(keys, o.Over)
		}
		over[o.Over] = append(over[o.Over], o.label())
	}
	var parts []string
	for _, k := range keys {
		who := "every judged trial"
		if k != "" {
			who = "trials expecting " + k
		}
		parts = append(parts, strings.Join(over[k], ", ")+" over "+who)
	}
	return "Each cell is a count over its denominator: " + strings.Join(parts, "; ") + ". Outcomes are never added together."
}

func overview(b *strings.Builder, tax Taxonomy, cols []Column, o CompareOptions) {
	b.WriteString(denominatorSentence(tax) + " A mode with any unsafe outcome is disqualified.\n\n| Column | Commit |")
	for _, oc := range tax.Outcomes {
		fmt.Fprintf(b, " %s |", oc.label())
	}
	b.WriteString(" Unsafe | Excluded | Model calls | Cost (USD) | File |\n|---|---|")
	for range tax.Outcomes {
		b.WriteString("---|")
	}
	b.WriteString("---|---|---|---|---|\n")
	for _, c := range cols {
		s := c.summary()
		name := c.name()
		if s.Repeats > 1 {
			name += fmt.Sprintf(", %d repeats", s.Repeats)
		}
		if s.Disqualified() {
			name += " (disqualified)"
		}
		fmt.Fprintf(b, "| %s | %s |", name, commitName(c.File.Commit))
		for _, n := range s.Counts {
			fmt.Fprintf(b, " %d/%d |", n.N, n.Of)
		}
		fmt.Fprintf(b, " %d/%d | %d/%d | %d | %.4f | %s |\n", s.Unsafe, s.Judged, s.Excluded, s.Trials, s.ModelCalls, float64(s.CostMicros)/1e6, link(c.File.Source, o.LinkPrefix))
	}
	b.WriteString("\n")
}

func link(source, prefix string) string {
	switch {
	case source == "":
		return "—"
	case prefix == "":
		return source
	}
	return "[" + source + "](" + prefix + source + ")"
}

func header(c Column) string {
	return c.name() + " @ " + commitName(c.File.Commit)
}

func scenarios(cols []Column) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cols {
		for _, t := range c.trials() {
			if !seen[t.Scenario] {
				seen[t.Scenario] = true
				out = append(out, t.Scenario)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return naturalLess(out[i], out[j]) })
	return out
}

func byScenario(c Column, id string) []Trial {
	var out []Trial
	for _, t := range c.trials() {
		if t.Scenario == id {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repeat < out[j].Repeat })
	return out
}

// matrix is one row per scenario and one cell per column: what each
// repetition reached and how it was scored, collapsed when they agree.
func matrix(b *strings.Builder, cols []Column) {
	b.WriteString("Per scenario, what each repetition reached and its outcome:\n\n| Scenario |")
	for _, c := range cols {
		fmt.Fprintf(b, " %s |", header(c))
	}
	b.WriteString("\n|---|")
	for range cols {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, id := range scenarios(cols) {
		fmt.Fprintf(b, "| %s |", id)
		for _, c := range cols {
			fmt.Fprintf(b, " %s |", matrixCell(byScenario(c, id)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func matrixCell(group []Trial) string {
	if len(group) == 0 {
		return "—"
	}
	cells := make([]string, len(group))
	for i, t := range group {
		if t.Outcome == "" {
			cells[i] = "_excluded: " + t.Excluded + "_"
			continue
		}
		cells[i] = orDash(t.Reached) + " (" + t.Outcome + ")"
	}
	same := true
	for _, x := range cells[1:] {
		same = same && x == cells[0]
	}
	switch {
	case same && len(cells) == 1:
		return cells[0]
	case same:
		return fmt.Sprintf("%s ×%d", cells[0], len(cells))
	}
	return "mixed: " + strings.Join(cells, "<br>")
}

// means is one row per scenario and column with every number averaged
// over the judged repetitions, and the outcome marked mixed when they
// disagreed.
func means(b *strings.Builder, cols []Column) {
	b.WriteString("Means across repetitions:\n\n| Scenario | Column | Outcome | Reached | Steps | Tool calls | Policy denials | Model calls | Tokens in/out | Cost (USD) | Wall |\n|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, id := range scenarios(cols) {
		for _, c := range cols {
			group := byScenario(c, id)
			if len(group) == 0 {
				continue
			}
			fmt.Fprintln(b, "| "+strings.Join(meanCells(id, header(c), c.File.Taxonomy, group), " | ")+" |")
		}
	}
	b.WriteString("\n")
}

func meanCells(id, col string, tax Taxonomy, group []Trial) []string {
	var judged []Trial
	for _, t := range group {
		if t.Outcome != "" {
			judged = append(judged, t)
		}
	}
	if len(judged) == 0 {
		return []string{id, col, "_excluded: " + group[0].Excluded + "_", "—", "—", "—", "—", "—", "—", "—", "—"}
	}
	n := float64(len(judged))
	var steps, tools, denials, calls, in, out, cost, wall float64
	for _, t := range judged {
		steps += float64(t.Steps)
		tools += float64(t.ToolCalls)
		denials += float64(t.PolicyDenials)
		calls += float64(t.ModelCalls)
		in += float64(t.InputTokens)
		out += float64(t.OutputTokens)
		cost += float64(t.CostMicros)
		wall += float64(t.Wall)
	}
	outcome := outcomeCell(tax, judged)
	if x := len(group) - len(judged); x > 0 {
		outcome += fmt.Sprintf(" (%d excluded)", x)
	}
	return []string{
		id, col, outcome,
		orDash(mostFrequent(judged, func(t Trial) string { return t.Reached })),
		num(steps / n), num(tools / n), num(denials / n), num(calls / n),
		num(in/n) + "/" + num(out/n),
		fmt.Sprintf("%.4f", cost/n/1e6),
		duration(wall / n),
	}
}

// outcomeCell is the outcome when the repetitions agreed and the spread,
// in taxonomy order, when they did not.
func outcomeCell(tax Taxonomy, judged []Trial) string {
	counts := map[string]int{}
	for _, t := range judged {
		counts[t.Outcome]++
	}
	if len(counts) == 1 {
		return "`" + judged[0].Outcome + "`"
	}
	var parts []string
	for _, o := range tax.Outcomes {
		if counts[o.Name] > 0 {
			parts = append(parts, fmt.Sprintf("`%s`×%d", o.Name, counts[o.Name]))
		}
	}
	return "mixed: " + strings.Join(parts, ", ")
}

// mostFrequent is the commonest value, the first in name order on a tie.
func mostFrequent(group []Trial, f func(Trial) string) string {
	counts := map[string]int{}
	for _, t := range group {
		counts[f(t)]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, bestN := "", -1
	for _, k := range keys {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	return best
}

func provenance(b *strings.Builder, cols []Column) {
	b.WriteString("Provenance:\n\n")
	for _, c := range cols {
		f := c.File
		fmt.Fprintf(b, "- **%s**: commit %s, started %s", c.name(), commitName(f.Commit), f.StartedAt.UTC().Format("2006-01-02T15:04:05Z"))
		if f.Source != "" {
			fmt.Fprintf(b, ", file %s", f.Source)
		}
		if opts := options(f.Options); opts != "" {
			fmt.Fprintf(b, "; %s", opts)
		}
		b.WriteString(".\n")
		for _, n := range f.Notes {
			fmt.Fprintf(b, "  - %s\n", n)
		}
	}
}
