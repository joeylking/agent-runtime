package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MaxResponseBodyBytes caps one HTTP response body from a streamable HTTP
// server, the same bound the SDK puts on one stdio frame. A body that goes
// past it fails the read with an error rather than being cut short, so a
// hostile server cannot exhaust memory and cannot have a truncated message
// taken for a whole one.
const MaxResponseBodyBytes = 16 << 20

// StderrTailBytes is how much of a stdio server's stderr is kept when
// Server.Stderr is nil. The tail is appended to a failed connect or tool
// listing, which is when an operator needs to know why the server died.
const StderrTailBytes = 4 << 10

// baseEnv names the variables a stdio server receives from this process
// without being asked for: what a program needs to find its interpreter and
// a place to write, and nothing that carries a credential.
func baseEnv(goos string) []string {
	if goos == "windows" {
		return []string{"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "ComSpec", "TEMP", "TMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOME"}
	}
	return []string{"PATH", "HOME", "TMPDIR"}
}

// childEnv builds a stdio server's environment: the base variables, then
// the ones named in inherit, taken from lookup when set, then extra as
// written. A later entry replaces an earlier one of the same name.
func childEnv(goos string, inherit, extra []string, lookup func(string) (string, bool)) []string {
	env := []string{}
	index := map[string]int{}
	set := func(name, value string) {
		key := name
		if goos == "windows" {
			key = strings.ToUpper(name)
		}
		entry := name + "=" + value
		if i, ok := index[key]; ok {
			env[i] = entry
			return
		}
		index[key] = len(env)
		env = append(env, entry)
	}
	for _, names := range [][]string{baseEnv(goos), inherit} {
		for _, name := range names {
			if value, ok := lookup(name); ok {
				set(name, value)
			}
		}
	}
	for _, kv := range extra {
		name, value, _ := strings.Cut(kv, "=")
		set(name, value)
	}
	return env
}

func validateEnv(server string, inherit, extra []string) error {
	for _, name := range inherit {
		if name == "" || strings.Contains(name, "=") {
			return fmt.Errorf("mcp: server %q: InheritEnv entry %q is not a variable name", server, name)
		}
	}
	for _, kv := range extra {
		if name, _, ok := strings.Cut(kv, "="); !ok || name == "" {
			return fmt.Errorf("mcp: server %q: Env entry %q is not NAME=value", server, kv)
		}
	}
	return nil
}

// stderrTail keeps the last StderrTailBytes a stdio server wrote to stderr.
// It reads from a pipe it owns, so a caller can wait for the server's end
// of it to close before reading what it said.
type stderrTail struct {
	mu   sync.Mutex
	buf  []byte
	done chan struct{}
}

// newStderrTail returns the tail and the file the server writes to. The
// caller closes the file once the server has started, or failed to.
func newStderrTail() (*stderrTail, *os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	t := &stderrTail{done: make(chan struct{})}
	go func() {
		defer close(t.done)
		defer r.Close()
		_, _ = io.Copy(t, r)
	}()
	return t, w, nil
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - StderrTailBytes; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// annotate adds what the server said on stderr to err, waiting for the
// server's end of the pipe to close so that a server that printed why it
// died and exited is heard in full.
func (t *stderrTail) annotate(ctx context.Context, err error) error {
	if t == nil {
		return err
	}
	select {
	case <-t.done:
	case <-ctx.Done():
	}
	if tail := t.String(); tail != "" {
		return fmt.Errorf("%w; stderr: %q", err, tail)
	}
	return err
}

// listRecorder keeps the raw result of every tools/list response on one
// connection, matched to its request by JSON-RPC id. The SDK hands the
// client a decoded schema whose numbers are float64s; the pin needs the
// bytes the server sent.
type listRecorder struct {
	mu      sync.Mutex
	pending map[any]bool
	results []json.RawMessage
}

func (r *listRecorder) sent(msg jsonrpc.Message) {
	req, ok := msg.(*jsonrpc.Request)
	if !ok || req.Method != "tools/list" || !req.ID.IsValid() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[any]bool{}
	}
	r.pending[req.ID.Raw()] = true
}

func (r *listRecorder) received(msg jsonrpc.Message) {
	resp, ok := msg.(*jsonrpc.Response)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.pending[resp.ID.Raw()] {
		return
	}
	delete(r.pending, resp.ID.Raw())
	if resp.Error == nil {
		r.results = append(r.results, bytes.Clone(resp.Result))
	}
}

func (r *listRecorder) take() []json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.results
	r.results = nil
	return out
}

// recordingTransport shows a connection's messages to a listRecorder. It is
// only used where the SDK's connection has no unexported client hooks to
// lose by being wrapped: stdio and in-memory. Streamable HTTP is recorded at
// the HTTP layer instead.
type recordingTransport struct {
	inner sdk.Transport
	rec   *listRecorder
}

func (t *recordingTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingConn{Connection: conn, rec: t.rec}, nil
}

type recordingConn struct {
	sdk.Connection
	rec *listRecorder
}

func (c *recordingConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err == nil {
		c.rec.received(msg)
	}
	return msg, err
}

func (c *recordingConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	c.rec.sent(msg)
	return c.Connection.Write(ctx, msg)
}

// httpClient is the client a streamable HTTP connection uses: a copy of the
// operator's, or a fresh one, never http.DefaultClient, with its transport
// wrapped so every response body is capped and tools/list results are
// recorded. The operator's client is not modified.
func httpClient(base *http.Client, rec *listRecorder) *http.Client {
	var c http.Client
	if base != nil {
		c = *base
	}
	inner := c.Transport
	if inner == nil {
		inner = http.DefaultTransport.(*http.Transport).Clone()
	}
	c.Transport = &limitedTransport{inner: inner, limit: MaxResponseBodyBytes, rec: rec}
	return &c
}

// limitedTransport caps each response body at limit bytes and, given a
// recorder, shows it the JSON-RPC messages that pass.
type limitedTransport struct {
	inner http.RoundTripper
	limit int64
	rec   *listRecorder
}

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.rec != nil && req.Method == http.MethodPost && req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			raw, _ := io.ReadAll(io.LimitReader(body, MaxResponseBodyBytes))
			body.Close()
			if msg, err := jsonrpc.DecodeMessage(raw); err == nil {
				t.rec.sent(msg)
			}
		}
	}
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.limit {
		resp.Body.Close()
		return nil, fmt.Errorf("mcp: response body of %d bytes exceeds %d", resp.ContentLength, t.limit)
	}
	var body io.ReadCloser = &limitedBody{inner: resp.Body, left: t.limit, limit: t.limit}
	if t.rec != nil {
		switch mediaType(resp.Header.Get("Content-Type")) {
		case "application/json":
			body = &messageTap{inner: body, rec: t.rec}
		case "text/event-stream":
			body = &messageTap{inner: body, rec: t.rec, sse: true}
		}
	}
	resp.Body = body
	return resp, nil
}

func mediaType(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// limitedBody fails the read that would take a body past its limit.
type limitedBody struct {
	inner       io.ReadCloser
	left, limit int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var probe [1]byte
		n, err := b.inner.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("mcp: response body exceeds %d bytes", b.limit)
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.inner.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.inner.Close() }

// messageTap shows a recorder each JSON-RPC message in a response body as
// the SDK reads it: the whole body for application/json, each event's data
// for an SSE stream. A message is recorded before the read that completes it
// returns, so it is on record before the SDK can act on it.
type messageTap struct {
	inner  io.ReadCloser
	rec    *listRecorder
	sse    bool
	buf    []byte // the whole body, or the SSE line in progress
	data   []byte // the SSE event's data so far
	isData bool
}

func (m *messageTap) Read(p []byte) (int, error) {
	n, err := m.inner.Read(p)
	m.buf = append(m.buf, p[:n]...)
	if m.sse {
		m.lines(err == io.EOF)
	} else if err == io.EOF {
		m.message(m.buf)
		m.buf = nil
	}
	return n, err
}

func (m *messageTap) lines(eof bool) {
	for {
		i := bytes.IndexByte(m.buf, '\n')
		if i < 0 {
			break
		}
		m.line(bytes.TrimRight(m.buf[:i], "\r"))
		m.buf = m.buf[i+1:]
	}
	if eof {
		if len(m.buf) > 0 {
			m.line(bytes.TrimRight(m.buf, "\r"))
			m.buf = nil
		}
		m.line(nil)
	}
}

func (m *messageTap) line(line []byte) {
	if len(line) == 0 {
		if m.isData {
			m.message(m.data)
		}
		m.data, m.isData = nil, false
		return
	}
	value, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	value = bytes.TrimPrefix(value, []byte(" "))
	if m.isData {
		m.data = append(m.data, '\n')
	}
	m.data = append(m.data, value...)
	m.isData = true
}

func (m *messageTap) message(raw []byte) {
	if msg, err := jsonrpc.DecodeMessage(raw); err == nil {
		m.rec.received(msg)
	}
}

func (m *messageTap) Close() error { return m.inner.Close() }
