package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_GrantsThroughTheWebhookAndResumes(t *testing.T) {
	var out strings.Builder
	if err := run(&out, filepath.Join(t.TempDir(), "approver.db"), false); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	s := out.String()
	for _, want := range []string{
		"approval.requested",
		"[bot] > kind    publication",
		"[bot] > tool    publish_proposal",
		"[bot] > ----- end of approval ",
		"[bot] approve by chat:joey: HTTP 202 approved",
		"webhook.decision",
		"approval.decided",
		"is COMPLETED (goal_completed); publish_proposal ran 1 time(s)",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("output lacks %q:\n%s", want, s)
		}
	}
}

func TestRun_RejectCancels(t *testing.T) {
	var out strings.Builder
	if err := run(&out, filepath.Join(t.TempDir(), "approver.db"), true); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	s := out.String()
	for _, want := range []string{"[bot] reject by chat:joey: HTTP 200 rejected", "resume: ", "is CANCELLED (approval_rejected); publish_proposal ran 0 time(s)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output lacks %q:\n%s", want, s)
		}
	}
}
