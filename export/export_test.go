package export_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/export"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/trace"
)

type tool struct {
	spec agentrt.ToolSpec
	fn   func(agentrt.ToolCall) (agentrt.ToolResult, error)
}

func (t tool) Spec() agentrt.ToolSpec { return t.spec }
func (t tool) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	return t.fn(c)
}

// echo answers with its arguments; summary is what the step records.
func echo(summary string) agentrt.Tool {
	return tool{spec: agentrt.ToolSpec{Name: "echo", Description: "echo", InputSchema: []byte(`{"type":"object"}`), SideEffect: agentrt.ReadOnly, Timeout: time.Second},
		fn: func(c agentrt.ToolCall) (agentrt.ToolResult, error) {
			return agentrt.ToolResult{Content: c.Args, Summary: summary}, nil
		}}
}

// runScripted executes one run of n echo steps and a completion on store,
// through a real driver, and returns the run.
func runScripted(t *testing.T, store *agentrt.Store, n int, args string) agentrt.Run {
	t.Helper()
	var decisions []agentrt.Decision
	for i := 0; i < n; i++ {
		decisions = append(decisions, scripted.ToolCall("echo", args, fmt.Sprintf("step %d", i)))
	}
	decisions = append(decisions, scripted.Complete(`{"ok":true}`))
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: &scripted.Agent{Decisions: decisions}, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{echo("echoed")}})
	if err != nil {
		t.Fatal(err)
	}
	limits := agentrt.DefaultLimits()
	limits.MaxSteps = n + 1
	limits.LoopThreshold = n + 1
	run, err := d.Start(context.Background(), "goal", limits)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusCompleted {
		t.Fatalf("run %s %s", run.Status, run.Reason)
	}
	return run
}

func openStore(t *testing.T, path string) *agentrt.Store {
	t.Helper()
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func openReadOnly(t *testing.T, path string) *agentrt.Store {
	t.Helper()
	store, err := agentrt.OpenExisting(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// collect is a Sink that keeps what it was given.
type collect struct {
	mu     sync.Mutex
	events []agentrt.Event
}

func (c *collect) sink(e agentrt.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *collect) all() []agentrt.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]agentrt.Event(nil), c.events...)
}

func seqs(events []agentrt.Event) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.Seq
	}
	return out
}

// Every run's events arrive interleaved in Seq order, which is commit
// order, and equal what the store lists per run.
func TestFollower_DeliversEveryRunInCommitOrder(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "runs.db"))
	a := runScripted(t, store, 2, `{"n":1}`)
	b := runScripted(t, store, 3, `{"n":2}`)
	var got collect
	if err := (&export.Follower{Store: store, Once: true}).Follow(context.Background(), got.sink); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ea, _ := store.ListEvents(ctx, a.ID)
	eb, _ := store.ListEvents(ctx, b.ID)
	all := got.all()
	if len(all) != len(ea)+len(eb) {
		t.Fatalf("delivered %d events, store has %d", len(all), len(ea)+len(eb))
	}
	want := map[int64]agentrt.Event{}
	for _, e := range append(ea, eb...) {
		want[e.Seq] = e
	}
	for i, e := range all {
		if i > 0 && e.Seq <= all[i-1].Seq {
			t.Fatalf("event %d has seq %d after %d", i, e.Seq, all[i-1].Seq)
		}
		w, ok := want[e.Seq]
		if !ok {
			t.Fatalf("seq %d not in the store", e.Seq)
		}
		if e.RunID != w.RunID || e.StepID != w.StepID || e.Type != w.Type || !e.At.Equal(w.At) || !bytes.Equal(e.Payload, w.Payload) {
			t.Fatalf("seq %d: delivered %+v, store has %+v", e.Seq, e, w)
		}
	}
	// Run a alone, and in its own order.
	var onlyA collect
	if err := (&export.Follower{Store: store, RunID: a.ID, Once: true}).Follow(ctx, onlyA.sink); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seqs(onlyA.all())) != fmt.Sprint(seqs(ea)) {
		t.Fatalf("run %s: delivered %v, store has %v", a.ID, seqs(onlyA.all()), seqs(ea))
	}
}

// A follower restarted on a persisted cursor delivers only what came after
// the cursor, so across restarts every event is delivered exactly once.
func TestFollower_ExactlyOnceAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, filepath.Join(dir, "runs.db"))
	cursor := export.FileCursor{Path: filepath.Join(dir, "cursor")}
	runScripted(t, store, 2, `{}`)
	ctx := context.Background()
	var first collect
	if err := (&export.Follower{Store: store, Cursor: cursor, Once: true}).Follow(ctx, first.sink); err != nil {
		t.Fatal(err)
	}
	saved, err := cursor.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last := first.all()[len(first.all())-1].Seq
	if saved != last {
		t.Fatalf("cursor %d, last delivered %d", saved, last)
	}
	b := runScripted(t, store, 1, `{}`)
	var second collect
	if err := (&export.Follower{Store: store, Cursor: cursor, Once: true}).Follow(ctx, second.sink); err != nil {
		t.Fatal(err)
	}
	for _, e := range second.all() {
		if e.RunID != b.ID || e.Seq <= last {
			t.Fatalf("second follow delivered %+v from before the cursor", e)
		}
	}
	eb, _ := store.ListEvents(ctx, b.ID)
	if len(second.all()) != len(eb) {
		t.Fatalf("second follow delivered %d events, run has %d", len(second.all()), len(eb))
	}
	seen := map[int64]int{}
	for _, e := range append(first.all(), second.all()...) {
		seen[e.Seq]++
	}
	for seq, n := range seen {
		if n != 1 {
			t.Fatalf("seq %d delivered %d times", seq, n)
		}
	}
	// Nothing new: no delivery and the cursor stands.
	var third collect
	if err := (&export.Follower{Store: store, Cursor: cursor, Once: true}).Follow(ctx, third.sink); err != nil {
		t.Fatal(err)
	}
	if len(third.all()) != 0 {
		t.Fatalf("third follow delivered %d events", len(third.all()))
	}
	if saved, _ := cursor.Load(ctx); saved != second.all()[len(second.all())-1].Seq {
		t.Fatalf("cursor %d after an empty follow", saved)
	}
}

// A live follower sees a run committed after it started, and stops on
// cancellation with the cursor saved.
func TestFollower_LiveRunAndStopOnCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.db")
	store := openStore(t, path)
	runScripted(t, store, 1, `{}`) // so the read-only open finds a database
	reader := openReadOnly(t, path)
	cursor := &export.MemCursor{}
	var got collect
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (&export.Follower{Store: reader, Cursor: cursor, Interval: 10 * time.Millisecond}).Follow(ctx, got.sink)
	}()
	run := runScripted(t, store, 2, `{}`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		all := got.all()
		if len(all) > 0 && all[len(all)-1].RunID == run.ID && all[len(all)-1].Type == agentrt.EventRunFinished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run.finished of %s not delivered; have %d events", run.ID, len(all))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Follow returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Follow did not return after cancellation")
	}
	all := got.all()
	if cursor.Seq != all[len(all)-1].Seq {
		t.Fatalf("cursor %d, last delivered %d", cursor.Seq, all[len(all)-1].Seq)
	}
}

// A follower on a read-only store leaves the database file byte for byte
// as it was, with its modification time.
func TestFollower_ReadOnlyLeavesTheFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.db")
	store := openStore(t, path)
	runScripted(t, store, 3, `{}`)
	store.Close()
	before, binfo := fileState(t, path)
	time.Sleep(20 * time.Millisecond) // so a rewrite would move mtime
	reader := openReadOnly(t, path)
	var got collect
	if err := (&export.Follower{Store: reader, Cursor: export.FileCursor{Path: filepath.Join(dir, "cursor")}, Once: true, PageSize: 2}).Follow(context.Background(), got.sink); err != nil {
		t.Fatal(err)
	}
	if len(got.all()) == 0 {
		t.Fatal("nothing delivered")
	}
	reader.Close()
	after, ainfo := fileState(t, path)
	if before != after {
		t.Fatal("database content changed under a read-only follower")
	}
	if !binfo.ModTime().Equal(ainfo.ModTime()) {
		t.Fatalf("database mtime moved from %v to %v", binfo.ModTime(), ainfo.ModTime())
	}
	if binfo.Mode() != ainfo.Mode() {
		t.Fatalf("database mode changed from %v to %v", binfo.Mode(), ainfo.Mode())
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		if info, _ := os.Stat(path + "-wal"); info.Size() != 0 {
			t.Fatalf("a read-only follower wrote %d bytes of WAL", info.Size())
		}
	}
}

func fileState(t *testing.T, path string) ([32]byte, os.FileInfo) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b), info
}

// Many events are read a small page at a time, in order and complete, and
// a payload past agentrt.MaxPageText arrives capped as the store's own
// page reads cap it, so one row cannot make the follower read without
// bound.
func TestFollower_PagesALargeTable(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "runs.db"))
	big := `{"s":"` + strings.Repeat("x", agentrt.MaxPageText+10) + `"}`
	run := runScripted(t, store, 40, big)
	var got collect
	if err := (&export.Follower{Store: store, Once: true, PageSize: 7}).Follow(context.Background(), got.sink); err != nil {
		t.Fatal(err)
	}
	events, _ := store.ListEvents(context.Background(), run.ID)
	all := got.all()
	if len(all) != len(events) {
		t.Fatalf("delivered %d of %d events", len(all), len(events))
	}
	capped := 0
	for i, e := range all {
		if e.Seq != events[i].Seq {
			t.Fatalf("event %d: seq %d, want %d", i, e.Seq, events[i].Seq)
		}
		if len(events[i].Payload) <= agentrt.MaxPageText {
			continue
		}
		var m struct {
			Truncated bool   `json:"truncated"`
			Length    int    `json:"length"`
			Head      string `json:"head"`
		}
		if err := json.Unmarshal(e.Payload, &m); err != nil || !m.Truncated || m.Length != len(events[i].Payload) || len(m.Head) == 0 {
			t.Fatalf("event %d: capped payload %.80s... decodes as %+v, %v", i, e.Payload, m, err)
		}
		capped++
	}
	if capped == 0 {
		t.Fatal("no payload exceeded the cap")
	}
}

// A sink error stops the follower and the cursor names the last event
// delivered before it, so a restart begins with the event that failed.
func TestFollower_SinkErrorStopsWithCursorBeforeIt(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "runs.db"))
	run := runScripted(t, store, 2, `{}`)
	events, _ := store.ListEvents(context.Background(), run.ID)
	boom := errors.New("boom")
	cursor := &export.MemCursor{}
	n := 0
	err := (&export.Follower{Store: store, Cursor: cursor, Once: true}).Follow(context.Background(), func(agentrt.Event) error {
		n++
		if n == 4 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Follow returned %v", err)
	}
	if cursor.Seq != events[2].Seq {
		t.Fatalf("cursor %d, want %d (the third event)", cursor.Seq, events[2].Seq)
	}
}

func TestFollower_RejectsBadSettings(t *testing.T) {
	store := openStore(t, ":memory:")
	ctx := context.Background()
	sink := func(agentrt.Event) error { return nil }
	if err := (&export.Follower{}).Follow(ctx, sink); err == nil {
		t.Fatal("nil store accepted")
	}
	if err := (&export.Follower{Store: store}).Follow(ctx, nil); err == nil {
		t.Fatal("nil sink accepted")
	}
	if err := (&export.Follower{Store: store, Interval: -1}).Follow(ctx, sink); err == nil {
		t.Fatal("negative interval accepted")
	}
	if err := (&export.Follower{Store: store, PageSize: -1}).Follow(ctx, sink); err == nil {
		t.Fatal("negative page size accepted")
	}
}

func TestFileCursor_LoadsZeroWhenMissingAndRefusesGarbage(t *testing.T) {
	dir := t.TempDir()
	c := export.FileCursor{Path: filepath.Join(dir, "cursor")}
	ctx := context.Background()
	if seq, err := c.Load(ctx); err != nil || seq != 0 {
		t.Fatalf("missing cursor: %d, %v", seq, err)
	}
	if err := c.Save(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if seq, err := c.Load(ctx); err != nil || seq != 42 {
		t.Fatalf("saved cursor: %d, %v", seq, err)
	}
	info, _ := os.Stat(c.Path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cursor mode %v", info.Mode())
	}
	os.WriteFile(c.Path, []byte("not a number"), 0o600)
	if _, err := c.Load(ctx); err == nil {
		t.Fatal("garbage cursor loaded")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// The JSON Lines sink writes exactly what trace.JSONL writes.
func TestJSONL_MatchesTrace(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "runs.db"))
	var fromTrace bytes.Buffer
	obs := trace.JSONL(&fromTrace)
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("echo", `{"a":"<b>&"}`, "r"), scripted.Complete(`{}`)}}, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{echo("s")}, Observer: obs,
		// The store keeps times in UTC; a live observer sees the clock as read.
		Now: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(context.Background(), "g", agentrt.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	var fromExport bytes.Buffer
	if err := (&export.Follower{Store: store, Once: true}).Follow(context.Background(), export.JSONL(&fromExport)); err != nil {
		t.Fatal(err)
	}
	if fromTrace.String() != fromExport.String() {
		t.Fatalf("records differ:\n%s\n----\n%s", fromTrace.String(), fromExport.String())
	}
	// A write error is returned.
	err = (&export.Follower{Store: store, Once: true}).Follow(context.Background(), export.JSONL(failWriter{}))
	if err == nil {
		t.Fatal("write error not returned")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// A follower that dies without returning, as a killed process does,
// saves nothing for the page it was delivering: the next follower on the
// same cursor delivers that page's events again. Delivery is at least
// once across a hard kill, and the redelivery stops at the page.
func TestFollower_AtLeastOnceAfterAHardKill(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, filepath.Join(dir, "runs.db"))
	cursor := export.FileCursor{Path: filepath.Join(dir, "cursor")}
	runScripted(t, store, 3, `{}`)
	ctx := context.Background()
	const page = 4
	var first collect
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = (&export.Follower{Store: store, Cursor: cursor, Once: true, PageSize: page}).Follow(ctx, func(e agentrt.Event) error {
			if len(first.all()) == page+2 {
				runtime.Goexit() // the process dies inside the second page
			}
			return first.sink(e)
		})
	}()
	<-done
	saved, err := cursor.Load(ctx)
	if err != nil || saved != first.all()[page-1].Seq {
		t.Fatalf("cursor %d, %v; want the first page's last, %d", saved, err, first.all()[page-1].Seq)
	}
	var second collect
	if err := (&export.Follower{Store: store, Cursor: cursor, Once: true, PageSize: page}).Follow(ctx, second.sink); err != nil {
		t.Fatal(err)
	}
	again := first.all()[page:]
	if len(again) != 2 || len(second.all()) < 2 || fmt.Sprint(seqs(second.all()[:2])) != fmt.Sprint(seqs(again)) {
		t.Fatalf("delivered after the kill %v, then %v; want %v delivered again", seqs(again), seqs(second.all()), seqs(again))
	}
	all, _ := store.ListEventsAfter(ctx, "", 0, 1000)
	if got := seqs(append(first.all()[:page], second.all()...)); fmt.Sprint(got) != fmt.Sprint(seqs(all)) {
		t.Fatalf("delivered %v, store has %v", got, seqs(all))
	}
}
