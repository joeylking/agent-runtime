package providers_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "dial tcp: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestClassifyTransport_EveryClass(t *testing.T) {
	const (
		bare = iota
		transient
		permanent
	)
	dns := &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"timeout", timeoutError{}, bare},
		{"wrapped timeout", fmt.Errorf("post: %w", timeoutError{}), bare},
		{"deadline", &url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded}, bare},
		{"canceled", &url.Error{Op: "Post", URL: "http://x", Err: context.Canceled}, bare},
		{"dns not found", &url.Error{Op: "Post", URL: "http://x.invalid", Err: dns}, permanent},
		{"dns temporary", &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true}, transient},
		{"unknown authority", &url.Error{Op: "Post", URL: "https://x", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, permanent},
		{"hostname mismatch", x509.HostnameError{Host: "x"}, permanent},
		{"unsupported scheme", &url.Error{Op: "Post", URL: "ftp://x", Err: errors.New(`unsupported protocol scheme "ftp"`)}, permanent},
		{"no host", &url.Error{Op: "Post", URL: "http:///x", Err: errors.New("http: no Host in request URL")}, permanent},
		{"malformed url", &url.Error{Op: "parse", URL: "http://%zz", Err: url.EscapeError("%zz")}, permanent},
		{"body too large", fmt.Errorf("read: %w", providers.ErrBodyTooLarge), permanent},
		{"connection refused", &url.Error{Op: "Post", URL: "http://x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, transient},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, transient},
		{"mid-body EOF", io.ErrUnexpectedEOF, bare},
		{"wrapped mid-body EOF", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), bare},
	} {
		got := providers.ClassifyTransport(tc.err)
		var tr agentrt.TransientError
		isTransient := errors.As(got, &tr)
		switch {
		case !errors.Is(got, tc.err):
			t.Errorf("%s: %v does not wrap the original", tc.name, got)
		case tc.want == transient && !isTransient:
			t.Errorf("%s: %v, want transient", tc.name, got)
		case tc.want != transient && (isTransient || got != tc.err):
			t.Errorf("%s: %v, want the error returned as it is", tc.name, got)
		}
	}
	if providers.ClassifyTransport(nil) != nil {
		t.Fatal("nil must classify as nil")
	}
}

func TestClassifyTransport_RealRoundTrips(t *testing.T) {
	var tr agentrt.TransientError
	tlsSrv := httptest.NewUnstartedServer(http.NotFoundHandler())
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0)
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		url  string
	}{
		{"untrusted certificate", context.Background(), tlsSrv.URL},
		{"unsupported scheme", context.Background(), "ftp://x/api"},
		{"canceled", ctx, "http://127.0.0.1:1/"},
	} {
		req, err := http.NewRequestWithContext(tc.ctx, http.MethodPost, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = http.DefaultClient.Do(req)
		if err == nil {
			t.Fatalf("%s: no error", tc.name)
		}
		if got := providers.ClassifyTransport(err); errors.As(got, &tr) {
			t.Errorf("%s: %v, want it permanent or bare", tc.name, got)
		}
	}
	if err := providers.ClassifyTransport(ctx.Err()); !errors.Is(err, context.Canceled) || errors.As(err, &tr) {
		t.Errorf("cancellation = %v, want context.Canceled bare", err)
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
	for _, status := range []int{408, 409, 425, 429, 500, 503, 529} {
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

func TestReadBody_RefusesABodyOverTheLimit(t *testing.T) {
	body, err := providers.ReadBody(strings.NewReader(strings.Repeat("y", providers.MaxBodyBytes)))
	if err != nil || len(body) != providers.MaxBodyBytes {
		t.Fatalf("a body of exactly the limit: %d bytes, %v", len(body), err)
	}
	_, err = providers.ReadBody(strings.NewReader(strings.Repeat("y", providers.MaxBodyBytes+1)))
	if !errors.Is(err, providers.ErrBodyTooLarge) || !strings.Contains(err.Error(), fmt.Sprint(providers.MaxBodyBytes)) {
		t.Fatalf("one byte over = %v, want an error naming the limit", err)
	}
}

func TestClient_LimitsBodiesAndNeverSharesTheDefault(t *testing.T) {
	size := providers.MaxBodyBytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("y", size)))
	}))
	defer srv.Close()
	c := providers.Client(nil)
	if c == http.DefaultClient || c.Timeout != providers.DefaultTimeout {
		t.Fatalf("default client = %+v, want a fresh one with the backstop timeout", c)
	}
	mine := &http.Client{Timeout: time.Second}
	if got := providers.Client(mine); got == mine || got.Timeout != time.Second || mine.Transport != nil {
		t.Fatalf("the caller's client must be copied, not changed: %+v", mine)
	}
	for _, tc := range []struct {
		size    int
		tooMuch bool
	}{{providers.MaxBodyBytes, false}, {providers.MaxBodyBytes + 1, true}} {
		size = tc.size
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if tc.tooMuch != errors.Is(err, providers.ErrBodyTooLarge) || len(b) > providers.MaxBodyBytes {
			t.Fatalf("body of %d: read %d, err %v", tc.size, len(b), err)
		}
	}
}

func TestClassifyResponse_CarriesRetryAfter(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"7"}}}
	var se providers.StatusError
	err := providers.ClassifyResponse("openai", resp, []byte("slow down"))
	var tr agentrt.TransientError
	if !errors.As(err, &tr) || !errors.As(err, &se) || se.RetryAfter != 7*time.Second || tr.RetryAfter != 7*time.Second {
		t.Fatalf("err = %#v", err)
	}
	// ClassifyStatus has no header to read, so the delay is absent on both.
	err = providers.ClassifyStatus("openai", 429, []byte("slow down"))
	if !errors.As(err, &tr) || !errors.As(err, &se) || se.RetryAfter != 0 || tr.RetryAfter != 0 {
		t.Fatalf("ClassifyStatus err = %#v", err)
	}
	for _, v := range []string{"", "soon", "-3"} {
		if d := providers.RetryAfter(http.Header{"Retry-After": {v}}); d != 0 {
			t.Fatalf("Retry-After %q = %v, want zero", v, d)
		}
	}
	if d := providers.RetryAfter(http.Header{"Retry-After": {time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}}); d <= 0 || d > time.Hour {
		t.Fatalf("an HTTP date = %v", d)
	}
}

func TestRetryAfter_AbsurdOrNegativeIsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    string
	}{
		{"more than an hour in seconds", "3601"},
		{"negative seconds", "-1"},
		{"more than an hour as a date", time.Now().Add(time.Hour + time.Minute).UTC().Format(http.TimeFormat)},
		{"a date already past", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)},
	} {
		if d := providers.RetryAfter(http.Header{"Retry-After": {tc.v}}); d != 0 {
			t.Errorf("%s: Retry-After %q = %v, want zero", tc.name, tc.v, d)
		}
	}
	// Exactly one hour in seconds is still honoured.
	if d := providers.RetryAfter(http.Header{"Retry-After": {"3600"}}); d != time.Hour {
		t.Fatalf("3600 seconds = %v, want exactly an hour", d)
	}
}

func TestTruncate_CutsOnARuneBoundary(t *testing.T) {
	if got := providers.Truncate("héllo", 2); got != "h…" {
		t.Fatalf("got %q", got)
	}
	if got := providers.Truncate("short", 10); got != "short" {
		t.Fatalf("got %q", got)
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
