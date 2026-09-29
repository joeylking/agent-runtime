package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCall_DeadSessionFailsFastNamingTheServer kills the server side of a
// live session, as if its process had died, and checks that a call on it
// fails right away with an error naming the server, rather than hanging
// until the rule's own timeout.
func TestCall_DeadSessionFailsFastNamingTheServer(t *testing.T) {
	m, err := Pin(context.Background(), rawServer(t, func(string) string { return listing(readSchema) }))
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	client, server := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := server.Connect(ctx)
	if err != nil {
		t.Fatalf("connect raw server: %v", err)
	}
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
			switch req.Method {
			case "initialize":
				resp.Result = json.RawMessage(okInitialize)
			case "tools/list":
				resp.Result = json.RawMessage(listing(readSchema))
			default:
				resp.Error = &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "not here"}
			}
			if err := conn.Write(ctx, resp); err != nil {
				return
			}
		}
	}()
	s := Server{Name: "fs", DefaultTimeout: 5 * time.Second, transport: client}
	tools, rep, err := Load(context.Background(), s, m, Rules{"t0": {SideEffect: agentrt.ReadOnly}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { rep.Connection.Close() })

	// Kill the server side, as a dead process would: the pipe closes from
	// its end and nothing answers again.
	cancel()
	conn.Close()
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	_, err = call(t, tools[0], `{}`)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), `"fs"`) {
		t.Fatalf("err = %v, want an error naming the server", err)
	}
	if elapsed > time.Second {
		t.Fatalf("a dead session took %s to fail, want it fast, not bounded by the rule's 5s timeout", elapsed)
	}

	// Every later call fails the same way, fast, not only the first one.
	if _, err := call(t, tools[0], `{}`); err == nil || !strings.Contains(err.Error(), `"fs"`) {
		t.Fatalf("second call err = %v, want it to fail naming the server too", err)
	}
}

// TestCall_ReturnsWhenTheRuleTimeoutPassesEvenIfTheServerNeverAnswers uses a
// server that does not merely take its time, but never sends a response to
// tools/call at all: the call still returns at the rule's own bound, because
// the deadline is on our outgoing wait, not on the server's cooperation.
func TestCall_ReturnsWhenTheRuleTimeoutPassesEvenIfTheServerNeverAnswers(t *testing.T) {
	// rawServer answers every method it recognizes, tools/call included, so
	// a server that drops tools/call on the floor is built by hand here.
	client, server := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := server.Connect(ctx)
	if err != nil {
		t.Fatalf("connect raw server: %v", err)
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
			switch req.Method {
			case "initialize":
				conn.Write(ctx, &jsonrpc.Response{ID: req.ID, Result: json.RawMessage(okInitialize)})
			case "tools/list":
				conn.Write(ctx, &jsonrpc.Response{ID: req.ID, Result: json.RawMessage(listing(readSchema))})
			case "tools/call":
				// Never answered: the server has stopped responding.
			default:
				conn.Write(ctx, &jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "not here"}})
			}
		}
	}()
	s := Server{Name: "fs", DefaultTimeout: 5 * time.Second, transport: client}
	canon := canonOf(t, readSchema)
	manifest := &Manifest{Server: "fs", Tools: map[string]PinnedTool{
		"t0": {Name: "t0", Description: "d", InputSchema: canon, SchemaHash: hashBytes(canon), DescriptionHash: hashString("d")},
	}}
	tools, rep, err := Load(context.Background(), s, manifest, Rules{"t0": {SideEffect: agentrt.ReadOnly, Timeout: 100 * time.Millisecond}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { rep.Connection.Close() })

	start := time.Now()
	if _, err := call(t, tools[0], `{"path":"a"}`); err == nil {
		t.Fatal("a server that never answers must fail the call")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("call took %s, want it bounded by the rule's 100ms timeout", elapsed)
	}
}

// canonOf is the canonical form of a schema literal, as Pin would record it,
// so a hand-built manifest matches what build checks against.
func canonOf(t *testing.T, schema string) []byte {
	t.Helper()
	canon, err := canonicalSchema(json.RawMessage(schema))
	if err != nil {
		t.Fatalf("canonicalSchema: %v", err)
	}
	return canon
}

// TestConnection_CloseIsSafeTwiceAndConcurrentWithACall calls Close from two
// goroutines while a call is in flight on a third, under the race detector.
func TestConnection_CloseIsSafeTwiceAndConcurrentWithACall(t *testing.T) {
	f := newFake(t)
	f.add("slow", "Blocks until told to stop.", readSchema, nil, func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	m := mustPin(t, f)
	tools, rep, err := load(t, f, m, Rules{"slow": {SideEffect: agentrt.ReadOnly, Timeout: 2 * time.Second}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); call(t, tools[0], `{"path":"a"}`) }()
	go func() { defer wg.Done(); rep.Connection.Close() }()
	go func() { defer wg.Done(); rep.Connection.Close() }()
	wg.Wait()

	// A further, sequential close must still be harmless: no panic, no hang.
	done := make(chan struct{})
	go func() { rep.Connection.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a third Close did not return")
	}
}
