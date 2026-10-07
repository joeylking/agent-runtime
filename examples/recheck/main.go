// Command recheck shows a finished run checked again: a scripted agent
// reads, writes, and publishes under a lenient policy that allows all
// three; the run is then re-checked under a stricter policy, which prints
// the steps it would have stopped; last, one event is altered in a copy of
// the database, and the check agentrt verify runs finds it. It needs no
// model and makes no network call.
//
//	go run ./examples/recheck [-dir path]
//
// -dir defaults to a fresh temporary directory, removed afterwards; the
// database and its altered copy are written there.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/trace"
)

func main() {
	dir := flag.String("dir", "", "directory for the database and its altered copy (default: a fresh temporary one)")
	flag.Parse()
	if *dir == "" {
		tmp, err := os.MkdirTemp("", "agentrt-recheck-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		defer os.RemoveAll(tmp)
		*dir = tmp
	}
	if err := run(os.Stdout, *dir); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// tool answers with its arguments: what matters here is the decision about
// calling it.
type tool struct{ spec agentrt.ToolSpec }

func (t tool) Spec() agentrt.ToolSpec { return t.spec }

func (t tool) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	return agentrt.ToolResult{Content: c.Args, Summary: t.spec.Name + " done"}, nil
}

func newTool(name string, se agentrt.SideEffect) tool {
	return tool{agentrt.ToolSpec{Name: name, Description: name + " the notes", SideEffect: se, Timeout: time.Second,
		InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}}
}

// versioned names a side-effect policy by the version of the rules it was
// built from, which a map cannot do for itself, so each decision records
// which policy made it.
type versioned struct {
	agentrt.SideEffectPolicy
	id string
}

func (p versioned) PolicyID() string { return p.id }

var (
	lenient = versioned{agentrt.SideEffectPolicy{agentrt.ReadOnly: agentrt.Allow, agentrt.LocalMutation: agentrt.Allow, agentrt.RemoteMutation: agentrt.Allow}, "rules v1: mutations allowed"}
	strict  = versioned{agentrt.SideEffectPolicy{agentrt.ReadOnly: agentrt.Allow, agentrt.LocalMutation: agentrt.Allow, agentrt.RemoteMutation: agentrt.Deny}, "rules v2: nothing leaves the machine"}
)

func run(out io.Writer, dir string) error {
	ctx := context.Background()
	path := filepath.Join(dir, "runs.db")
	store, err := agentrt.OpenStore(path)
	if err != nil {
		return err
	}
	defer store.Close()

	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"path":"notes.md"}`, "read the notes"),
		scripted.ToolCall("write", `{"path":"summary.md"}`, "write the summary"),
		scripted.ToolCall("publish", `{"path":"summary.md"}`, "post the summary"),
		scripted.Complete(`{"published":"summary.md"}`),
	}}
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: agent, Policy: lenient,
		Tools: []agentrt.Tool{newTool("read", agentrt.ReadOnly), newTool("write", agentrt.LocalMutation), newTool("publish", agentrt.RemoteMutation)}})
	if err != nil {
		return err
	}
	r, err := d.Start(ctx, "summarize the notes and post them", agentrt.DefaultLimits())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "run %s ran under %q and is %s\n\n", r.ID, lenient.id, r.Status)

	// The same record, handed to the stricter policy evaluation by
	// evaluation, as the first policy saw it.
	rep, err := agentrt.Recheck(ctx, store, r.ID, strict, agentrt.RecheckOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "re-checked under %q:\n", strict.id)
	if err := trace.WriteRecheck(out, rep); err != nil {
		return err
	}
	for _, e := range rep.Evaluations {
		if e.Result == agentrt.RecheckDifferent && e.Rechecked != nil && e.Rechecked.Outcome == agentrt.Deny {
			fmt.Fprintf(out, "would have been stopped: step %d, %s\n", e.StepIndex, e.Tool)
		}
	}

	// A copy of the database with one event altered after the fact: the
	// policy decision on the publish now says it was denied.
	copyPath := filepath.Join(dir, "altered.db")
	if _, err := store.DB().ExecContext(ctx, `VACUUM INTO ?`, copyPath); err != nil {
		return err
	}
	altered, err := agentrt.OpenStore(copyPath)
	if err != nil {
		return err
	}
	defer altered.Close()
	var seq int64
	if err := altered.DB().QueryRowContext(ctx, `SELECT seq FROM events WHERE type = ? AND payload_json LIKE '%"outcome":"allow"%' ORDER BY seq DESC LIMIT 1`, agentrt.EventStepPolicy).Scan(&seq); err != nil {
		return err
	}
	if _, err := altered.DB().ExecContext(ctx, `UPDATE events SET payload_json = replace(payload_json, '"outcome":"allow"', '"outcome":"deny"') WHERE seq = ?`, seq); err != nil {
		return err
	}
	fmt.Fprintf(out, "\naltered event %d in a copy; the hash chain, as agentrt verify checks it:\n", seq)
	for _, s := range []*agentrt.Store{store, altered} {
		head, hash, err := s.ChainHead(ctx)
		if err != nil {
			return err
		}
		vr, err := s.VerifyEvents(ctx, 0, head)
		if err != nil {
			return err
		}
		name := "original"
		if s == altered {
			name = "copy"
		}
		if vr.Break != nil {
			fmt.Fprintf(out, "  %s: head seq %d %s; broken at seq %d: %s\n", name, head, hash[:12], vr.Break.Seq, vr.Break.Detail)
		} else {
			fmt.Fprintf(out, "  %s: head seq %d %s; intact: %d event(s)\n", name, head, hash[:12], vr.Checked)
		}
	}
	return nil
}
