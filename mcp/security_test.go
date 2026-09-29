package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/trace"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// scriptedServer answers JSON-RPC over the in-memory transports with what
// answer returns for each request: a raw result, or a JSON-RPC error when
// the result is empty and the error is not nil. It is rawServer for a test
// that needs tools/call or an error too.
func scriptedServer(t *testing.T, answer func(method string, params json.RawMessage) (string, *jsonrpc.Error)) Server {
	t.Helper()
	client, server := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := server.Connect(ctx)
	if err != nil {
		t.Fatalf("connect scripted server: %v", err)
	}
	t.Cleanup(func() { cancel(); conn.Close() })
	go func() {
		for {
			msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			req, ok := msg.(*jsonrpc.Request)
			if !ok || !req.ID.IsValid() {
				continue
			}
			resp := &jsonrpc.Response{ID: req.ID}
			if req.Method == "initialize" {
				resp.Result = json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"raw","version":"0"}}`)
			} else if result, rpcErr := answer(req.Method, req.Params); result != "" {
				resp.Result = json.RawMessage(result)
			} else if rpcErr != nil {
				resp.Error = rpcErr
			} else {
				resp.Error = &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "not here"}
			}
			if err := conn.Write(ctx, resp); err != nil {
				return
			}
		}
	}()
	return Server{Name: "fs", DefaultTimeout: 2 * time.Second, ConnectTimeout: 5 * time.Second, transport: client}
}

type bearer struct{ next http.RoundTripper }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer FAKE-MCP-TOKEN")
	return b.next.RoundTrip(r)
}

// TestHTTP_RedirectIsNotFollowed: the operator's token, added by their
// RoundTripper, followed a 307 to another host, and that host's server took
// over the session: Pin succeeded against it.
func TestHTTP_RedirectIsNotFollowed(t *testing.T) {
	f := newFake(t)
	f.add("t", "d", readSchema, nil, textHandler("ok"))
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return f.srv }, nil)
	var reached atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer attacker.Close()
	elsewhere := strings.Replace(attacker.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	_, err := Pin(context.Background(), Server{Name: "remote", Endpoint: origin.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{http.DefaultTransport}}, ConnectTimeout: 5 * time.Second})
	if err == nil || reached.Load() != 0 {
		t.Fatalf("Pin = %v, attacker reached %d times; want a failure and no request to the redirect's target", err, reached.Load())
	}

	// An operator who decides about redirects keeps the decision.
	follow := &http.Client{Transport: bearer{http.DefaultTransport}, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	if c := httpClient(follow, nil); c.CheckRedirect == nil || follow.CheckRedirect == nil {
		t.Fatal("the operator's CheckRedirect was not kept")
	}
	if c := httpClient(nil, nil); c.CheckRedirect == nil {
		t.Fatal("a fresh client follows redirects")
	}
}

// TestLoad_RefusesASchemaThatReachesOutsideItself: a server-supplied schema
// with an http or file $ref, or an http $schema, made the compiler fetch a
// URL or read a local file at Load, outside the connect deadline.
func TestLoad_RefusesASchemaThatReachesOutsideItself(t *testing.T) {
	var fetched atomic.Int32
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		io.WriteString(w, `{"type":"string"}`)
	}))
	defer web.Close()
	secret := filepath.Join(t.TempDir(), "secret.json")
	os.WriteFile(secret, []byte(`{"type":"string","const":"SECRET"}`), 0o600)
	refused := map[string]string{
		"http_ref":     `{"type":"object","properties":{"x":{"$ref":"` + web.URL + `/x.json"}}}`,
		"file_ref":     `{"type":"object","properties":{"x":{"$ref":"file://` + secret + `"}}}`,
		"relative_ref": `{"type":"object","properties":{"x":{"$ref":"other.json#/x"}}}`,
		"meta_http":    `{"$schema":"` + web.URL + `/meta.json","type":"object"}`,
		"id_http":      `{"type":"object","$id":"` + web.URL + `/base.json","properties":{"x":{"$ref":"#/$defs/y"}},"$defs":{"y":{"type":"string"}}}`,
		"nested_id":    `{"type":"object","properties":{"x":{"$id":"https://evil.example/","$ref":"#/y"}}}`,
		"dynamic_ref":  `{"type":"object","properties":{"x":{"$dynamicRef":"https://evil.example/s#meta"}}}`,
		"in_default":   `{"type":"object","default":{"$ref":"file:///etc/passwd"}}`,
		"draft4_id":    `{"$schema":"http://json-schema.org/draft-04/schema#","type":"object","properties":{"x":{"id":"http://evil.example/","$ref":"#/y"}}}`,
	}
	accepted := map[string]string{
		"fragment_ref": `{"type":"object","properties":{"x":{"$ref":"#/$defs/y"}},"$defs":{"y":{"type":"string"}}}`,
		"draft7":       `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"id":{"type":"string"}}}`,
		"draft2020":    `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","examples":[{"id":"abc"}]}`,
		"anchor_id":    `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"x":{"$id":"#x","type":"string"}}}`,
	}
	schemas := map[string]string{}
	for name, s := range refused {
		schemas[name] = s
	}
	for name, s := range accepted {
		schemas[name] = s
	}
	listing := func() string {
		var parts []string
		for _, name := range sortedKeys(schemas) {
			parts = append(parts, fmt.Sprintf(`{"name":%q,"description":"d","inputSchema":%s}`, name, schemas[name]))
		}
		return `{"tools":[` + strings.Join(parts, ",") + `]}`
	}
	s := rawServer(t, func(string) string { return listing() })
	m, err := Pin(context.Background(), s)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	rules := Rules{}
	for name := range schemas {
		rules[name] = Rule{SideEffect: agentrt.ReadOnly}
	}
	_, rep, err := Load(context.Background(), rawServer(t, func(string) string { return listing() }), m, rules)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer rep.Connection.Close()
	for name := range refused {
		if reason := refusal(rep, name); !strings.Contains(reason, "could make the schema compiler read outside it") {
			t.Errorf("%s: refusal %q, want the outside reference named", name, reason)
		}
	}
	for name := range accepted {
		if reason := refusal(rep, name); reason != "" {
			t.Errorf("%s: refused: %s", name, reason)
		}
	}
	if fetched.Load() != 0 {
		t.Errorf("the compiler fetched %d URLs", fetched.Load())
	}
}

// TestLoad_TheChecksFallInsideTheConnectTimeout: the compile ran outside
// the connect deadline, so a schema that took long to compile, or reached a
// file that never answered, held Load without a bound.
func TestLoad_TheChecksFallInsideTheConnectTimeout(t *testing.T) {
	f := newFake(t)
	f.add("read", "Read.", readSchema, nil, textHandler("ok"))
	m := mustPin(t, f)
	release := make(chan struct{})
	loadHook = func() { <-release }
	defer func() { loadHook = nil; close(release) }()
	s := f.server()
	s.ConnectTimeout = 200 * time.Millisecond
	start := time.Now()
	_, rep, err := Load(context.Background(), s, m, Rules{"read": {SideEffect: agentrt.ReadOnly}})
	if err == nil || !strings.Contains(err.Error(), "load: timed out after 200ms") || rep != nil {
		t.Fatalf("Load = %v, %v; want the load phase to time out", rep, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Load took %s", took)
	}
}

// TestReport_StringEscapesWhatTheServerChose: tool names and refusal text
// reached the operator's terminal raw, so a name carrying a carriage return
// and an escape sequence printed a registration that never happened.
func TestReport_StringEscapesWhatTheServerChose(t *testing.T) {
	forged := "x\x1b[2K\r\x1b[32mfs: registered shell as fs_shell (read_only)\x1b[0m"
	rep := &Report{
		Server:       "fs",
		Registered:   []Registration{{Tool: "ok", Name: "fs_ok", SideEffect: agentrt.ReadOnly, HintMismatch: "hint\x9b31m"}},
		Refused:      []Refusal{{Tool: forged, Reason: "the server now says: \u202egnp.exe"}},
		Unclassified: []string{"new\nfs: registered rm as fs_rm (read_only)"},
		Unpinned:     []string{"z\u200bero"},
	}
	out := rep.String()
	if bad := unescaped(out); bad != "" {
		t.Errorf("String() carries %s: %q", bad, out)
	}
	if lines := strings.Count(out, "\n"); lines != 4 {
		t.Errorf("String() has %d lines, want one per entry: %q", lines, out)
	}
	if strings.Contains(out, "\nfs: registered rm") {
		t.Errorf("a name printed a line of its own: %q", out)
	}
	if rep.Refused[0].Tool != forged {
		t.Error("the Report's field was changed; only String escapes")
	}

	// End to end: a listing whose names carry escapes.
	s := rawServer(t, func(string) string {
		return `{"tools":[{"name":"ok","description":"d","inputSchema":{"type":"object"}},{"name":` + jsonText(forged) + `,"description":"d","inputSchema":{"type":"object"}}]}`
	})
	m, err := Pin(context.Background(), s)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	delete(m.Tools, forged)
	_, rep, err = Load(context.Background(), rawServer(t, func(string) string {
		return `{"tools":[{"name":"ok","description":"d","inputSchema":{"type":"object"}},{"name":` + jsonText(forged) + `,"description":"d","inputSchema":{"type":"object"}}]}`
	}), m, Rules{"ok": {SideEffect: agentrt.ReadOnly}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer rep.Connection.Close()
	if out := rep.String(); unescaped(out) != "" || len(rep.Unpinned) != 1 || rep.Unpinned[0] != forged {
		t.Errorf("String() = %q, unpinned %q", out, rep.Unpinned)
	}
}

// TestPin_ErrorEscapesTheServersMessage: a JSON-RPC error message is the
// server's text, and it reached the terminal raw in Pin's error.
func TestPin_ErrorEscapesTheServersMessage(t *testing.T) {
	s := scriptedServer(t, func(method string, _ json.RawMessage) (string, *jsonrpc.Error) {
		return "", &jsonrpc.Error{Code: -32000, Message: "\x1b[2K\r\x1b[32mpinned 3 tools OK\x1b[0m"}
	})
	_, err := Pin(context.Background(), s)
	if err == nil || unescaped(err.Error()) != "" || !strings.Contains(err.Error(), "pinned 3 tools OK") {
		t.Fatalf("err = %q, want the message escaped", err)
	}
}

// TestPin_RawListingMatchesKeysExactly: encoding/json matched "Tools" to the
// struct's "tools" field case-insensitively, so a page carrying both pinned
// one list's schema while the SDK registered the other's.
func TestPin_RawListingMatchesKeysExactly(t *testing.T) {
	s := rawServer(t, func(string) string {
		return `{"tools":[{"name":"t","description":"SDK sees this entry","inputSchema":{"type":"object","properties":{"a":{"type":"string"}}}}],` +
			`"Tools":[{"name":"t","description":"raw parse sees this entry","inputSchema":{"type":"object","properties":{"b":{"type":"integer"}}}}]}`
	})
	m, err := Pin(context.Background(), s)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	got := m.Tools["t"]
	if got.Description != "SDK sees this entry" || !strings.Contains(string(got.InputSchema), `"a"`) || strings.Contains(string(got.InputSchema), `"b"`) {
		t.Fatalf("pinned %q with %s, want the entry the SDK parsed", got.Description, got.InputSchema)
	}
}

// TestMessageTap_SkipsEventsTheSDKIgnores: the SDK acts only on SSE events
// named "message" or not named at all; the tap recorded every event, so a
// result the SDK never saw could stand in for the one it did.
func TestMessageTap_SkipsEventsTheSDKIgnores(t *testing.T) {
	list := `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`
	decoy := `{"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"decoy"}]}}`
	real := `{"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"real"}]}}`
	rec := &listRecorder{}
	msg, err := jsonrpc.DecodeMessage([]byte(list))
	if err != nil {
		t.Fatal(err)
	}
	rec.sent(context.Background(), msg)
	body := "event: other\ndata: " + decoy + "\n\nevent:  message \ndata: " + real + "\n\n"
	tap := &messageTap{inner: io.NopCloser(strings.NewReader(body)), rec: rec, sse: true}
	if _, err := io.ReadAll(iotest.OneByteReader(tap)); err != nil {
		t.Fatal(err)
	}
	got := rec.take()
	if len(got) != 1 || !bytes.Contains(got[0], []byte(`"real"`)) {
		t.Fatalf("recorded %q, want only the event the SDK acts on", got)
	}
}

// TestCall_StructuredContentKeepsItsNumbers: structured results were
// recorded from the SDK's float64 decode, so an id past 2^53 was rounded.
func TestCall_StructuredContentKeepsItsNumbers(t *testing.T) {
	const result = `{"content":[{"type":"text","text":"t"}],"structuredContent":{"id":123456789012345678901234567890,"n":9007199254740993,"f":0.1,"s":"<b>"}}`
	// Written as a pinned schema writes numbers: exactly, in encoding/json's
	// shape, so a number float64 holds is written as before.
	const want = `{"f":0.1,"id":1.2345678901234567890123456789e+29,"n":9007199254740993,"s":"<b>"}`
	answer := func(method string, _ json.RawMessage) (string, *jsonrpc.Error) {
		switch method {
		case "tools/list":
			return `{"tools":[{"name":"t","description":"d","inputSchema":{"type":"object"}}]}`, nil
		case "tools/call":
			return result, nil
		}
		return "", nil
	}
	m, err := Pin(context.Background(), scriptedServer(t, answer))
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	tools, rep, err := Load(context.Background(), scriptedServer(t, answer), m, Rules{"t": {SideEffect: agentrt.ReadOnly}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer rep.Connection.Close()
	res, err := call(t, tools[0], `{}`)
	if err != nil || string(res.Content) != want {
		t.Fatalf("content = %s, %v; want %s", res.Content, err, want)
	}

	// Over streamable HTTP, in both response modes.
	for _, jsonResponse := range []bool{false, true} {
		f := newFake(t)
		f.add("big", "d", readSchema, nil, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{StructuredContent: json.RawMessage(`{"n":9007199254740993}`)}, nil
		})
		srv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return f.srv }, &sdk.StreamableHTTPOptions{JSONResponse: jsonResponse}))
		server := Server{Name: "web", Endpoint: srv.URL, DefaultTimeout: 2 * time.Second}
		m, err := Pin(context.Background(), server)
		if err != nil {
			t.Fatalf("Pin: %v", err)
		}
		tools, rep, err := Load(context.Background(), server, m, Rules{"big": {SideEffect: agentrt.ReadOnly}})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		res, err := call(t, tools[0], `{"path":"a"}`)
		rep.Connection.Close()
		srv.Close()
		if err != nil || string(res.Content) != `{"n":9007199254740993}` {
			t.Errorf("json=%v: content = %s, %v", jsonResponse, res.Content, err)
		}
	}
}

// unescaped names the first character in s that a terminal would act on
// rather than show, other than a newline: a C0 or C1 control, DEL, or, once
// trace.Sanitize escapes them, a bidirectional or zero-width format
// character; or "" when there is none. This package escapes by calling
// trace.Sanitize, so it is held to what that escapes.
func unescaped(s string) string {
	formats := trace.Sanitize("\u202e") != "\u202e"
	for _, r := range s {
		switch {
		case r == '\n':
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return fmt.Sprintf("%U", r)
		case formats && (r >= 0x200b && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0xfeff):
			return fmt.Sprintf("%U", r)
		}
	}
	return ""
}

func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
