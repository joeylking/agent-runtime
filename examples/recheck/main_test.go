package main

import (
	"strings"
	"testing"
)

func TestRun_StricterPolicyStopsThePublishAndVerifyFindsTheAlteredEvent(t *testing.T) {
	var out strings.Builder
	if err := run(&out, t.TempDir()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	s := out.String()
	for _, want := range []string{
		`ran under "rules v1: mutations allowed" and is COMPLETED`,
		"1 of 3 re-checked evaluations differ; policy changed: recorded \"rules v1: mutations allowed\", given \"rules v2: nothing leaves the machine\"; recorded specs",
		"DIFFERENT step 2 seq ",
		" publish: allow -> deny: side effect remote_mutation is deny by policy",
		"same      step 0 seq ",
		"same      step 1 seq ",
		"would have been stopped: step 2, publish\n",
		"  original: head seq ",
		"; intact: ",
		"; broken at seq ",
		"the stored hash is not the one the event's fields and the previous event's hash give",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "would have been stopped") != 1 {
		t.Fatalf("output:\n%s", s)
	}
}
