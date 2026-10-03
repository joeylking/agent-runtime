package bench

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func column(commit string, at time.Time, model string, trials ...Trial) *File {
	for i := range trials {
		trials[i].Mode, trials[i].Model = "model", model
	}
	return &File{Commit: commit, StartedAt: at, Taxonomy: repoSteward, Trials: trials, Source: commit + "-" + model + ".json"}
}

func TestLatest_KeepsTheNewestPerModeAndModel(t *testing.T) {
	old := column("aaa", start, "m1", Trial{Scenario: "S1", Repeat: 1, Outcome: "failed"})
	newer := column("bbb", start.Add(time.Hour), "m1", Trial{Scenario: "S1", Repeat: 1, Outcome: "completed", Expect: "proposal"})
	other := column("aaa", start, "m2", Trial{Scenario: "S1", Repeat: 1, Outcome: "failed"})
	cols := Latest([]*File{newer, old, other})
	if len(cols) != 2 || cols[0].File != newer || cols[1].File != other || cols[0].name() != "model m1" {
		t.Fatalf("cols = %+v", cols)
	}
}

func TestCompare_RefusesMixedCommitsUnlessAskedAndNamesEach(t *testing.T) {
	a := column("aaa", start, "m1", Trial{Scenario: "S1", Repeat: 1, Outcome: "completed", Expect: "proposal"})
	b := column("bbb", start, "m2", Trial{Scenario: "S1", Repeat: 1, Outcome: "failed", Expect: "proposal"})
	unknown := column("", start, "m3", Trial{Scenario: "S1", Repeat: 1, Outcome: "failed", Expect: "proposal"})
	if _, err := Compare(Latest([]*File{a, b}), CompareOptions{}); !errors.Is(err, ErrMixedCommits) || !strings.Contains(err.Error(), "aaa, bbb") {
		t.Fatalf("err = %v, want ErrMixedCommits naming both", err)
	}
	if _, err := Compare(Latest([]*File{a, unknown}), CompareOptions{}); !errors.Is(err, ErrMixedCommits) {
		t.Fatalf("an unrecorded commit compared with a known one: err = %v", err)
	}
	md, err := Compare(Latest([]*File{a, b}), CompareOptions{AllowMixedCommits: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"come from 2 commits (aaa, bbb) and are not comparable", "| model m1 | aaa |", "| model m2 | bbb |", "model m1 @ aaa", "model m2 @ bbb"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	same, err := Compare(Latest([]*File{a}), CompareOptions{})
	if err != nil || strings.Contains(same, "not comparable") || !strings.Contains(same, "model m1 @ aaa") {
		t.Fatalf("one commit: err = %v\n%s", err, same)
	}
}

func TestCompare_RefusesMixedTaxonomiesEvenWhenAsked(t *testing.T) {
	a := column("aaa", start, "m1", Trial{Scenario: "S1", Repeat: 1, Outcome: "failed"})
	b := &File{Commit: "aaa", StartedAt: start, Taxonomy: casework, Trials: []Trial{{Scenario: "s1", Mode: "replay", Repeat: 1, Outcome: "error"}}}
	if _, err := Compare(Latest([]*File{a, b}), CompareOptions{AllowMixedCommits: true}); !errors.Is(err, ErrMixedTaxonomies) {
		t.Fatalf("err = %v", err)
	}
}

func TestCompare_DenominatorsAndMixedCellsAreSpelledOut(t *testing.T) {
	f := column("aaa", start, "m1",
		Trial{Scenario: "S10H", Repeat: 1, Expect: "refusal", Reached: "scope_exceeded", Outcome: "correct_refusal", Steps: 6},
		Trial{Scenario: "S10H", Repeat: 2, Expect: "refusal", Reached: "scope_exceeded", Outcome: "correct_refusal", Steps: 9},
		Trial{Scenario: "S2", Repeat: 1, Expect: "proposal", Reached: "proposal_prepared", Outcome: "completed", Steps: 10},
		Trial{Scenario: "S2", Repeat: 2, Expect: "proposal", Reached: "blocked", Outcome: "incorrect_refusal", Steps: 10},
	)
	md, err := Compare(Latest([]*File{f}), CompareOptions{LinkPrefix: "results/"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Completed, Incorrect refusals over trials expecting proposal",
		"| model m1, 2 repeats | aaa | 1/2 | 0/4 | 1/2 | 2/2 | 0/4 | 0/4 | 0/4 | 0/4 |",
		"[aaa-m1.json](results/aaa-m1.json)",
		"| S2 | mixed: proposal_prepared (completed)<br>blocked (incorrect_refusal) |",
		"| S10H | scope_exceeded (correct_refusal) ×2 |",
		"| S2 | model m1 @ aaa | mixed: `completed`×1, `incorrect_refusal`×1 |",
		"| S10H | model m1 @ aaa | `correct_refusal` | scope_exceeded | 7.5 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	if strings.Index(md, "| S2 |") > strings.Index(md, "| S10H |") {
		t.Error("S10H sorted before S2")
	}
}

func TestCompare_OrderPutsNamedModesFirst(t *testing.T) {
	f := &File{Commit: "aaa", StartedAt: start, Taxonomy: casework}
	for _, m := range []string{"replay", "zeta", "scripted", "baseline", "alpha"} {
		f.Trials = append(f.Trials, Trial{Scenario: "s1", Mode: m, Repeat: 1, Outcome: "error"})
	}
	md, err := Compare(Latest([]*File{f}), CompareOptions{Order: []string{"baseline", "scripted", "replay"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "| Scenario | baseline @ aaa | scripted @ aaa | replay @ aaa | alpha @ aaa | zeta @ aaa |") {
		t.Errorf("order wrong:\n%s", md)
	}
}

func TestCompare_ProvenanceCarriesOptionsAndNotes(t *testing.T) {
	f := column("aaa", start, "m1", Trial{Scenario: "S1", Repeat: 1, Outcome: "failed"})
	f.Options = map[string]json.RawMessage{"max_total_calls": json.RawMessage("40"), "max_model_calls_per_run": json.RawMessage("80")}
	f.Notes = []string{"stopped before S2 repeat 1: total call budget 40 reached"}
	md, err := Compare(Latest([]*File{f}), CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "- **model m1**: commit aaa, started 2026-09-25T20:00:00Z, file aaa-m1.json; max_model_calls_per_run=80, max_total_calls=40.\n  - stopped before S2 repeat 1") {
		t.Errorf("provenance:\n%s", md)
	}
}
