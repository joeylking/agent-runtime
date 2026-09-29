package providers_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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

// TestClient_RefusesRedirects: net/http forwards a custom key header such as
// x-api-key to any host a 307 names, and Authorization to the same host name
// on another port or over plain http. The sweep saw both keys arrive at the
// redirect's target with the whole prompt.
func TestClient_RefusesRedirects(t *testing.T) {
	var reached int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/steal?key=sk-FAKE", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	req, _ := http.NewRequest(http.MethodPost, origin.URL, strings.NewReader("the prompt"))
	req.Header.Set("X-Api-Key", "sk-FAKE-NOT-A-KEY")
	resp, err := providers.Client(nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || reached != 0 {
		t.Fatalf("status %d, target reached %d times; want the 307 itself and no second request", resp.StatusCode, reached)
	}
	err = providers.ClassifyResponse("anthropic", resp, []byte("body with sk-FAKE"))
	var se providers.StatusError
	var tr agentrt.TransientError
	if !errors.As(err, &se) || errors.As(err, &tr) || se.Status != 307 {
		t.Fatalf("err = %#v, want a permanent StatusError", err)
	}
	host := strings.TrimPrefix(target.URL, "http://")
	if !strings.Contains(err.Error(), "redirects are not followed") || !strings.Contains(err.Error(), host) ||
		strings.Contains(err.Error(), "steal") || strings.Contains(err.Error(), "sk-FAKE") {
		t.Fatalf("err = %q, want the refusal naming only the Location host", err)
	}
	if err := providers.ClassifyStatus("ollama", 302, []byte("x")); !errors.As(err, &se) || errors.As(err, &tr) {
		t.Fatalf("ClassifyStatus 302 = %#v, want permanent", err)
	}

	// A caller who decides about redirects keeps the decision.
	mine := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	resp, err = providers.Client(mine).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if reached != 1 {
		t.Fatalf("the caller's own CheckRedirect was not kept: target reached %d times", reached)
	}
}

func TestLoopback_OnlyLocalhostAndLiteralLoopbackAddresses(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "127.0.0.2": true, "::1": true, "::ffff:127.0.0.1": true,
		"LOCALHOST": false, "localhost.": false, "foo.localhost": false, "127.1": false, "2130706433": false,
		"0177.0.0.1": false, "0.0.0.0": false, "127.0.0.1.nip.io": false, "evil.example": false, "": false, "fe80::1%lo0": false,
	} {
		if got := providers.Loopback(host); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestCheckEndpoint_KeyOverPlainHTTPOnlyToLoopback(t *testing.T) {
	for _, tc := range []struct {
		base string
		key  bool
		ok   bool
	}{
		{"https://api.example.com/", true, true},
		{"http://127.0.0.1:8080/v1", true, true},
		{"http://localhost/v1", true, true},
		{"http://[::1]/v1", true, true},
		{"http://gateway.example.net/", false, true},
		{"http://gateway.example.net/", true, false},
		{"http://LOCALHOST/v1", true, false},
		{"http://127.0.0.1.nip.io/v1", true, false},
		{"HTTP://evil.example/v1", true, false},
		{"http://evil.example@127.0.0.1/v1", true, false},
		{"ftp://127.0.0.1/", false, false},
		{"https:///v1", false, false},
	} {
		_, err := providers.CheckEndpoint("p", tc.base, tc.key)
		if (err == nil) != tc.ok {
			t.Errorf("CheckEndpoint(%q, key=%v) = %v, want ok=%v", tc.base, tc.key, err, tc.ok)
		}
	}
}

// remoteConn is a connection that claims to have reached addr, which is how a
// resolver that maps "localhost" elsewhere looks from the dialler.
type remoteConn struct {
	net.Conn
	addr net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.addr }

func TestLoopbackOnly_RefusesAConnectionThatLeavesTheMachine(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()

	resp, err := providers.LoopbackOnly(providers.Client(nil)).Get(srv.URL)
	if err != nil {
		t.Fatalf("a loopback server was refused: %v", err)
	}
	resp.Body.Close()

	elsewhere := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			return nil, err
		}
		return remoteConn{c, &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 80}}, nil
	}}
	mine := &http.Client{Transport: elsewhere}
	_, err = providers.LoopbackOnly(providers.Client(mine)).Get("http://localhost/v1")
	if !errors.Is(err, providers.ErrNotLoopback) || hits != 1 {
		t.Fatalf("err = %v, hits %d; want ErrNotLoopback and no request sent", err, hits)
	}
	var tr agentrt.TransientError
	if c := providers.ClassifyTransport(err); errors.As(c, &tr) {
		t.Fatalf("a refused dial is retried: %v", c)
	}
	if elsewhere.DialContext == nil || mine.Transport != elsewhere {
		t.Fatal("the caller's transport was modified")
	}
}

func TestUsageCount_OnlyANonNegativeIntegerThatFits(t *testing.T) {
	for raw, want := range map[string]int{"": 0, "null": 0, "0": 0, "17": 17, " 9223372036854775807 ": 1<<63 - 1} {
		if got, err := providers.UsageCount("input_tokens", json.RawMessage(raw)); err != nil || got != want {
			t.Errorf("UsageCount(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"-1", "9223372036854775808", "1e3", "1.5", `"12"`, "true", "{}", "+1"} {
		_, err := providers.UsageCount("input_tokens", json.RawMessage(raw))
		if !errors.Is(err, providers.ErrUnusableUsage) || !strings.Contains(err.Error(), "input_tokens") {
			t.Errorf("UsageCount(%q) = %v, want an unusable-usage error naming the field", raw, err)
		}
	}
}

func TestCheckUsage_NegativeOrMoreCachedThanInputIsUnusable(t *testing.T) {
	if err := providers.CheckUsage(agentrt.Usage{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []agentrt.Usage{{InputTokens: -1}, {OutputTokens: -1}, {InputTokens: 1, CachedInputTokens: -1}, {InputTokens: 1, CachedInputTokens: 2}} {
		if err := providers.CheckUsage(u); !errors.Is(err, providers.ErrUnusableUsage) {
			t.Errorf("CheckUsage(%+v) = %v", u, err)
		}
	}
}
