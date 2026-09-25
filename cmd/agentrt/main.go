// Command agentrt is the operator surface over a runs database: list runs,
// show one, print its event log, and approve, reject, or cancel. It reads
// and decides; it never executes a step.
//
//	agentrt [-db path] [-json] <command> [arguments]
//
// The database is taken from -db or AGENTRT_DB. Exit status is 0 on success,
// 1 on error, and 2 on a usage problem.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/trace"
	"github.com/joeylking/agent-runtime/view"
)

const usage = `agentrt is the operator surface over an agent-runtime database.

usage: agentrt [-db path] [-json] <command> [arguments]

commands:
  runs                  list every run, newest first
  show <run>            the run, its steps, and any approval waiting
  events <run>          the run's audit log
  approve <run>         grant the run's pending approval
  reject <run>          refuse it, which cancels the run
  cancel <run>          end a run that is not terminal

global flags:
  -db path              SQLite database (default: $AGENTRT_DB)
  -json                 emit JSON instead of aligned text

approve and reject flags:
  -approval id          which approval (default: the run's only pending one)
  -by who               who decided (default: $USER)
  -note text            note recorded with the decision

cancel flags:
  -by who, -note text   as above

There is no resume command. Resuming a run executes the approved request and
continues the loop, which needs the consumer's agent, its tools, and its
policy, so resume belongs in the consumer's own command:

  run, err := driver.Resume(ctx, runID)

exit status: 0 success, 1 error, 2 usage.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// errUsage marks an error the operator can fix by reading the usage text.
var errUsage = errors.New("usage")

// env is what every command needs: the store, the output format, and where
// to write.
type env struct {
	store *agentrt.Store
	path  string
	json  bool
	out   io.Writer
	errw  io.Writer
}

func run(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("agentrt", flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.Usage = func() { fmt.Fprint(errw, usage) }
	db := fs.String("db", "", "SQLite database (default: $AGENTRT_DB)")
	asJSON := fs.Bool("json", false, "emit JSON instead of aligned text")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return exitOK
		}
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(errw, usage)
		return exitUsage
	}
	if rest[0] == "help" {
		fmt.Fprint(out, usage)
		return exitOK
	}
	path := *db
	if path == "" {
		path = os.Getenv("AGENTRT_DB")
	}
	if path == "" {
		return fail(errw, fmt.Errorf("%w: no database: pass -db or set AGENTRT_DB", errUsage))
	}
	store, err := agentrt.OpenStore(path)
	if err != nil {
		return fail(errw, err)
	}
	defer store.Close()

	e := &env{store: store, path: path, json: *asJSON, out: out, errw: errw}
	ctx := context.Background()
	cmd, cargs := rest[0], rest[1:]
	switch cmd {
	case "runs":
		err = cmdRuns(ctx, e, cargs)
	case "show":
		err = cmdShow(ctx, e, cargs)
	case "events":
		err = cmdEvents(ctx, e, cargs)
	case "approve":
		err = cmdDecide(ctx, e, cargs, true)
	case "reject":
		err = cmdDecide(ctx, e, cargs, false)
	case "cancel":
		err = cmdCancel(ctx, e, cargs)
	default:
		err = fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
	if err != nil {
		return fail(errw, err)
	}
	return exitOK
}

func fail(errw io.Writer, err error) int {
	if errors.Is(err, errUsage) {
		fmt.Fprintln(errw, err)
		fmt.Fprint(errw, usage)
		return exitUsage
	}
	fmt.Fprintln(errw, "error:", err)
	return exitError
}

func cmdRuns(ctx context.Context, e *env, args []string) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	fs.SetOutput(e.errw)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	rows, err := view.Runs(ctx, e.store)
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.out, "no runs")
		return nil
	}
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATUS\tREASON\tSTEPS\tCREATED\tAPPROVAL\tGOAL")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", r.ID, r.Status, dash(string(r.Reason)), r.Steps, stamp(r.CreatedAt), dash(r.PendingApprovalID), clip(r.Goal, 60))
	}
	return tw.Flush()
}

func cmdShow(ctx context.Context, e *env, args []string) error {
	runID, rest, err := positional(args, "show: expected a run id")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(e.errw)
	if err := fs.Parse(rest); err != nil {
		return errUsage
	}
	summary, err := view.Summary(ctx, e.store, runID)
	if err != nil {
		return err
	}
	steps, err := view.Steps(ctx, e.store, runID)
	if err != nil {
		return err
	}
	approvals, err := e.store.ListApprovals(ctx, runID)
	if err != nil {
		return err
	}
	var waiting *agentrt.Approval
	if summary.PendingApprovalID != "" {
		if a, err := view.PendingApproval(ctx, e.store, runID, summary.PendingApprovalID); err == nil {
			waiting = &a
		}
	}
	if e.json {
		return printJSON(e.out, struct {
			Run       view.RunSummary    `json:"run"`
			Steps     []view.StepSummary `json:"steps"`
			Approvals []agentrt.Approval `json:"approvals,omitempty"`
			Waiting   *agentrt.Approval  `json:"waiting_approval,omitempty"`
		}{Run: summary, Steps: steps, Approvals: approvals, Waiting: waiting})
	}
	fmt.Fprintf(e.out, "run     %s\n", summary.ID)
	fmt.Fprintf(e.out, "goal    %s\n", summary.Goal)
	fmt.Fprintf(e.out, "status  %s", summary.Status)
	if summary.Reason != "" {
		fmt.Fprintf(e.out, " (%s)", summary.Reason)
	}
	if summary.ReasonDetail != "" {
		fmt.Fprintf(e.out, ": %s", summary.ReasonDetail)
	}
	fmt.Fprintf(e.out, "\nsteps   %d\n", summary.Steps)
	fmt.Fprintf(e.out, "created %s", stamp(summary.CreatedAt))
	if !summary.FinishedAt.IsZero() {
		fmt.Fprintf(e.out, "  finished %s", stamp(summary.FinishedAt))
	}
	fmt.Fprintln(e.out)
	if summary.ModelCalls > 0 {
		fmt.Fprintf(e.out, "model   %d call(s), %d in, %d out, estimated %s\n", summary.ModelCalls, summary.Usage.InputTokens, summary.Usage.OutputTokens, dollars(summary.EstimatedCost))
	}
	if waiting != nil {
		printWaiting(e, *waiting)
	}
	if len(steps) == 0 {
		return nil
	}
	fmt.Fprintln(e.out)
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tTOOL\tDECISION\tSTATUS\tPOLICY\tOBSERVATION\tSUMMARY")
	for _, s := range steps {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Index, dash(s.Tool), dash(string(s.Decision)), s.Status, dash(string(s.Policy)), dash(string(s.Observation)), clip(s.Summary, 60))
	}
	return tw.Flush()
}

// printWaiting puts the decision in front of the operator: what kind of
// approval is asked for and exactly what they would be granting.
func printWaiting(e *env, a agentrt.Approval) {
	fmt.Fprintf(e.out, "\nAPPROVAL WAITING  %s\n", a.ID)
	fmt.Fprintf(e.out, "kind    %s\n", a.Kind)
	fmt.Fprintf(e.out, "tool    %s %s\n", a.Request.Spec.Name, string(a.Request.Args))
	if !a.ExpiresAt.IsZero() {
		fmt.Fprintf(e.out, "expires %s\n", stamp(a.ExpiresAt))
	}
	fmt.Fprintln(e.out, "presentation")
	var buf bytes.Buffer
	if err := json.Indent(&buf, a.Presentation, "  ", "  "); err != nil {
		fmt.Fprintf(e.out, "  %s\n", string(a.Presentation))
	} else {
		fmt.Fprintf(e.out, "  %s\n", buf.String())
	}
	fmt.Fprintf(e.out, "decide with: agentrt -db %s approve %s\n", e.path, a.RunID)
}

func cmdEvents(ctx context.Context, e *env, args []string) error {
	runID, rest, err := positional(args, "events: expected a run id")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(e.errw)
	if err := fs.Parse(rest); err != nil {
		return errUsage
	}
	if _, err := e.store.GetRun(ctx, runID); err != nil {
		return err
	}
	events, err := e.store.ListEvents(ctx, runID)
	if err != nil {
		return err
	}
	observer := trace.Writer(e.out)
	if e.json {
		observer = trace.JSONL(e.out)
	}
	for _, ev := range events {
		observer(ev)
	}
	return nil
}

func cmdDecide(ctx context.Context, e *env, args []string, approve bool) error {
	verb := "reject"
	if approve {
		verb = "approve"
	}
	runID, rest, err := positional(args, verb+": expected a run id")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(e.errw)
	approvalID := fs.String("approval", "", "approval id (default: the run's only pending approval)")
	by := fs.String("by", "", "who decided (default: $USER)")
	note := fs.String("note", "", "note recorded with the decision")
	if err := fs.Parse(rest); err != nil {
		return errUsage
	}
	a, err := view.PendingApproval(ctx, e.store, runID, *approvalID)
	if err != nil {
		return err
	}
	obs := trace.Writer(e.errw)
	if approve {
		err = agentrt.Approve(ctx, e.store, obs, runID, a.ID, who(*by), *note)
	} else {
		err = agentrt.Reject(ctx, e.store, obs, runID, a.ID, who(*by), *note)
	}
	if err != nil {
		return err
	}
	decided, err := e.store.GetApproval(ctx, runID, a.ID)
	if err != nil {
		return err
	}
	return e.report(ctx, runID, &decided, approve)
}

func cmdCancel(ctx context.Context, e *env, args []string) error {
	runID, rest, err := positional(args, "cancel: expected a run id")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(e.errw)
	by := fs.String("by", "", "who cancelled (default: $USER)")
	note := fs.String("note", "", "note recorded with the cancellation")
	if err := fs.Parse(rest); err != nil {
		return errUsage
	}
	if err := agentrt.Cancel(ctx, e.store, trace.Writer(e.errw), runID, who(*by), *note); err != nil {
		return err
	}
	return e.report(ctx, runID, nil, false)
}

// report prints what the run looks like after a decision, and what the
// operator has to do next: approving does not resume the run.
func (e *env) report(ctx context.Context, runID string, a *agentrt.Approval, approved bool) error {
	summary, err := view.Summary(ctx, e.store, runID)
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, struct {
			Run      view.RunSummary   `json:"run"`
			Approval *agentrt.Approval `json:"approval,omitempty"`
		}{Run: summary, Approval: a})
	}
	if a != nil {
		fmt.Fprintf(e.out, "approval %s: %s by %s\n", a.ID, a.Status, a.DecidedBy)
	}
	fmt.Fprintf(e.out, "run %s: %s", summary.ID, summary.Status)
	if summary.Reason != "" {
		fmt.Fprintf(e.out, " (%s)", summary.Reason)
	}
	fmt.Fprintln(e.out)
	if approved && summary.Status == agentrt.StatusWaitingForApproval {
		fmt.Fprintln(e.out, "the run stays waiting until the consumer resumes it")
	}
	return nil
}

func positional(args []string, what string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("%w: %s", errUsage, what)
	}
	return args[0], args[1:], nil
}

func who(by string) string {
	if by != "" {
		return by
	}
	return os.Getenv("USER")
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func stamp(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }

func dollars(m agentrt.Micros) string { return fmt.Sprintf("$%.4f", float64(m)/1e6) }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
