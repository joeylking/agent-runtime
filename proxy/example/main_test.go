package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if code, ok := role(context.Background(), os.Getenv(roleEnv), os.Args[1:]); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// TestDemo_RunsEveryStep: the demonstration runs end to end, and says
// what it shows: one payment collected after an approval, a cut-off one
// that waits for the operator, and one execution with the proxy against
// two without.
func TestDemo_RunsEveryStep(t *testing.T) {
	var out strings.Builder
	if err := run(context.Background(), t.TempDir(), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{
		"model sees: account household holds",
		"model sees: PENDING_APPROVAL",
		"model sees: paid 950.00 to landlord",
		"model sees: INTERRUPTED",
		"model sees (isError): REJECTED",
		"model sees (isError): DUPLICATE",
		"through the proxy the server made 1 payment",
		"without the proxy the server made 2 payments",
		"every process the demo started has exited",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q", want)
		}
	}
}
