package providers_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "dial tcp: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestClassifyTransport_TimeoutIsBareAndOthersAreTransient(t *testing.T) {
	var tr agentrt.TransientError
	// A timeout must survive unwrapped: the accounting caller charges it.
	for _, err := range []error{timeoutError{}, fmt.Errorf("post: %w", timeoutError{}), context.DeadlineExceeded} {
		got := providers.ClassifyTransport(err)
		if !errors.Is(got, err) || errors.As(got, &tr) {
			t.Fatalf("ClassifyTransport(%v) = %v, want the error bare", err, got)
		}
	}
	lost := errors.New("connection reset by peer")
	got := providers.ClassifyTransport(lost)
	if !errors.As(got, &tr) || !errors.Is(got, lost) {
		t.Fatalf("ClassifyTransport(%v) = %v, want a transient error wrapping it", lost, got)
	}
	if providers.ClassifyTransport(nil) != nil {
		t.Fatal("nil must classify as nil")
	}
}

func TestClassifyStatus_RetryableAndPermanent(t *testing.T) {
	for _, status := range []int{200, 201, 299} {
		if err := providers.ClassifyStatus("ollama", status, []byte("{}")); err != nil {
			t.Fatalf("status %d = %v, want nil", status, err)
		}
	}
	var tr agentrt.TransientError
	var se providers.StatusError
	for _, status := range []int{408, 429, 500, 503, 529} {
		err := providers.ClassifyStatus("anthropic", status, []byte("  overloaded  "))
		if !errors.As(err, &tr) || !errors.As(err, &se) || se.Status != status {
			t.Fatalf("status %d = %v, want a transient status error", status, err)
		}
		if want := fmt.Sprintf("anthropic: %d: overloaded", status); !strings.Contains(err.Error(), want) {
			t.Fatalf("status %d message = %q, want it to contain %q", status, err.Error(), want)
		}
	}
	for _, status := range []int{400, 401, 404, 422} {
		err := providers.ClassifyStatus("anthropic", status, []byte("bad request"))
		if errors.As(err, &tr) {
			t.Fatalf("status %d must be permanent, got %v", status, err)
		}
		if !errors.As(err, &se) || se.Status != status || se.Body != "bad request" {
			t.Fatalf("status %d = %#v", status, err)
		}
	}
}

func TestClassifyStatus_BodyExcerptIsBounded(t *testing.T) {
	var se providers.StatusError
	err := providers.ClassifyStatus("ollama", 400, []byte(strings.Repeat("x", 5000)))
	if !errors.As(err, &se) {
		t.Fatalf("err = %v", err)
	}
	if len(se.Body) != 2048+len("…") {
		t.Fatalf("excerpt is %d bytes, want the 2048-byte excerpt plus an ellipsis", len(se.Body))
	}
}

func TestClassifyAPIError_StatusOnlyAdapter(t *testing.T) {
	var tr agentrt.TransientError
	api := errors.New("429 rate_limit_error")
	if err := providers.ClassifyAPIError(429, api); !errors.As(err, &tr) {
		t.Fatalf("429 = %v, want transient", err)
	}
	if err := providers.ClassifyAPIError(400, api); errors.As(err, &tr) || !errors.Is(err, api) {
		t.Fatalf("400 = %v, want the error unchanged", err)
	}
	// No HTTP response: the error is a transport failure.
	if err := providers.ClassifyAPIError(0, timeoutError{}); errors.As(err, &tr) {
		t.Fatalf("timeout without a response = %v, want it bare", err)
	}
	if err := providers.ClassifyAPIError(0, errors.New("EOF")); !errors.As(err, &tr) {
		t.Fatalf("connection failure without a response = %v, want transient", err)
	}
	if providers.ClassifyAPIError(500, nil) != nil {
		t.Fatal("no error must classify as nil whatever the status")
	}
}

func TestReadBody_StopsAtTheLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < (providers.MaxBodyBytes/1024)+64; i++ {
			w.Write([]byte(strings.Repeat("y", 1024)))
		}
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := providers.ReadBody(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != providers.MaxBodyBytes {
		t.Fatalf("read %d bytes, want the %d-byte limit", len(body), providers.MaxBodyBytes)
	}
}

func TestSynthesizeToolUseID_UniquePerIndex(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		id := providers.SynthesizeToolUseID(i)
		if !strings.HasPrefix(id, "call_") || !strings.HasSuffix(id, fmt.Sprintf("_%d", i)) {
			t.Fatalf("id = %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
