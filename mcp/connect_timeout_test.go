package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// silentServer answers only the methods named respond, and drops any other
// request on the floor: no response is ever written for it, so a caller
// waiting on it hangs until its own context ends. It is how a server that
// never answers initialize, or never answers tools/list, is simulated.
func silentServer(t *testing.T, respond map[string]string) Server {
	t.Helper()
	client, server := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := server.Connect(ctx)
	if err != nil {
		t.Fatalf("connect silent server: %v", err)
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
			var resp *jsonrpc.Response
			switch {
			case req.Method == "server/discover":
				// Answered with an error so the client falls back to the
				// legacy initialize handshake, which respond then controls
				// like every other method.
				resp = &jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "not here"}}
			default:
				result, ok := respond[req.Method]
				if !ok {
					continue // never answered
				}
				resp = &jsonrpc.Response{ID: req.ID, Result: json.RawMessage(result)}
			}
			if err := conn.Write(ctx, resp); err != nil {
				return
			}
		}
	}()
	return Server{Name: "fs", ConnectTimeout: 50 * time.Millisecond, transport: client}
}

const okInitialize = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"raw","version":"0"}}`

func TestServer_ConnectTimeoutBoundsAnInitializeThatNeverAnswers(t *testing.T) {
	s := silentServer(t, nil) // answers nothing at all
	start := time.Now()
	_, err := Pin(context.Background(), s)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), `"fs"`) || !strings.Contains(err.Error(), "connect") || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("err = %v, want the connect phase named with its bound", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Pin took %s, want it bounded by ConnectTimeout", elapsed)
	}
}

func TestServer_ConnectTimeoutBoundsAListingThatNeverAnswers(t *testing.T) {
	s := silentServer(t, map[string]string{"initialize": okInitialize})
	start := time.Now()
	_, err := Pin(context.Background(), s)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), `"fs"`) || !strings.Contains(err.Error(), "list tools") || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("err = %v, want the list tools phase named with its bound", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Pin took %s, want it bounded by ConnectTimeout", elapsed)
	}

	// Load fails the same way, and leaves no connection to close.
	m := &Manifest{Server: "fs", Tools: map[string]PinnedTool{"t0": {Name: "t0", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	s2 := silentServer(t, map[string]string{"initialize": okInitialize})
	_, rep, err := Load(context.Background(), s2, m, Rules{"t0": {SideEffect: agentrt.ReadOnly}})
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("Load err = %v", err)
	}
	if rep != nil {
		t.Fatalf("report = %+v, want none: Load never reached building one", rep)
	}
}

func TestServer_ConnectTimeoutDoesNotShortenTheSession(t *testing.T) {
	f := newFake(t)
	f.add("read_file", "Read a file.", readSchema, nil, textHandler("contents"))
	server := f.server()
	server.ConnectTimeout = 150 * time.Millisecond
	m, err := Pin(context.Background(), server)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	server2 := f.server()
	server2.ConnectTimeout = 150 * time.Millisecond
	tools, rep, err := Load(context.Background(), server2, m, Rules{"read_file": {SideEffect: agentrt.ReadOnly, Timeout: 2 * time.Second}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer rep.Connection.Close()

	// The phase's own timeout has long since fired; the session must still
	// answer, because the SDK does not cancel it when the context that
	// bounded Connect ends.
	time.Sleep(400 * time.Millisecond)
	res, err := call(t, tools[0], `{"path":"a"}`)
	if err != nil || text(t, res.Content) != "contents" {
		t.Fatalf("call after the phase timeout = %+v, %v", res, err)
	}
}

func TestPhaseErr_NamesTheStepOnlyWhenItsOwnDeadlineFired(t *testing.T) {
	// A caller cancellation, not a phase timeout, is reported as it is.
	err := phaseErr("fs", "connect", time.Second, nil, context.Canceled)
	if strings.Contains(err.Error(), "timed out after") {
		t.Errorf("err = %v, want cancellation left alone", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to still wrap context.Canceled", err)
	}
	err = phaseErr("fs", "list tools", 5*time.Second, nil, fmt.Errorf("read: %w", context.DeadlineExceeded))
	if !strings.Contains(err.Error(), `"fs"`) || !strings.Contains(err.Error(), "list tools") || !strings.Contains(err.Error(), "timed out after 5s") {
		t.Errorf("err = %v, want the step and bound named", err)
	}
}
