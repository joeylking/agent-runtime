package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A panic inside a transaction must roll it back: the store has one
// connection, and a transaction left open would wedge every later call.
func TestStore_PanicInTransactionRollsBack(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not propagate")
			}
		}()
		store.tx(ctx, nil, func(t *txn) error {
			if err := t.insertRun(ctx, Run{ID: "r1", Goal: "g", Status: StatusRunning, CreatedAt: time.Now()}); err != nil {
				return err
			}
			panic("boom")
		})
	}()
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	runs, err := store.ListRuns(tctx)
	if err != nil {
		t.Fatalf("store unusable after a panic: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("the panicking transaction committed: %+v", runs)
	}
}

// Two processes opening a new file at once must both succeed: the version
// is read and the migrations applied in one IMMEDIATE transaction.
func TestStore_ConcurrentFirstOpen(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		path := filepath.Join(t.TempDir(), "c.db")
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				s, err := OpenStore(path)
				if err == nil {
					err = s.Close()
				}
				errs[i] = err
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("trial %d: %v", trial, err)
			}
		}
		s, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n)
		s.Close()
		if n != len(migrations) {
			t.Fatalf("trial %d: %d migrations recorded, want %d", trial, n, len(migrations))
		}
	}
}

func TestStore_NewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, '')`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := OpenStore(path); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("err = %v, want ErrSchemaVersion", err)
	}
	if _, err := OpenExisting(path, true); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("OpenExisting err = %v, want ErrSchemaVersion", err)
	}
}

// RFC3339Nano drops trailing zeros, so ".15Z" sorts before ".1Z" as text.
// Lists are in insertion order, which is creation order.
func TestStore_ListsInInsertionOrder(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	earlier, later := base.Add(100*time.Millisecond), base.Add(150*time.Millisecond)
	if formatTime(later) >= formatTime(earlier) {
		t.Fatal("the fixture no longer exercises text ordering")
	}
	lim := Limits{MaxSteps: 1, MaxConsecutiveToolFailures: 1, LoopThreshold: 1}
	err = store.tx(ctx, nil, func(t *txn) error {
		for i, at := range []time.Time{earlier, later} {
			id := fmt.Sprint("r", i)
			if err := t.insertRun(ctx, Run{ID: id, Goal: "g", Status: StatusRunning, Limits: lim, CreatedAt: at}); err != nil {
				return err
			}
			if err := t.insertApproval(ctx, Approval{ID: fmt.Sprint("a", i), RunID: "r0", StepID: "s", Kind: "k", Status: ApprovalPending, CreatedAt: at}); err != nil {
				return err
			}
			if err := t.insertModelCall(ctx, ModelCall{ID: fmt.Sprint("m", i), RunID: "r0", StepID: "s", Attempt: i + 1, Status: CallDispatched, Model: "m", DispatchedAt: at}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := store.ListRuns(ctx)
	if len(runs) != 2 || runs[0].ID != "r1" {
		t.Fatalf("ListRuns newest first = %s, %s", runs[0].ID, runs[1].ID)
	}
	approvals, _ := store.ListApprovals(ctx, "r0")
	if len(approvals) != 2 || approvals[0].ID != "a0" {
		t.Fatalf("ListApprovals = %s, %s", approvals[0].ID, approvals[1].ID)
	}
	calls, _ := store.ListModelCalls(ctx, "r0")
	if len(calls) != 2 || calls[0].ID != "m0" {
		t.Fatalf("ListModelCalls = %s, %s", calls[0].ID, calls[1].ID)
	}
}

// Of two granted approvals for one step, Resume executes the one granted
// last, whatever its timestamp sorts like as text.
func TestResume_GrantedApprovalIsTheLatestInserted(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	publish := &stubTool{spec: ToolSpec{Name: "publish", Description: "p", InputSchema: []byte(`{"type":"object"}`), SideEffect: RemoteMutation, Timeout: time.Second}}
	run := Run{ID: "r1", Goal: "g", Status: StatusWaitingForApproval, Limits: Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}, StepCount: 1, CreatedAt: base, StartedAt: base}
	step := Step{ID: "s0", RunID: "r1", Index: 0, Status: StepAwaitingApproval, Decision: &Decision{Kind: DecideToolCall, Tool: "publish", Args: []byte(`{"n":1}`)}, StartedAt: base}
	grant := func(id, args string, at time.Time) Approval {
		req := ToolRequest{RunID: "r1", StepID: "s0", Spec: publish.spec, Args: json.RawMessage(args)}
		pd, _ := DefaultPolicy().Evaluate(ctx, req, RunView{})
		a := Approval{ID: id, RunID: "r1", StepID: "s0", Kind: pd.Kind, Capability: pd.Capability, Presentation: pd.Presentation, Request: req, Status: ApprovalApproved, CreatedAt: at, DecidedAt: at}
		a.Hash, _ = approvalHash(a.Kind, a.Capability, a.Presentation, a.Request)
		return a
	}
	if err := store.tx(ctx, nil, func(t *txn) error {
		if err := t.insertRun(ctx, run); err != nil {
			return err
		}
		if err := t.insertStep(ctx, step); err != nil {
			return err
		}
		if err := t.insertApproval(ctx, grant("old", `{"n":1}`, base.Add(100*time.Millisecond))); err != nil {
			return err
		}
		return t.insertApproval(ctx, grant("new", `{"n":2}`, base.Add(150*time.Millisecond)))
	}); err != nil {
		t.Fatal(err)
	}
	var got []json.RawMessage
	tool := &argsTool{stubTool: publish, seen: &got}
	d, err := NewDriver(Config{Store: store, Agent: &listAgent{decisions: []Decision{{}, {Kind: DecideComplete}}}, Policy: DefaultPolicy(), Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resume(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0]) != `{"n":2}` {
		t.Fatalf("executed %s, want the latest grant {\"n\":2}", got)
	}
}

type argsTool struct {
	*stubTool
	seen *[]json.RawMessage
}

func (a *argsTool) Call(_ context.Context, c ToolCall) (ToolResult, error) {
	*a.seen = append(*a.seen, c.Args)
	return ToolResult{Content: []byte(`{}`)}, nil
}

func TestIsConnectionLost_MatchesErrorsNotText(t *testing.T) {
	for _, err := range []error{
		io.EOF,
		fmt.Errorf("read body: %w", io.ErrUnexpectedEOF),
		&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		fmt.Errorf("post: %w", &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}),
		TransientError{Err: fmt.Errorf("wrapped: %w", syscall.ECONNRESET)},
	} {
		if !isConnectionLost(err) {
			t.Errorf("%v: not classified as connection lost", err)
		}
	}
	for _, err := range []error{
		errors.New("json: cannot decode: unexpected EOF"),
		errors.New("decode response: EOF in string"),
		errors.New("connection reset by peer (as text only)"),
		&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
	} {
		if isConnectionLost(err) {
			t.Errorf("%v: classified as connection lost", err)
		}
	}
}
