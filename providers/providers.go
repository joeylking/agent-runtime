// Package providers holds the dependency-free helpers every provider
// adapter shares: transport and HTTP error classification, a bounded body
// read, an HTTP client with a size limit, a backstop timeout, and no
// redirects, the endpoint and usage checks, tool-use id synthesis, and price
// table helpers. It imports nothing outside the
// standard library and the runtime, so a provider SDK lives in its own
// module and the core stays untainted.
//
// The classification contract matches what the runtime's accounting caller
// expects. A timeout is returned bare, because the request may have been
// received and charged and the caller must record it as ambiguous, and so is
// the caller's own cancellation. Every other failure is either a
// TransientError, which the caller retries, or a permanent error, which ends
// the run. A failure that no retry can cure is permanent even when it
// happens in transport: a host that does not resolve, a certificate that
// does not verify, a URL that cannot be sent.
package providers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
)

// MaxBodyBytes bounds how much of a provider response is read.
const MaxBodyBytes = 16 << 20

// DefaultTimeout bounds one request made through a client from Client when
// the consumer supplies none. It is a backstop, not the deadline: the
// caller's context is what bounds a request, and this only stops a request
// whose context has no deadline from hanging forever. It is longer than any
// non-streaming generation a provider will serve.
const DefaultTimeout = 15 * time.Minute

// maxExcerpt bounds how much of a body an error message repeats.
const maxExcerpt = 2048

// maxDrain bounds how much of an unread body is discarded so that its
// connection can be reused; a longer body is closed instead.
const maxDrain = 64 << 10

// ErrBodyTooLarge is wrapped by ReadBody and by a body read through Client
// when a response exceeds MaxBodyBytes. It is permanent: the same request
// would produce the same response.
var ErrBodyTooLarge = errors.New("response body too large")

// ReadBody reads r to its end, refusing a body longer than MaxBodyBytes
// with an error that wraps ErrBodyTooLarge, so a provider that answers with
// an unbounded stream cannot exhaust the process and an oversized reply is
// never mistaken for a malformed one.
func ReadBody(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return b, err
	}
	if len(b) > MaxBodyBytes {
		return nil, tooLarge()
	}
	return b, nil
}

func tooLarge() error {
	return fmt.Errorf("providers: %w: over the %d-byte limit", ErrBodyTooLarge, MaxBodyBytes)
}

// DrainClose discards what is left of a response body, up to a bound, and
// closes it, so the connection goes back to the pool. Adapters defer it in
// place of Close.
func DrainClose(body io.ReadCloser) {
	io.CopyN(io.Discard, body, maxDrain)
	body.Close()
}

// Client returns the client an adapter uses: a copy of c, or a new client
// with DefaultTimeout when c is nil, whose transport refuses any response
// body over MaxBodyBytes. The caller's client is never modified, and the
// shared http.DefaultClient is never used.
//
// Redirects are not followed. A credential rides in a header, and net/http
// forwards every header but Authorization to another host, and Authorization
// too to the same host name on another port or from https to http, so a
// followed redirect can hand the key and the whole prompt to whoever the
// endpoint names. The 3xx is returned as the response instead, and
// ClassifyResponse makes it a permanent StatusError. A caller whose client
// has a CheckRedirect of its own keeps it, and where its redirects send the
// key is then the caller's responsibility.
func Client(c *http.Client) *http.Client {
	var out http.Client
	if c != nil {
		out = *c
	} else {
		out.Timeout = DefaultTimeout
	}
	if out.CheckRedirect == nil {
		out.CheckRedirect = RefuseRedirects
	}
	out.Transport = LimitResponses(out.Transport)
	return &out
}

// RefuseRedirects is an http.Client CheckRedirect that follows no redirect,
// so the 3xx itself is what the client returns.
func RefuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// ErrNotLoopback is wrapped by a connection that LoopbackOnly refused. It
// is permanent: the name resolves where it resolves.
var ErrNotLoopback = errors.New("not a loopback address")

// Loopback reports whether host, as url.URL.Hostname returns it, names this
// machine: exactly "localhost", or a literal loopback IP address. Nothing
// else is: not "LOCALHOST" or "localhost.", not a name under .localhost,
// and not a numeric form such as 127.1 or 2130706433 that only some
// resolvers read as an address.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Zone() == "" && ip.Unmap().IsLoopback()
}

// CheckEndpoint parses an adapter's base URL and refuses one that is not an
// http or https URL with a host, one that carries user information, and one
// that would send a key (key is true) over plain http to a host Loopback
// does not accept.
func CheckEndpoint(provider, base string, key bool) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s: base URL %q is not an http or https URL", provider, base)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%s: base URL for %s carries user information", provider, u.Host)
	}
	if key && u.Scheme == "http" && !Loopback(u.Hostname()) {
		return nil, fmt.Errorf("%s: refusing to send an API key over plain http to %s", provider, u.Host)
	}
	return u, nil
}

// LoopbackOnly returns a copy of c, as Client returned it or any other,
// whose connections are refused, with an error wrapping ErrNotLoopback,
// unless the address actually dialled is a loopback address. An adapter
// uses it when a key travels over plain http: "localhost" is the resolver's
// answer, not a promise, and a proxy would carry the key off the machine.
// It works on an *http.Transport, cloned and never modified, or on the
// default transport when c has none. A client whose transport is some other
// RoundTripper is returned unchanged, and where it sends the key is its
// owner's responsibility.
func LoopbackOnly(c *http.Client) *http.Client {
	out := *c
	rt, limited := out.Transport, false
	if l, ok := rt.(limitTransport); ok {
		rt, limited = l.next, true
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	t, ok := rt.(*http.Transport)
	if !ok {
		return &out
	}
	t = t.Clone()
	dial := t.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if !remoteLoopback(conn.RemoteAddr()) {
			conn.Close()
			return nil, fmt.Errorf("providers: %s resolved to %s: %w", addr, conn.RemoteAddr(), ErrNotLoopback)
		}
		return conn, nil
	}
	out.Transport = t
	if limited {
		out.Transport = limitTransport{t}
	}
	return &out
}

func remoteLoopback(a net.Addr) bool {
	switch x := a.(type) {
	case *net.TCPAddr:
		return x.IP.IsLoopback()
	case *net.UnixAddr:
		return true
	}
	ap, err := netip.ParseAddrPort(a.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}

// LimitResponses wraps rt, or http.DefaultTransport when rt is nil, so that
// reading more than MaxBodyBytes of any response body fails with an error
// wrapping ErrBodyTooLarge. It is for a client whose body is read by code
// the adapter does not own, such as a provider SDK.
func LimitResponses(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	if l, ok := rt.(limitTransport); ok {
		return l
	}
	return limitTransport{rt}
}

type limitTransport struct{ next http.RoundTripper }

func (l limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.next.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &limitBody{rc: resp.Body, left: MaxBodyBytes}
	return resp, nil
}

type limitBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitBody) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, tooLarge()
	}
	// Read one byte past the limit so that a body of exactly the limit
	// still ends cleanly and a longer one is detected.
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n - int(-b.left), tooLarge()
	}
	return n, err
}

func (b *limitBody) Close() error { return b.rc.Close() }

// Transient reports whether an HTTP status is worth retrying: 408 request
// timeout, 409 conflict, 425 too early, 429 rate limited, and every 5xx,
// which includes Anthropic's 529 overloaded.
func Transient(status int) bool {
	return status == 408 || status == 409 || status == 425 || status == 429 || status >= 500
}

// StatusError is a provider's non-2xx response. It carries the status so a
// caller can act on it without matching strings, and never the request, so
// no credential rides along with it.
type StatusError struct {
	Provider string
	Status   int
	Body     string
	// RetryAfter is the delay the provider asked for in a Retry-After
	// header, or zero. A transient StatusError carries the same value on the
	// agentrt.TransientError that wraps it, so a caller can read it from
	// either with errors.As.
	RetryAfter time.Duration
}

func (e StatusError) Error() string {
	return fmt.Sprintf("%s: %d: %s", e.Provider, e.Status, e.Body)
}

// ClassifyTransport maps a failed HTTP round trip onto the runtime's
// classes. A timeout is returned bare so the accounting caller charges it
// as ambiguous, and so is a body cut off mid-read, because the provider
// received the request and may have billed it. The caller's cancellation
// is returned bare too: it is not the provider's failure. A host that does
// not resolve, a certificate that does not verify, and a URL that cannot
// be sent are permanent. Anything else, such as a refused or reset
// connection, is transient.
func ClassifyTransport(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return err
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if permanentTransport(err) {
		return err
	}
	return agentrt.TransientError{Err: err}
}

func permanentTransport(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return true
	}
	var (
		verify    *tls.CertificateVerificationError
		authority x509.UnknownAuthorityError
		host      x509.HostnameError
		invalid   x509.CertificateInvalidError
		roots     x509.SystemRootsError
		escape    url.EscapeError
		badHost   url.InvalidHostError
	)
	if errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &host) ||
		errors.As(err, &invalid) || errors.As(err, &roots) || errors.As(err, &escape) || errors.As(err, &badHost) {
		return true
	}
	if errors.Is(err, ErrBodyTooLarge) || errors.Is(err, ErrNotLoopback) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Op == "parse" {
		return true
	}
	// net/http reports these as plain strings.
	msg := err.Error()
	return strings.Contains(msg, "unsupported protocol scheme") || strings.Contains(msg, "no Host in request URL")
}

// ClassifyStatus converts a provider response into an error, or nil for a
// 2xx. A retryable status yields a TransientError wrapping a StatusError;
// any other non-2xx yields the StatusError itself, which is permanent. A
// 3xx is permanent too, since Client follows no redirect, and its Body says
// so in place of the response body.
func ClassifyStatus(provider string, status int, body []byte) error {
	return classify(provider, status, body, 0)
}

// ClassifyResponse is ClassifyStatus for a response whose headers are at
// hand, so a Retry-After header is carried on the StatusError and, when the
// status is transient, on the agentrt.TransientError wrapping it too. A 3xx
// names the host its Location header points at, and nothing else of it: a
// redirect's path or query may carry what the endpoint was given.
func ClassifyResponse(provider string, resp *http.Response, body []byte) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return StatusError{Provider: provider, Status: resp.StatusCode, Body: redirected(resp.Header.Get("Location"))}
	}
	return classify(provider, resp.StatusCode, body, RetryAfter(resp.Header))
}

// redirected is the Body of a StatusError for a 3xx.
func redirected(location string) string {
	const text = "redirects are not followed"
	if location == "" {
		return text
	}
	u, err := url.Parse(location)
	switch {
	case err != nil:
		return text + "; the Location header does not parse"
	case u.Host == "":
		return text + "; the response pointed elsewhere on the same host"
	}
	return fmt.Sprintf("%s; the response pointed to host %q", text, u.Host)
}

func classify(provider string, status int, body []byte, after time.Duration) error {
	if status >= 200 && status < 300 {
		return nil
	}
	if status >= 300 && status < 400 {
		return StatusError{Provider: provider, Status: status, Body: redirected("")}
	}
	err := StatusError{Provider: provider, Status: status, Body: excerpt(body), RetryAfter: after}
	if Transient(status) {
		return agentrt.TransientError{Err: err, RetryAfter: after}
	}
	return err
}

// maxRetryAfter bounds what RetryAfter reports. A provider's own retry
// clock cannot be trusted past this, so a longer or negative value is
// treated the same as no header at all rather than stalling a caller for an
// absurd delay.
const maxRetryAfter = time.Hour

// RetryAfter parses a Retry-After header, in seconds or as an HTTP date,
// and returns zero when there is none, it cannot be read, or it names a
// negative or more-than-an-hour delay.
func RetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		if s < 0 {
			return 0
		}
		d := time.Duration(s) * time.Second
		if d > maxRetryAfter {
			return 0
		}
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 && d <= maxRetryAfter {
			return d
		}
	}
	return 0
}

// ClassifyAPIError classifies an SDK error that yields only a status code
// and the error itself. A status of zero means the SDK reported no HTTP
// response, so the error is a transport failure.
func ClassifyAPIError(status int, err error) error {
	if err == nil {
		return nil
	}
	if status == 0 {
		return ClassifyTransport(err)
	}
	if Transient(status) {
		return agentrt.TransientError{Err: err}
	}
	return err
}

func excerpt(body []byte) string {
	return Truncate(strings.TrimSpace(string(body)), maxExcerpt)
}

// Truncate cuts s to at most n bytes on a rune boundary and marks the cut
// with an ellipsis, for text from a provider or a model that an error
// message repeats.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// SynthesizeToolUseID names the ith tool use of a response for providers
// that return no id. Tool-use ids only have to be unique within a request,
// and both consumers' adapters use this form.
func SynthesizeToolUseID(i int) string {
	return fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
}

// ErrUnusableUsage is wrapped by UsageCount and CheckUsage, and is
// agentrt.ErrUnusableUsage itself. A provider that reports a negative
// count, one too large to hold, or more cached input than input has not
// said what the call cost, and a count taken on trust would let it drive a
// run's cost below zero and past every limit. An adapter returns such a
// reply as an agentrt.ServedError with zero usage wrapping this error, and
// the accounting caller charges its own conservative estimate instead.
var ErrUnusableUsage = agentrt.ErrUnusableUsage

// UsageCount reads one token count from a provider's reply: absent or null
// is zero, and anything but a non-negative JSON integer that fits an int is
// an error naming field and wrapping ErrUnusableUsage.
func UsageCount(field string, raw json.RawMessage) (int, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, strconv.IntSize)
	if err != nil || n < 0 || strings.HasPrefix(s, "+") {
		return 0, fmt.Errorf("%w: %s is %s, not a count", ErrUnusableUsage, field, Truncate(s, 64))
	}
	return int(n), nil
}

// CheckUsage refuses usage with a negative count or with more cached input
// than input, with an error wrapping ErrUnusableUsage.
func CheckUsage(u agentrt.Usage) error {
	switch {
	case u.InputTokens < 0:
		return fmt.Errorf("%w: input tokens %d", ErrUnusableUsage, u.InputTokens)
	case u.OutputTokens < 0:
		return fmt.Errorf("%w: output tokens %d", ErrUnusableUsage, u.OutputTokens)
	case u.CachedInputTokens < 0:
		return fmt.Errorf("%w: cached input tokens %d", ErrUnusableUsage, u.CachedInputTokens)
	case u.CachedInputTokens > u.InputTokens:
		return fmt.Errorf("%w: cached input tokens %d exceed input tokens %d", ErrUnusableUsage, u.CachedInputTokens, u.InputTokens)
	}
	return nil
}
