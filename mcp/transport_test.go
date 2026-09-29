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
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// childMode makes the test binary a stdio MCP server when a test starts it
// as one, so the subprocess tests need nothing but the binary itself.
const childMode = "AGENTRT_MCP_TEST_SERVER"

func TestMain(m *testing.M) {
	switch os.Getenv(childMode) {
	case "":
		os.Exit(m.Run())
	case "env":
		childServer(false)
	case "die-on-list":
		childServer(true)
	}
	os.Exit(0)
}

// childServer offers one tool that answers with its own environment. With
// dieOnList it says why on stderr and exits when asked for its tools.
func childServer(dieOnList bool) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "child", Version: "0"}, nil)
	srv.AddTool(&sdk.Tool{Name: "env", Description: "Print the environment.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.Join(os.Environ(), "\n")}}}, nil
		})
	if dieOnList {
		srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
			return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
				if method == "tools/list" {
					fmt.Fprintln(os.Stderr, "listing refused: no licence")
					os.Exit(3)
				}
				return next(ctx, method, req)
			}
		})
	}
	_ = srv.Run(context.Background(), &sdk.StdioTransport{})
}

func TestChildEnv_IsMinimalAndExplicit(t *testing.T) {
	host := map[string]string{
		"PATH": "/bin", "HOME": "/home/op", "TMPDIR": "/tmp/op",
		"AWS_SECRET_ACCESS_KEY": "secret", "GITHUB_TOKEN": "secret", "LANG": "C",
	}
	lookup := func(name string) (string, bool) { v, ok := host[name]; return v, ok }

	got := childEnv("darwin", nil, nil, lookup)
	if want := []string{"PATH=/bin", "HOME=/home/op", "TMPDIR=/tmp/op"}; !slices.Equal(got, want) {
		t.Errorf("default = %v, want %v", got, want)
	}

	got = childEnv("linux", []string{"LANG", "UNSET"}, []string{"API_KEY=k", "PATH=/opt/bin", "EMPTY="}, lookup)
	if want := []string{"PATH=/opt/bin", "HOME=/home/op", "TMPDIR=/tmp/op", "LANG=C", "API_KEY=k", "EMPTY="}; !slices.Equal(got, want) {
		t.Errorf("with additions = %v, want %v", got, want)
	}

	winHost := map[string]string{"PATH": `C:\bin`, "SystemRoot": `C:\Windows`, "USERPROFILE": `C:\Users\op`, "GITHUB_TOKEN": "secret"}
	got = childEnv("windows", nil, []string{"Path=D:\\bin"}, func(n string) (string, bool) { v, ok := winHost[n]; return v, ok })
	if want := []string{`Path=D:\bin`, `SystemRoot=C:\Windows`, `USERPROFILE=C:\Users\op`}; !slices.Equal(got, want) {
		t.Errorf("windows = %v, want %v", got, want)
	}

	if got := childEnv("linux", nil, nil, func(string) (string, bool) { return "", false }); got == nil {
		t.Error("an empty environment must be empty, not nil, or exec inherits everything")
	}
}

func TestServer_RejectsMalformedEnv(t *testing.T) {
	for _, s := range []Server{
		{Name: "fs", Command: []string{"x"}, Env: []string{"NOEQUALS"}},
		{Name: "fs", Command: []string{"x"}, Env: []string{"=v"}},
		{Name: "fs", Command: []string{"x"}, InheritEnv: []string{"A=B"}},
		{Name: "fs", Command: []string{"x"}, InheritEnv: []string{""}},
	} {
		if err := s.validate(); err == nil {
			t.Errorf("%+v was accepted", s)
		}
	}
}

func skipWithoutUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a Unix shell")
	}
}

// TestStdio_ServerGetsOnlyTheEnvironmentItIsGiven runs the test binary as a
// real stdio server and asks it what it can see.
func TestStdio_ServerGetsOnlyTheEnvironmentItIsGiven(t *testing.T) {
	t.Setenv("AGENTRT_MCP_TEST_SECRET", "s3cret")
	t.Setenv("AGENTRT_MCP_TEST_PASSED", "yes")
	server := Server{
		Name:           "child",
		Command:        []string{os.Args[0]},
		Env:            []string{childMode + "=env"},
		InheritEnv:     []string{"AGENTRT_MCP_TEST_PASSED"},
		DefaultTimeout: 5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m, err := Pin(ctx, server)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	tools, rep, err := Load(ctx, server, m, Rules{"env": {SideEffect: agentrt.ReadOnly}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer rep.Connection.Close()
	res, err := tools[0].Call(ctx, agentrt.ToolCall{Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	env := strings.Split(text(t, res.Content), "\n")
	for _, want := range []string{"AGENTRT_MCP_TEST_PASSED=yes", childMode + "=env", "PATH=" + os.Getenv("PATH")} {
		if !slices.Contains(env, want) {
			t.Errorf("child env lacks %s: %v", want, env)
		}
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains([]string{"PATH", "HOME", "TMPDIR", "AGENTRT_MCP_TEST_PASSED", childMode}, name) {
			t.Errorf("child env has %s, which nobody passed", kv)
		}
	}
}

func TestStdio_StderrTailIsInAFailedConnect(t *testing.T) {
	skipWithoutUnix(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server := Server{Name: "sh", Command: []string{"sh", "-c", "echo oops: missing config >&2; exit 1"}}
	_, err := Pin(ctx, server)
	if err == nil || !strings.Contains(err.Error(), "connect") || !strings.Contains(err.Error(), "oops: missing config") {
		t.Errorf("err = %v, want the connect failure with the server's stderr", err)
	}

	var stderr bytes.Buffer
	server.Stderr = &stderr
	_, err = Pin(ctx, server)
	if err == nil || strings.Contains(err.Error(), "oops") {
		t.Errorf("err = %v, want no tail when the operator takes stderr", err)
	}
	if !strings.Contains(stderr.String(), "oops: missing config") {
		t.Errorf("stderr = %q, want it written to the operator's writer", stderr.String())
	}

	server = Server{Name: "sh", Command: []string{"sh", "-c", "head -c 20000 /dev/zero | tr '\\0' x >&2; echo; echo the end >&2; exit 1"}}
	_, err = Pin(ctx, server)
	if err == nil || !strings.Contains(err.Error(), "the end") || len(err.Error()) > StderrTailBytes+500 {
		t.Errorf("err is %d bytes, want the tail of the stderr only", len(err.Error()))
	}
}

func TestStdio_StderrTailIsInAFailedListing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Pin(ctx, Server{Name: "child", Command: []string{os.Args[0]}, Env: []string{childMode + "=die-on-list"}})
	if err == nil || !strings.Contains(err.Error(), "list tools") || !strings.Contains(err.Error(), "listing refused: no licence") {
		t.Errorf("err = %v, want the listing failure with the server's stderr", err)
	}
}

func TestLimitedTransport_FailsABodyPastTheLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := len(r.URL.Query().Get("n"))
		if r.URL.Query().Has("length") {
			w.Header().Set("Content-Length", fmt.Sprint(n))
		}
		// Written in two flushes so there is no Content-Length to check
		// up front unless the test sets one.
		w.Write(bytes.Repeat([]byte("x"), n/2))
		w.(http.Flusher).Flush()
		w.Write(bytes.Repeat([]byte("x"), n-n/2))
	}))
	defer srv.Close()
	client := &http.Client{Transport: &limitedTransport{inner: http.DefaultTransport, limit: 10}}
	get := func(n int, length bool) ([]byte, error) {
		url := srv.URL + "?n=" + strings.Repeat("x", n)
		if length {
			url += "&length"
		}
		resp, err := client.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	if body, err := get(10, false); err != nil || len(body) != 10 {
		t.Errorf("at the limit: %d bytes, err %v", len(body), err)
	}
	if body, err := get(11, false); err == nil || !strings.Contains(err.Error(), "exceeds 10 bytes") {
		t.Errorf("past the limit: %d bytes, err %v, want the read to fail", len(body), err)
	}
	if _, err := get(11, true); err == nil || !strings.Contains(err.Error(), "exceeds 10") {
		t.Errorf("past the limit by Content-Length: err %v", err)
	}
}

func TestHTTPClient_CopiesAndNeverUsesTheDefault(t *testing.T) {
	c := httpClient(nil, nil)
	if c == http.DefaultClient {
		t.Fatal("got http.DefaultClient")
	}
	lt, ok := c.Transport.(*limitedTransport)
	if !ok || lt.limit != MaxResponseBodyBytes || lt.inner == http.DefaultTransport {
		t.Errorf("transport = %#v, want a limiter over a fresh transport", c.Transport)
	}

	inner := &http.Transport{}
	mine := &http.Client{Transport: inner, Timeout: time.Minute}
	c = httpClient(mine, nil)
	if c == mine || mine.Transport != inner {
		t.Fatal("the operator's client was modified or reused")
	}
	if lt, ok := c.Transport.(*limitedTransport); !ok || lt.inner != inner || c.Timeout != time.Minute {
		t.Errorf("client = %#v, want the operator's settings under the limiter", c)
	}
}

// TestPin_OverStreamableHTTPReadsTheRawSchema pins a real streamable HTTP
// server, in both of its response modes, and checks the raw bytes reached the
// pin through the HTTP transport's recorder.
func TestPin_OverStreamableHTTPReadsTheRawSchema(t *testing.T) {
	for _, jsonResponse := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", jsonResponse), func(t *testing.T) {
			f := newFake(t)
			f.add("big", "d", `{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993}}}`, nil, textHandler("ok"))
			handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return f.srv }, &sdk.StreamableHTTPOptions{JSONResponse: jsonResponse})
			srv := httptest.NewServer(handler)
			defer srv.Close()
			server := Server{Name: "web", Endpoint: srv.URL, DefaultTimeout: 2 * time.Second}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			m, err := Pin(ctx, server)
			if err != nil {
				t.Fatalf("Pin: %v", err)
			}
			if got := string(m.Tools["big"].InputSchema); !strings.Contains(got, "9007199254740993") {
				t.Errorf("pinned %s, want the literal", got)
			}
			tools, rep, err := Load(ctx, server, m, Rules{"big": {SideEffect: agentrt.ReadOnly}})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			defer rep.Connection.Close()
			res, err := tools[0].Call(ctx, agentrt.ToolCall{Args: json.RawMessage(`{}`)})
			if err != nil || text(t, res.Content) != "ok" {
				t.Errorf("call = %s, %v", res.Content, err)
			}
		})
	}
}

func TestPin_OversizedHTTPResponseFails(t *testing.T) {
	f := newFake(t)
	f.add("huge", strings.Repeat("d", MaxResponseBodyBytes), readSchema, nil, textHandler("ok"))
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return f.srv }, &sdk.StreamableHTTPOptions{JSONResponse: true})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Pin(ctx, Server{Name: "web", Endpoint: srv.URL})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d bytes", MaxResponseBodyBytes)) {
		t.Fatalf("err = %v, want a listing past MaxResponseBodyBytes to fail on the limit", err)
	}
}

func TestMessageTap_RecordsJSONAndSSEResults(t *testing.T) {
	list := `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`
	result := `{"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"a","inputSchema":{"maximum":9007199254740993}}]}}`
	for name, body := range map[string]struct {
		sse  bool
		text string
	}{
		"json":             {false, result},
		"sse":              {true, "event: message\r\nid: 1\r\ndata: " + result + "\r\n\r\n"},
		"sse split":        {true, ": ping\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\ndata: \"result\":{\"tools\":[]}}\n\n"},
		"sse unterminated": {true, "data: " + result},
	} {
		rec := &listRecorder{}
		msg, err := jsonrpc.DecodeMessage([]byte(list))
		if err != nil {
			t.Fatal(err)
		}
		rec.sent(msg)
		tap := &messageTap{inner: io.NopCloser(strings.NewReader(body.text)), rec: rec, sse: body.sse}
		// One byte a read, so every line and event boundary falls between
		// reads.
		if _, err := io.ReadAll(iotest.OneByteReader(tap)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := rec.take()
		if len(got) != 1 || !json.Valid(got[0]) || !bytes.Contains(got[0], []byte(`"tools"`)) {
			t.Errorf("%s: recorded %q, want the one tools/list result", name, got)
		}
	}
}
