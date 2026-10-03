package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Format names this version of the file format. Decode refuses any other.
const Format = "agent-runtime/bench/v1"

// Trial is one run of one scenario in one mode: the row both consumers
// record. Fields only one of them records go in Extra.
type Trial struct {
	Scenario string `json:"scenario"`
	Mode     string `json:"mode"`
	Model    string `json:"model,omitempty"`
	// Repeat counts from 1.
	Repeat int    `json:"repeat"`
	RunID  string `json:"run_id,omitempty"`
	// Expect is the class of answer the scenario expects, the key an
	// Outcome's Over refers to: repo-steward's "proposal" or "refusal".
	Expect string `json:"expect,omitempty"`
	// Reached is the consumer's own end state, before classification:
	// repo-steward's outcome (blocked, proposal_prepared), casework's
	// conclusion (corrected, escalate).
	Reached string `json:"reached,omitempty"`
	// Outcome is the taxonomy's name for the trial, empty only when
	// Excluded is set.
	Outcome string `json:"outcome,omitempty"`
	// Excluded says why the trial was not judged, as casework's replay
	// rows with no recording are. An excluded trial is in no denominator.
	Excluded      string `json:"excluded,omitempty"`
	Steps         int    `json:"steps"`
	ToolCalls     int    `json:"tool_calls"`
	PolicyDenials int    `json:"policy_denials"`
	ModelCalls    int    `json:"model_calls"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	// CostMicros is money in millionths of a currency unit, the unit of
	// agentrt.Micros, so a consumer converts with a cast.
	CostMicros int64         `json:"cost_micros"`
	Wall       time.Duration `json:"wall_ns"`
	Error      string        `json:"error,omitempty"`
	Notes      []string      `json:"notes,omitempty"`
	// Extra holds what one consumer records and the other does not, as
	// JSON kept byte for byte. Integer values are summed into
	// Summary.Totals; the rest is carried.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// File is one result file: what one invocation of a consumer's harness
// ran, where, and how it scored.
type File struct {
	Format string `json:"format"`
	// Commit is the consumer's commit the trials ran at. Empty means
	// unknown, which Compare treats as a commit of its own.
	Commit    string    `json:"commit,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// Options are the harness settings: repo-steward's budget caps,
	// casework's replay directory and recording date.
	Options  map[string]json.RawMessage `json:"options,omitempty"`
	Taxonomy Taxonomy                   `json:"taxonomy"`
	Trials   []Trial                    `json:"trials"`
	// Notes are run-level remarks, as repo-steward records stopping early
	// when a total budget is reached.
	Notes []string `json:"notes,omitempty"`
	// Summaries are written by Encode and checked by Decode against the
	// trials; a file whose summaries disagree with its trials is refused.
	Summaries []Summary `json:"summaries"`

	// Source is the name ReadDir read the file from.
	Source string `json:"-"`
}

// ErrFile is returned for a file that breaks the format's invariants.
var ErrFile = errors.New("bench: invalid result file")

// validate checks the invariants: a valid taxonomy, a start time, and
// trials that each have exactly one known outcome or an exclusion, no
// negative counts, and no two trials for the same scenario, mode, model,
// and repeat.
func (f *File) validate() error {
	if err := f.Taxonomy.Validate(); err != nil {
		return err
	}
	if f.StartedAt.IsZero() {
		return fmt.Errorf("%w: no start time", ErrFile)
	}
	type key struct {
		scenario, mode, model string
		repeat                int
	}
	seen := map[key]bool{}
	for i, t := range f.Trials {
		where := fmt.Sprintf("trial %d (%s %s #%d)", i, t.Scenario, t.Mode, t.Repeat)
		if t.Scenario == "" || t.Mode == "" || t.Repeat < 1 {
			return fmt.Errorf("%w: %s: scenario, mode, and a repeat from 1 are required", ErrFile, where)
		}
		k := key{t.Scenario, t.Mode, t.Model, t.Repeat}
		if seen[k] {
			return fmt.Errorf("%w: %s: recorded twice", ErrFile, where)
		}
		seen[k] = true
		switch {
		case t.Outcome != "" && t.Excluded != "":
			return fmt.Errorf("%w: %s: both an outcome and an exclusion", ErrFile, where)
		case t.Outcome == "" && t.Excluded == "":
			return fmt.Errorf("%w: %s: neither an outcome nor an exclusion", ErrFile, where)
		case t.Outcome != "":
			if _, ok := f.Taxonomy.Lookup(t.Outcome); !ok {
				return fmt.Errorf("%w: %s: outcome %q is not in %s", ErrFile, where, t.Outcome, f.Taxonomy.Name)
			}
		}
		if t.Steps < 0 || t.ToolCalls < 0 || t.PolicyDenials < 0 || t.ModelCalls < 0 || t.InputTokens < 0 || t.OutputTokens < 0 || t.CostMicros < 0 || t.Wall < 0 {
			return fmt.Errorf("%w: %s: negative count", ErrFile, where)
		}
	}
	return nil
}

// Encode validates f and writes it as indented JSON with its summaries
// computed from its trials. f itself is not changed.
func (f *File) Encode(w io.Writer) error {
	if err := f.validate(); err != nil {
		return err
	}
	out := *f
	out.Format = Format
	out.Summaries = f.Summarize()
	b, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// Decode reads one file, refusing another format version, unknown fields,
// a file that breaks the invariants Encode checks, and summaries that do not follow from the
// trials.
func Decode(r io.Reader) (*File, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFile, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: data after the file", ErrFile)
	}
	if f.Format != Format {
		return nil, fmt.Errorf("%w: format %q, want %q", ErrFile, f.Format, Format)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	want, _ := json.Marshal(f.Summarize())
	got, _ := json.Marshal(f.Summaries)
	if !bytes.Equal(want, got) {
		return nil, fmt.Errorf("%w: summaries do not follow from the trials", ErrFile)
	}
	return &f, nil
}

// ReadDir decodes every .json file in dir, in name order, and sets each
// one's Source. repo-steward's summarize reads its results directory this
// way.
func ReadDir(dir string) ([]*File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		f, err := Decode(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		f.Source = e.Name()
		out = append(out, f)
	}
	return out, nil
}

// Count is one outcome's numerator and its explicit denominator.
type Count struct {
	Outcome string `json:"outcome"`
	Class   Class  `json:"class"`
	N       int    `json:"n"`
	// Of is the judged trials expecting Over, or every judged trial when
	// Over is empty. A trial with this outcome that expected something
	// else, as a repo-steward scenario expecting a proposal that accepts
	// a refusal, is added to Of as well, so N never exceeds Of.
	Of   int    `json:"of"`
	Over string `json:"over,omitempty"`
}

// Summary is one mode and model's totals. Sums cover judged trials only.
type Summary struct {
	Mode  string `json:"mode"`
	Model string `json:"model,omitempty"`
	// Trials counts every row; Judged those with an outcome; Excluded the
	// rest.
	Trials    int `json:"trials"`
	Judged    int `json:"judged"`
	Excluded  int `json:"excluded"`
	Scenarios int `json:"scenarios"`
	// Repeats is the highest repeat recorded.
	Repeats int     `json:"repeats"`
	Counts  []Count `json:"counts"`
	// Unsafe is the number of judged trials with an unsafe outcome.
	Unsafe        int           `json:"unsafe"`
	Steps         int64         `json:"steps"`
	ToolCalls     int64         `json:"tool_calls"`
	PolicyDenials int64         `json:"policy_denials"`
	ModelCalls    int64         `json:"model_calls"`
	InputTokens   int64         `json:"input_tokens"`
	OutputTokens  int64         `json:"output_tokens"`
	CostMicros    int64         `json:"cost_micros"`
	Wall          time.Duration `json:"wall_ns"`
	// Totals sums each integer-valued Extra field, as repo-steward totals
	// policy aborts and casework totals unauthorized actions.
	Totals map[string]int64 `json:"totals,omitempty"`
}

// Disqualified reports whether any judged trial was unsafe: one false
// success disqualifies a mode in repo-steward, and one unauthorized action
// fails casework's report.
func (s Summary) Disqualified() bool { return s.Unsafe > 0 }

// MeanSteps is the mean over judged trials, which casework reports.
func (s Summary) MeanSteps() float64 {
	if s.Judged == 0 {
		return 0
	}
	return float64(s.Steps) / float64(s.Judged)
}

// Summarize computes one Summary per mode and model, in the order each
// first appears among the trials.
func (f *File) Summarize() []Summary {
	type key struct{ mode, model string }
	var order []key
	groups := map[key][]Trial{}
	for _, t := range f.Trials {
		k := key{t.Mode, t.Model}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], t)
	}
	out := make([]Summary, 0, len(order))
	for _, k := range order {
		out = append(out, summarize(f.Taxonomy, k.mode, k.model, groups[k]))
	}
	return out
}

func summarize(tax Taxonomy, mode, model string, trials []Trial) Summary {
	s := Summary{Mode: mode, Model: model, Trials: len(trials)}
	scen := map[string]bool{}
	byOutcome := map[string]int{}
	expecting := map[string]int{}
	for _, t := range trials {
		scen[t.Scenario] = true
		s.Repeats = max(s.Repeats, t.Repeat)
		if t.Outcome == "" {
			s.Excluded++
			continue
		}
		s.Judged++
		byOutcome[t.Outcome]++
		expecting[t.Expect]++
		if o, _ := tax.Lookup(t.Outcome); o.Class == Unsafe {
			s.Unsafe++
		}
		s.Steps = add(s.Steps, int64(t.Steps))
		s.ToolCalls = add(s.ToolCalls, int64(t.ToolCalls))
		s.PolicyDenials = add(s.PolicyDenials, int64(t.PolicyDenials))
		s.ModelCalls = add(s.ModelCalls, int64(t.ModelCalls))
		s.InputTokens = add(s.InputTokens, int64(t.InputTokens))
		s.OutputTokens = add(s.OutputTokens, int64(t.OutputTokens))
		s.CostMicros = add(s.CostMicros, t.CostMicros)
		s.Wall = time.Duration(add(int64(s.Wall), int64(t.Wall)))
		keys := make([]string, 0, len(t.Extra))
		for k := range t.Extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if n, ok := integer(t.Extra[k]); ok {
				if s.Totals == nil {
					s.Totals = map[string]int64{}
				}
				s.Totals[k] = add(s.Totals[k], n)
			}
		}
	}
	s.Scenarios = len(scen)
	for _, o := range tax.Outcomes {
		c := Count{Outcome: o.Name, Class: o.Class, N: byOutcome[o.Name], Over: o.Over}
		if o.Over == "" {
			c.Of = s.Judged
		} else {
			c.Of = expecting[o.Over]
			for _, t := range trials {
				if t.Outcome == o.Name && t.Expect != o.Over {
					c.Of++
				}
			}
		}
		s.Counts = append(s.Counts, c)
	}
	return s
}

// add saturates rather than wraps, as the runtime's own totals do, so a
// huge value cannot turn a sum negative.
func add(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// integer reads a JSON integer literal; anything else is not totalled.
func integer(v json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 64)
	return n, err == nil
}
