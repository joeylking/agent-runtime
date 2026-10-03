package bench

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden comparisons in testdata")

// Compare's whole output over each fixture is pinned byte for byte, so a
// change to one cell shows as a diff of that cell and nothing else.
func TestCompare_GoldenOverTheFixtures(t *testing.T) {
	for _, name := range []string{"casework", "repo-steward"} {
		f, _ := readTestdata(t, name+".json")
		f.Source = name + ".json"
		md, err := Compare(Latest([]*File{f}), CompareOptions{Order: []string{"baseline", "scripted", "replay"}, LinkPrefix: "results/"})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", name+".compare.md")
		if *update {
			if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if md != string(want) {
			t.Errorf("%s: Compare differs from %s; rerun with -update and review the diff:\n%s", name, path, md)
		}
	}
}
