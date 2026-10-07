// Command agentrt is the operator surface over a runs database: list runs,
// show one, print its event log, verify the event log's hash chain, and
// approve, reject, or cancel. It reads and decides; it never executes a
// step, and it never creates or migrates the database it is pointed at.
//
//	agentrt [-db path] [-json] <command> [arguments]
//
// The database is taken from -db or AGENTRT_DB. Exit status is 0 on success,
// 1 on error, and 2 on a usage problem.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/trace"
	"github.com/joeylking/agent-runtime/view"
)

const usage = `agentrt is the operator surface over an agent-runtime database.

usage: agentrt [-db path] [-json] <command> [arguments]

commands:
  runs                  list every run, newest first
  show <run>            the run, its steps, and every approval waiting
  events <run>          the run's audit log
  approve <run>         grant the run's pending approval
  reject <run>          refuse it, which cancels the run
  cancel <run>          end a run that is not terminal
  verify                check the hash chain over every run's events

global flags:
  -db path              SQLite database (default: $AGENTRT_DB)
  -json                 emit JSON instead of aligned text

runs, show, and events flags (show and events: come after the run id):
  -limit n              rows to show: runs, or steps and pending approvals,
                        or events (default: 100 for runs, 500 for show and
                        events; 0: unlimited, read a page at a time)
  -offset n             skip that many first (default: 0)

approve and reject flags (may come before or after the run id):
  -approval id          which approval (default: the run's only pending one)
  -by who               who decided (default: $USER); unauthenticated: this
                         is not a login, and whatever is given is recorded
                         exactly as given, never verified
  -note text             note recorded with the decision
  -yes                   confirm without prompting

approve and reject print the approval waiting before deciding anything, and
require one of -approval, -yes, or typing y at an interactive prompt; run
with none of those and stdin not a terminal, they refuse rather than guess.
-by empty and $USER unset is refused the same way.

cancel flags (may come before or after the run id):
  -by who, -note text   as above, without the confirmation requirement

verify flags:
  -from n               first event to check, by seq (default: the first)
  -to n                 last event to check, by seq (default: the newest);
                        a -to past the last event fails
  -head seq:hash        a head kept elsewhere (export.Follower.Head, or an
                        earlier verify's last checked): fails unless the
                        walk reaches seq and its event still has that hash

verify prints the chain's head, then either "intact" with the seq and hash
of the last event checked, or the first event that does not chain. It exits
1 for a break, for a -to not reached, and for a -head not reached or not
matched. It reads a page at a time and writes nothing. An intact chain
shows the events were not altered only up to a head kept where whoever can
write the database cannot reach: such a writer can recompute every hash
after a row they changed, or cut events from the end, and the chain still
verifies. Keep the last checked seq and hash, and pass them as -head next
time.

agentrt never creates a missing database and never migrates one on a
different schema version than this build: that is the consumer's job.

There is no resume command. Resuming a run executes the approved request and
continues the loop, which needs the consumer's agent, its tools, and its
policy, so resume belongs in the consumer's own command:

  run, err := driver.Resume(ctx, runID)

There is no recheck command for the same reason: re-checking a run's
decisions hands them to the consumer's policy, so it belongs in the
consumer's own command, or in its tests (testkit.Recheck):

  report, err := agentrt.Recheck(ctx, store, runID, policy, agentrt.RecheckOptions{})
  trace.WriteRecheck(os.Stdout, report)

exit status: 0 success, 1 error, 2 usage.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, isTerminal(os.Stdin)))
}

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// errUsage marks an error the operator can fix by reading the usage text.
var errUsage = errors.New("usage")

// errNoAnswer marks confirm's read reaching end of input before an answer
// was typed. It is not a decline: stdin passed a terminal check but gave
// nothing to read (a closed pipe, a redirect that looked like a terminal),
// and the caller refuses the same way as when there was no terminal to ask
// at, rather than recording it as a typed "n".
var errNoAnswer = errors.New("no answer before end of input")

// env is what a command needs once its arguments are known good: the store,
// the output format, where to write, and what to read a confirmation from.
type env struct {
	store       *agentrt.Store
	path        string
	json        bool
	out         io.Writer
	errw        io.Writer
	stdin       io.Reader
	interactive bool
}

// command is a fully validated invocation: which way to open the database,
// and what to run once it is open. Building one never touches the database,
// which is what lets every argument be checked before anything is opened.
type command struct {
	readOnly bool
	exec     func(ctx context.Context, e *env) error
}

func run(args []string, stdin io.Reader, out, errw io.Writer, interactive bool) int {
	fs := newFlagSet("agentrt")
	db := fs.String("db", "", "SQLite database (default: $AGENTRT_DB)")
	asJSON := fs.Bool("json", false, "emit JSON instead of aligned text")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return exitOK
		}
		return fail(errw, fmt.Errorf("%w: %v", errUsage, err))
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
	cmd, cargs := rest[0], rest[1:]

	// Every argument is validated before the database is touched: a typo in
	// a flag or a missing run id must not create, migrate, or even open the
	// file it names.
	spec, err := parse(cmd, cargs)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return exitOK
		}
		return fail(errw, err)
	}

	path := *db
	if path == "" {
		path = os.Getenv("AGENTRT_DB")
	}
	if path == "" {
		return fail(errw, fmt.Errorf("%w: no database: pass -db or set AGENTRT_DB", errUsage))
	}

	store, err := agentrt.OpenExisting(path, spec.readOnly)
	if err != nil {
		if msg, ok := openErrorMessage(err, path); ok {
			fmt.Fprint(errw, msg)
			return exitError
		}
		return fail(errw, err)
	}
	defer store.Close()

	e := &env{store: store, path: path, json: *asJSON, out: out, errw: errw, stdin: stdin, interactive: interactive}
	if err := spec.exec(context.Background(), e); err != nil {
		return fail(errw, err)
	}
	return exitOK
}

// schemaVersionRe pulls the two version numbers out of ErrSchemaVersion's
// message so the operator is told which side is newer instead of just two
// numbers.
var schemaVersionRe = regexp.MustCompile(`at version (\d+), this build writes (\d+)`)

func schemaMessage(err error, path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "error: %v\n", err)
	if m := schemaVersionRe.FindStringSubmatch(err.Error()); m != nil {
		dbVer, _ := strconv.Atoi(m[1])
		buildVer, _ := strconv.Atoi(m[2])
		switch {
		case dbVer > buildVer:
			fmt.Fprintf(&b, "%s is newer than this build of agentrt: it was written by a later schema.\n", path)
		case dbVer < buildVer:
			fmt.Fprintf(&b, "this build of agentrt is newer than %s: its schema is behind.\n", path)
		}
	}
	fmt.Fprintln(&b, "agentrt does not migrate a database; the consumer that owns it is responsible for its migrations.")
	return b.String()
}

// openErrorMessage turns an error from OpenExisting into the one clear,
// actionable sentence an operator needs: a schema version this build does
// not write, an insecure file mode, a database carrying a trigger or a
// view, and a symbolic link named as the database. Each is matched with
// errors.Is or errors.As, never by its text. ok is false for any other
// error, which the caller passes to fail unchanged.
func openErrorMessage(err error, path string) (msg string, ok bool) {
	switch {
	case errors.Is(err, agentrt.ErrSchemaVersion):
		return schemaMessage(err, path), true
	case errors.Is(err, agentrt.ErrUnsafeSchema):
		// ErrUnsafeSchema's own text ("database holds a trigger or view")
		// already says what is wrong; this adds why agentrt refuses it
		// rather than the operator having to know.
		return fmt.Sprintf("error: %v: a trigger or a view could run SQL you did not write inside agentrt's own statements, so it is refused rather than opened.\n", err), true
	case isInsecureMode(err):
		// ErrInsecureMode.Error() is already one complete, actionable
		// sentence (it names the file, its mode, and the chmod/chown fix).
		return fmt.Sprintf("error: %v\n", err), true
	case errors.Is(err, agentrt.ErrSymlink):
		return fmt.Sprintf("error: %v\n", err), true
	default:
		return "", false
	}
}

// isInsecureMode reports whether err is, or wraps, agentrt.ErrInsecureMode.
func isInsecureMode(err error) bool {
	var e agentrt.ErrInsecureMode
	return errors.As(err, &e)
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

// newFlagSet returns a FlagSet whose own output and usage text are
// suppressed: every message an operator sees is composed exactly once, by
// fail and the -h path in run, rather than partly by the flag package and
// partly by this command.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parseErr turns a flag.FlagSet parse error into an errUsage-wrapped error
// carrying the specific message, or passes flag.ErrHelp through unchanged
// so the caller can print the one usage text to stdout and exit 0.
func parseErr(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return fmt.Errorf("%w: %v", errUsage, err)
}

// reorder splits a command's arguments into its positional arguments and
// its flag tokens, so a flag may be written before or after the positional
// run id. The flag package alone stops consuming flags at the first
// non-flag argument, which is too strict for "approve <run> -by alice".
func reorder(fs *flag.FlagSet, args []string) (positional, flagArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue // value is part of this token
		}
		fl := fs.Lookup(name)
		if fl == nil {
			continue // unknown flag: fs.Parse reports it
		}
		if b, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue // a bool flag takes no separate value
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return positional, flagArgs
}

// parse validates one command's arguments against the database and returns
// how to run it. It never touches the database.
func parse(cmd string, args []string) (*command, error) {
	switch cmd {
	case "runs":
		return parseRuns(args)
	case "show":
		return parseShow(args)
	case "events":
		return parseEvents(args)
	case "approve":
		return parseDecide(args, true)
	case "reject":
		return parseDecide(args, false)
	case "cancel":
		return parseCancel(args)
	case "verify":
		return parseVerify(args)
	default:
		return nil, fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}

// Default page sizes for the read commands. The limit and offset are
// passed to the store's page reads, so what is read from the database is
// the page, not every row: a crafted or huge database cannot exhaust
// memory by having many rows, and no single text column read is longer
// than agentrt.MaxPageText. Totals for "N more" come from COUNT queries.
const (
	defaultRunsLimit   = 100
	defaultStepsLimit  = 500
	defaultEventsLimit = 500
)

// readPage is the page size -limit 0 reads in. Each read stays bounded;
// what accumulates is what the operator asked to see.
const readPage = 500

// readAll reads one page of limit items after offset, or, for limit <= 0
// (the documented opt-out), every item from offset on in pages of
// readPage.
func readAll[T any](limit, offset int, read func(limit, offset int) ([]T, int, error)) ([]T, int, error) {
	if limit > 0 {
		return read(limit, offset)
	}
	var out []T
	for {
		page, total, err := read(readPage, offset+len(out))
		if err != nil {
			return nil, 0, err
		}
		out = append(out, page...)
		if len(page) < readPage {
			return out, total, nil
		}
	}
}

func parseRuns(args []string) (*command, error) {
	fs := newFlagSet("runs")
	limit := fs.Int("limit", defaultRunsLimit, "show at most this many runs (0: unlimited)")
	offset := fs.Int("offset", 0, "skip this many runs first")
	if err := fs.Parse(args); err != nil {
		return nil, parseErr(err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: runs: unexpected argument %q", errUsage, fs.Arg(0))
	}
	return &command{readOnly: true, exec: func(ctx context.Context, e *env) error { return cmdRuns(ctx, e, *limit, *offset) }}, nil
}

func parseShow(args []string) (*command, error) {
	runID, rest, err := positional(args, "show: expected a run id")
	if err != nil {
		return nil, err
	}
	fs := newFlagSet("show")
	limit := fs.Int("limit", defaultStepsLimit, "show at most this many steps and pending approvals (0: unlimited)")
	offset := fs.Int("offset", 0, "skip this many steps and pending approvals first")
	if err := fs.Parse(rest); err != nil {
		return nil, parseErr(err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: show: unexpected argument %q", errUsage, fs.Arg(0))
	}
	return &command{readOnly: true, exec: func(ctx context.Context, e *env) error { return cmdShow(ctx, e, runID, *limit, *offset) }}, nil
}

func parseEvents(args []string) (*command, error) {
	runID, rest, err := positional(args, "events: expected a run id")
	if err != nil {
		return nil, err
	}
	fs := newFlagSet("events")
	limit := fs.Int("limit", defaultEventsLimit, "show at most this many events (0: unlimited)")
	offset := fs.Int("offset", 0, "skip this many events first")
	if err := fs.Parse(rest); err != nil {
		return nil, parseErr(err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: events: unexpected argument %q", errUsage, fs.Arg(0))
	}
	return &command{readOnly: true, exec: func(ctx context.Context, e *env) error { return cmdEvents(ctx, e, runID, *limit, *offset) }}, nil
}

func parseDecide(args []string, approve bool) (*command, error) {
	verb := "reject"
	if approve {
		verb = "approve"
	}
	fs := newFlagSet(verb)
	approvalID := fs.String("approval", "", "approval id (default: the run's only pending approval)")
	by := fs.String("by", "", "who decided (default: $USER)")
	note := fs.String("note", "", "note recorded with the decision")
	yes := fs.Bool("yes", false, "confirm without prompting")
	positional, flagArgs := reorder(fs, args)
	if err := fs.Parse(flagArgs); err != nil {
		return nil, parseErr(err)
	}
	if len(positional) == 0 {
		return nil, fmt.Errorf("%w: %s: expected a run id", errUsage, verb)
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("%w: %s: unexpected argument %q", errUsage, verb, positional[1])
	}
	runID := positional[0]
	whoStr := who(*by)
	if whoStr == "" {
		return nil, fmt.Errorf("%w: %s: -by is empty and $USER is unset; pass -by", errUsage, verb)
	}
	return &command{readOnly: false, exec: func(ctx context.Context, e *env) error {
		return cmdDecide(ctx, e, runID, *approvalID, whoStr, *note, *yes, approve)
	}}, nil
}

func parseCancel(args []string) (*command, error) {
	fs := newFlagSet("cancel")
	by := fs.String("by", "", "who cancelled (default: $USER)")
	note := fs.String("note", "", "note recorded with the cancellation")
	positional, flagArgs := reorder(fs, args)
	if err := fs.Parse(flagArgs); err != nil {
		return nil, parseErr(err)
	}
	if len(positional) == 0 {
		return nil, fmt.Errorf("%w: cancel: expected a run id", errUsage)
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("%w: cancel: unexpected argument %q", errUsage, positional[1])
	}
	runID := positional[0]
	return &command{readOnly: false, exec: func(ctx context.Context, e *env) error {
		return cmdCancel(ctx, e, runID, who(*by), *note)
	}}, nil
}

// keptHead is a head an operator kept elsewhere, as verify -head takes
// it: a seq and the hash its event had.
type keptHead struct {
	seq  int64
	hash string
}

func parseVerify(args []string) (*command, error) {
	fs := newFlagSet("verify")
	from := fs.Int64("from", 0, "first event to check, by seq")
	to := fs.Int64("to", 0, "last event to check, by seq")
	headArg := fs.String("head", "", "a head kept elsewhere, SEQ:HASH, that the chain must reach and match")
	if err := fs.Parse(args); err != nil {
		return nil, parseErr(err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: verify: unexpected argument %q", errUsage, fs.Arg(0))
	}
	if *from < 0 || *to < 0 {
		return nil, fmt.Errorf("%w: verify: -from and -to are seq numbers, not negative", errUsage)
	}
	if *from > 0 && *to > 0 && *to < *from {
		return nil, fmt.Errorf("%w: verify: -to %d is before -from %d", errUsage, *to, *from)
	}
	var kept *keptHead
	if *headArg != "" {
		seqText, hash, ok := strings.Cut(*headArg, ":")
		seq, err := strconv.ParseInt(seqText, 10, 64)
		if !ok || err != nil || seq < 1 || hash == "" {
			return nil, fmt.Errorf("%w: verify: -head is SEQ:HASH, a positive seq and the hash kept for it", errUsage)
		}
		if *to > 0 && seq > *to {
			return nil, fmt.Errorf("%w: verify: -head seq %d is past -to %d", errUsage, seq, *to)
		}
		if *from > seq {
			return nil, fmt.Errorf("%w: verify: -head seq %d is before -from %d, so the walk would not check it", errUsage, seq, *from)
		}
		kept = &keptHead{seq, hash}
	}
	return &command{readOnly: true, exec: func(ctx context.Context, e *env) error { return cmdVerify(ctx, e, *from, *to, kept) }}, nil
}

// cmdRuns lists runs, newest first, reading only the page it prints.
func cmdRuns(ctx context.Context, e *env, limit, offset int) error {
	offset = max(offset, 0)
	rows, total, err := readAll(limit, offset, func(limit, offset int) ([]view.RunSummary, int, error) {
		return view.RunsPage(ctx, e.store, limit, offset)
	})
	if err != nil {
		return err
	}
	truncated := offset+len(rows) < total
	if e.json {
		return printJSON(e.out, struct {
			Runs      []view.RunSummary `json:"runs"`
			Total     int               `json:"total"`
			Limit     int               `json:"limit"`
			Offset    int               `json:"offset"`
			Truncated bool              `json:"truncated,omitempty"`
		}{Runs: rows, Total: total, Limit: limit, Offset: offset, Truncated: truncated})
	}
	if total == 0 {
		fmt.Fprintln(e.out, "no runs")
		return nil
	}
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATUS\tREASON\tSTEPS\tCREATED\tAPPROVAL\tGOAL")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			trace.Sanitize(r.ID), trace.Sanitize(string(r.Status)), dash(trace.Sanitize(string(r.Reason))),
			r.Steps, stamp(r.CreatedAt), dash(trace.Sanitize(r.PendingApprovalID)), clip(trace.Sanitize(r.Goal), 60))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if truncated {
		fmt.Fprintf(e.out, "... %d more run(s); rerun with -offset %d to see them (showing %d of %d)\n",
			total-offset-len(rows), offset+len(rows), len(rows), total)
	}
	return nil
}

// maxTextField bounds a single text field printed in "show" outside the
// per-approval, per-field bound printWaiting already applies: the goal and
// the reason detail are free text a model or a policy chooses, and an
// unbounded database read of either would let a crafted or merely huge row
// flood the terminal.
const maxTextField = 4000

func cmdShow(ctx context.Context, e *env, runID string, limit, offset int) error {
	offset = max(offset, 0)
	detail, err := readDetail(ctx, e.store, runID, limit, offset)
	if err != nil {
		return err
	}
	summary, steps, pending := detail.Run, detail.Steps, detail.Pending
	stepsTruncated := offset+len(steps) < detail.StepsTotal
	// A run waiting on many pending approvals is paged the same way, sharing
	// -limit/-offset with steps: there is no separate flag for it, because a
	// run legitimately waiting on more than a handful of approvals at once
	// is not a case this command is tuned for.
	pendingTruncated := offset+len(pending) < detail.PendingTotal
	if e.json {
		var waiting *agentrt.Approval
		if detail.Waiting != nil {
			b := view.BoundApproval(*detail.Waiting)
			waiting = &b
		}
		boundedApprovals := make([]agentrt.Approval, len(detail.Approvals))
		for i, a := range detail.Approvals {
			boundedApprovals[i] = view.BoundApproval(a)
		}
		return printJSON(e.out, struct {
			Run            view.RunSummary    `json:"run"`
			Steps          []view.StepSummary `json:"steps"`
			StepsTotal     int                `json:"steps_total"`
			StepsTruncated bool               `json:"steps_truncated,omitempty"`
			Approvals      []agentrt.Approval `json:"approvals,omitempty"`
			ApprovalsTotal int                `json:"approvals_total"`
			Waiting        *agentrt.Approval  `json:"waiting_approval,omitempty"`
			Limit          int                `json:"limit"`
			Offset         int                `json:"offset"`
		}{Run: summary, Steps: steps, StepsTotal: detail.StepsTotal, StepsTruncated: stepsTruncated,
			Approvals: boundedApprovals, ApprovalsTotal: detail.ApprovalsTotal, Waiting: waiting, Limit: limit, Offset: offset})
	}
	fmt.Fprintf(e.out, "run     %s\n", trace.Sanitize(summary.ID))
	fmt.Fprintf(e.out, "goal    %s\n", capField(trace.Sanitize(summary.Goal)))
	fmt.Fprintf(e.out, "status  %s", trace.Sanitize(string(summary.Status)))
	if summary.Reason != "" {
		fmt.Fprintf(e.out, " (%s)", trace.Sanitize(string(summary.Reason)))
	}
	if summary.ReasonDetail != "" {
		fmt.Fprintf(e.out, ": %s", capField(trace.Sanitize(summary.ReasonDetail)))
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
	// Every pending approval up to the limit is shown, not just one: a run
	// can be waiting on more than one, and hiding the rest would leave an
	// operator deciding blind on whichever they happen to find.
	for _, a := range pending {
		printWaiting(e, a)
	}
	if pendingTruncated {
		fmt.Fprintf(e.out, "\n... %d more pending approval(s) not shown; rerun with -offset %d to see them\n", detail.PendingTotal-len(pending)-offset, offset+len(pending))
	}
	if len(steps) == 0 {
		return nil
	}
	fmt.Fprintln(e.out)
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tTOOL\tDECISION\tSTATUS\tPOLICY\tOBSERVATION\tSUMMARY")
	for _, s := range steps {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Index, dash(trace.Sanitize(s.Tool)), dash(trace.Sanitize(string(s.Decision))), trace.Sanitize(string(s.Status)),
			dash(trace.Sanitize(string(s.Policy))), dash(trace.Sanitize(string(s.Observation))), clip(trace.Sanitize(s.Summary), 60))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if stepsTruncated {
		fmt.Fprintf(e.out, "... %d more step(s); rerun with -offset %d to see them (showing %d of %d)\n",
			detail.StepsTotal-offset-len(steps), offset+len(steps), len(steps), detail.StepsTotal)
	}
	return nil
}

// readDetail reads one page of a run's detail, or, for limit <= 0, every
// step and approval from offset on, a bounded page at a time.
func readDetail(ctx context.Context, store *agentrt.Store, runID string, limit, offset int) (view.RunDetailPage, error) {
	if limit > 0 {
		return view.DetailPage(ctx, store, runID, limit, offset)
	}
	var all view.RunDetailPage
	for at := offset; ; at += readPage {
		p, err := view.DetailPage(ctx, store, runID, readPage, at)
		if err != nil {
			return view.RunDetailPage{}, err
		}
		if at == offset {
			all.Run, all.Waiting = p.Run, p.Waiting
		}
		all.StepsTotal, all.PendingTotal, all.ApprovalsTotal = p.StepsTotal, p.PendingTotal, p.ApprovalsTotal
		all.Steps = append(all.Steps, p.Steps...)
		all.Pending = append(all.Pending, p.Pending...)
		all.Approvals = append(all.Approvals, p.Approvals...)
		if len(p.Steps) < readPage && len(p.Pending) < readPage && len(p.Approvals) < readPage {
			return all, nil
		}
	}
}

// capField bounds a single free-text field printed in text mode to
// maxTextField bytes, on a UTF-8 boundary, with a visible count of what was
// cut.
func capField(s string) string {
	if len(s) <= maxTextField {
		return s
	}
	cut := headBytes(s, maxTextField)
	return fmt.Sprintf("%s… [%d more bytes]", cut, len(s)-len(cut))
}

// headBytes cuts s to at most n bytes, moving inward to the nearest UTF-8
// character boundary so the cut never splits a multi-byte rune.
func headBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// maxWaitingFieldBytes bounds a single string value printed per field in
// printWaiting: view.MaxFieldBytes is the same limit BoundApproval applies
// to the JSON this command emits, so text and JSON never disagree about how
// much of one model- or server-chosen value an operator, or a consumer
// parsing JSON, is shown.
const maxWaitingFieldBytes = view.MaxFieldBytes

// printWaiting puts the decision in front of the operator: what kind of
// approval is asked for and exactly what they would be granting. Every
// field is model- or server-supplied, is sanitized before it reaches the
// terminal, and is printed one field per line with its own byte count, so a
// long or padded value cannot push another field, or the closing line,
// off screen. A single string value over maxWaitingFieldBytes is shown by
// its head and tail rather than in full.
func printWaiting(e *env, a agentrt.Approval) {
	fmt.Fprintf(e.out, "\nAPPROVAL WAITING  %s\n", trace.Sanitize(a.ID))
	fmt.Fprintf(e.out, "kind    %s\n", trace.Sanitize(a.Kind))
	fmt.Fprintf(e.out, "tool    %s\n", trace.Sanitize(a.Request.Spec.Name))
	if !a.ExpiresAt.IsZero() {
		fmt.Fprintf(e.out, "expires %s\n", stamp(a.ExpiresAt))
	}
	if a.DecodeError != "" {
		// The row was too large to read whole, or damaged; approve and
		// reject read it again in full before anything is decided.
		fmt.Fprintf(e.out, "unread  %s\n", capField(trace.Sanitize(a.DecodeError)))
	}
	fmt.Fprintln(e.out, "arguments (the tool call the model made; one field per line, byte count is the field's own size)")
	printFields(e.out, a.Request.Args)
	fmt.Fprintln(e.out, "presentation (supplied by the consumer's policy; it may contain model-chosen text)")
	printFields(e.out, a.Presentation)
	fmt.Fprintf(e.out, "decide with: agentrt -db %q approve %s -approval %s\n", e.path, a.RunID, a.ID)
	// Everything above this line other than "APPROVAL WAITING", "kind",
	// "tool", "expires", the two section headers, and "decide with" may
	// contain model- or server-chosen text. This line does not, and it is
	// the last thing printed for this approval, so it is the last thing an
	// operator reads before deciding, not whatever the presentation ended
	// with.
	fmt.Fprintf(e.out, "----- end of approval %s for tool %s -----\n", trace.Sanitize(a.ID), trace.Sanitize(a.Request.Spec.Name))
}

// printFields prints a JSON object one field per line: the key, the byte
// size of its value exactly as stored (before any truncation), and the
// value itself, pretty-printed and sanitized line by line. A value that is
// not a JSON object (or is empty or invalid) is printed as a single
// "value" field instead, so this still degrades safely on a store row this
// command did not expect.
func printFields(w io.Writer, raw json.RawMessage) {
	if len(bytes.TrimSpace(raw)) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		printField(w, "value", clipBytes(string(raw), maxWaitingFieldBytes), len(raw))
		return
	}
	obj, ok := v.(map[string]any)
	if !ok {
		printOneField(w, "value", v)
		return
	}
	if len(obj) == 0 {
		fmt.Fprintln(w, "  (no fields)")
		return
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		printOneField(w, k, obj[k])
	}
}

// printOneField prints one field of printFields: its key, the byte size of
// v as it was actually stored, and v itself bounded to
// maxWaitingFieldBytes per string and pretty-printed.
func printOneField(w io.Writer, key string, v any) {
	orig, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(w, "  %s (unprintable: %v)\n", trace.Sanitize(key), err)
		return
	}
	// No prefix here: json.MarshalIndent only puts its prefix in front of
	// every line but the first, so a non-empty prefix left the value's own
	// opening brace two columns left of its matching closing brace once
	// printField's per-line indent was added below. An empty prefix nests
	// consistently on its own, so a uniform prefix added once, to every
	// line alike, stays uniform.
	bounded, err := json.MarshalIndent(view.BoundValue(v), "", "  ")
	if err != nil {
		printField(w, key, trace.Sanitize(string(orig)), len(orig))
		return
	}
	printField(w, key, string(bounded), len(orig))
}

func printField(w io.Writer, key, value string, origBytes int) {
	fmt.Fprintf(w, "  %s (%d bytes)\n", trace.Sanitize(key), origBytes)
	for _, line := range strings.Split(value, "\n") {
		fmt.Fprintf(w, "    %s\n", trace.Sanitize(line))
	}
}

// clipBytes shortens s to at most n bytes on a UTF-8 character boundary,
// marking the cut; unlike clip it does not also replace newlines, because
// its caller (an unparsed field's raw bytes) is sanitized afterward.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// maxEventPayloadTextBytes bounds a single event's payload as printed in
// text mode. -json keeps full fidelity, unbounded per field: it is meant to
// feed a collector, trace.JSONL's own doc comment says a consumer parsing
// JSON is not a terminal, and truncating raw bytes there risks corrupting
// the audit record instead of just its display. The aligned text writer
// prints straight to a terminal, so an oversized or padded payload is
// capped there instead.
const maxEventPayloadTextBytes = 8000

// cmdEvents prints a run's audit log, reading only the page it prints.
func cmdEvents(ctx context.Context, e *env, runID string, limit, offset int) error {
	offset = max(offset, 0)
	if _, err := e.store.GetRunCapped(ctx, runID); err != nil {
		return err
	}
	var observer agentrt.Observer
	var errFn func() error
	if e.json {
		observer, errFn = trace.JSONLWithErr(e.out)
	} else {
		w, fn := trace.WriterWithErr(e.out)
		observer = func(ev agentrt.Event) { w(capPayload(ev, maxEventPayloadTextBytes)) }
		errFn = fn
	}
	// Each page is printed as it is read, so -limit 0 streams the log
	// rather than holding it.
	shown, total := 0, 0
	for {
		n := limit
		if n <= 0 {
			n = readPage
		}
		page, t, err := e.store.ListEventsPage(ctx, runID, n, offset+shown)
		if err != nil {
			return err
		}
		total = t
		for _, ev := range page {
			observer(ev)
		}
		shown += len(page)
		if limit > 0 || len(page) < n {
			break
		}
	}
	if err := errFn(); err != nil {
		return fmt.Errorf("events: writing output: %w", err)
	}
	if offset+shown >= total {
		return nil
	}
	if e.json {
		// A distinct object, not a per-event record: "meta" marks it as not
		// an event, so a reader that expects one JSON object per line does
		// not mistake it for one. Compact, one line, matching trace.JSONL's
		// own format, not printJSON's pretty-printed indent: this line has
		// to stay one JSON Lines record like every other line here.
		enc := json.NewEncoder(e.out)
		return enc.Encode(struct {
			Meta      bool `json:"meta"`
			Truncated bool `json:"truncated"`
			Total     int  `json:"total"`
			Returned  int  `json:"returned"`
			Offset    int  `json:"offset"`
		}{true, true, total, shown, offset})
	}
	fmt.Fprintf(e.out, "... %d more event(s); rerun with -offset %d to see them (showing %d of %d)\n",
		total-offset-shown, offset+shown, shown, total)
	return nil
}

// errChainBroken is verify's error for an event that does not chain,
// after the break has been printed; errNotReached for a -to or -head the
// chain does not reach, and errHeadMismatch for a kept head whose event
// now has another hash.
var (
	errChainBroken  = errors.New("the event chain is broken")
	errNotReached   = errors.New("the event chain is shorter than asked")
	errHeadMismatch = errors.New("the event chain does not match the head kept")
)

// cmdVerify checks the events' hash chain from -from to -to and prints the
// head, what it found, and the hash of the last event checked. The head is
// read first, so events committed while it runs are not checked. A -to
// past the last event, and a kept head (-head) at a seq the walk did not
// reach or whose event's hash is not the one kept, fail it: a chain cut at
// its end verifies on its own, and only those show it is short.
func cmdVerify(ctx context.Context, e *env, from, to int64, kept *keptHead) error {
	seq, hash, err := e.store.ChainHead(ctx)
	if err != nil {
		return err
	}
	asked := to
	if to == 0 {
		to = seq
	}
	rep := agentrt.VerifyReport{}
	if to > 0 {
		if rep, err = e.store.VerifyEvents(ctx, from, to); err != nil {
			return err
		}
	}
	var problem error
	reached := rep.Break == nil && rep.Checked > 0 && rep.To >= asked
	if rep.Break == nil && asked > 0 && !reached {
		problem = fmt.Errorf("%w: -to %d was not reached; the last event checked is seq %d", errNotReached, asked, rep.To)
	}
	// The kept head's event, read when the walk checked it.
	var found string
	matches := false
	if kept != nil && rep.Break == nil && problem == nil {
		switch {
		case rep.Checked == 0 || kept.seq > rep.To:
			problem = fmt.Errorf("%w: the kept head, seq %d, was not reached; the last event checked is seq %d", errNotReached, kept.seq, rep.To)
		case kept.seq == rep.To:
			found = rep.Hash
		default:
			if found, err = e.store.EventHash(ctx, kept.seq); err != nil {
				return err
			}
		}
		if problem == nil {
			if matches = found == kept.hash; !matches {
				problem = fmt.Errorf("%w: seq %d has hash %s, not the %s kept", errHeadMismatch, kept.seq, trace.Sanitize(found), trace.Sanitize(kept.hash))
			}
		}
	}
	if e.json {
		type head struct {
			Seq  int64  `json:"seq"`
			Hash string `json:"hash"`
		}
		type chainBreak struct {
			Seq      int64  `json:"seq"`
			Expected string `json:"expected"`
			Found    string `json:"found"`
			Detail   string `json:"detail"`
		}
		type requested struct {
			To      int64 `json:"to"`
			Reached bool  `json:"reached"`
		}
		type keptOut struct {
			Seq     int64  `json:"seq"`
			Hash    string `json:"hash"`
			Found   string `json:"found"`
			Matches bool   `json:"matches"`
		}
		out := struct {
			Head      head        `json:"head"`
			From      int64       `json:"from"`
			To        int64       `json:"to"`
			Hash      string      `json:"hash"`
			Checked   int64       `json:"checked"`
			Intact    bool        `json:"intact"`
			Break     *chainBreak `json:"break,omitempty"`
			Requested *requested  `json:"requested,omitempty"`
			KeptHead  *keptOut    `json:"kept_head,omitempty"`
		}{Head: head{seq, hash}, From: rep.From, To: rep.To, Hash: rep.Hash, Checked: rep.Checked, Intact: rep.Break == nil}
		if b := rep.Break; b != nil {
			out.Break = &chainBreak{b.Seq, b.Expected, b.Found, b.Detail}
		}
		if asked > 0 {
			out.Requested = &requested{asked, reached}
		}
		if kept != nil {
			out.KeptHead = &keptOut{kept.seq, kept.hash, found, matches}
		}
		if err := printJSON(e.out, out); err != nil {
			return err
		}
	} else {
		if seq == 0 {
			fmt.Fprintln(e.out, "head: no events")
		} else {
			fmt.Fprintf(e.out, "head: seq %d %s\n", seq, trace.Sanitize(hash))
		}
		if b := rep.Break; b != nil {
			fmt.Fprintf(e.out, "broken at seq %d: %s\n  expected %s\n  found    %s\n", b.Seq, b.Detail, b.Expected, dash(trace.Sanitize(b.Found)))
		} else if rep.Checked == 0 {
			fmt.Fprintln(e.out, "intact: no events in the range")
		} else {
			fmt.Fprintf(e.out, "intact: %d event(s), seq %d to %d\nlast checked: seq %d %s\n", rep.Checked, rep.From, rep.To, rep.To, trace.Sanitize(rep.Hash))
		}
		if matches {
			fmt.Fprintf(e.out, "kept head: seq %d matches\n", kept.seq)
		}
	}
	if rep.Break != nil {
		return fmt.Errorf("%w at seq %d", errChainBroken, rep.Break.Seq)
	}
	return problem
}

// capPayload returns ev with its payload bounded to n bytes for text-mode
// printing. trace.Writer treats the payload as an opaque string, not
// parsed JSON, so truncating its raw bytes here does not need to leave
// valid JSON behind the way -json's untouched payload must.
func capPayload(ev agentrt.Event, n int) agentrt.Event {
	if len(ev.Payload) <= n {
		return ev
	}
	cut := headBytes(string(ev.Payload), n)
	ev.Payload = json.RawMessage(fmt.Sprintf("%s...[%d more bytes]", cut, len(ev.Payload)-len(cut)))
	return ev
}

// cmdDecide handles approve and reject. It shows the approval an operator
// would be granting or refusing before it decides anything, and it decides
// only when the operator named the approval explicitly, passed -yes, or
// confirmed at an interactive prompt.
func cmdDecide(ctx context.Context, e *env, runID, approvalID, by, note string, yes, approve bool) error {
	verb := "reject"
	if approve {
		verb = "approve"
	}
	a, err := view.PendingApproval(ctx, e.store, runID, approvalID)
	if err != nil {
		return err
	}
	if !e.json {
		printWaiting(e, a)
	}
	refuse := func() error {
		return fmt.Errorf("%w: %s: refusing without -approval, -yes, or an interactive terminal to confirm at; pass -approval %s or -yes", errUsage, verb, a.ID)
	}
	switch {
	case approvalID != "":
		// Naming the approval by id is itself the deliberate act.
	case yes:
		// -yes is explicit consent.
	case e.interactive && !e.json:
		// A prompt has nowhere to go that keeps -json's stdout pure JSON.
		ok, err := confirm(e, fmt.Sprintf("%s approval %s of run %s? [y/N] ", verb, a.ID, runID))
		if errors.Is(err, errNoAnswer) {
			// Reaching end of input before any answer is the same refusal
			// as having no terminal to ask at, not a typed decline.
			return refuse()
		}
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s: declined at the prompt", verb)
		}
	default:
		return refuse()
	}
	obs := trace.Writer(e.errw)
	// ApproveShown/RejectShown bind the decision to a.Hash, the hash of
	// exactly what was fetched and (in text mode) printed above. Anything
	// that changed the approval's stored kind, capability, presentation, or
	// request between that read and this write -- a concurrent writer, a
	// re-pause reusing the id, a corrupted row -- moves its hash and the
	// decision is refused rather than applied to whatever it has become.
	if approve {
		err = view.ApproveShown(ctx, e.store, obs, runID, a.ID, a.Hash, by, note)
	} else {
		err = view.RejectShown(ctx, e.store, obs, runID, a.ID, a.Hash, by, note)
	}
	if err != nil {
		if errors.Is(err, agentrt.ErrApprovalChanged) {
			return fmt.Errorf("%s: approval %s changed after it was shown; run the command again and read it before deciding", verb, a.ID)
		}
		return err
	}
	decided, err := e.store.GetApproval(ctx, runID, a.ID)
	if err != nil {
		return err
	}
	return e.report(ctx, runID, &decided, approve)
}

// confirm prints prompt and reads one line from e.stdin, treating "y" or
// "yes" (case-insensitively) as consent and any other typed line as a
// decline. Reaching the end of input before anything was typed returns
// errNoAnswer rather than a decline: nothing was answered.
func confirm(e *env, prompt string) (bool, error) {
	fmt.Fprint(e.out, prompt)
	line, err := bufio.NewReader(e.stdin).ReadString('\n')
	if line == "" && err != nil {
		return false, errNoAnswer
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}

func cmdCancel(ctx context.Context, e *env, runID, by, note string) error {
	if err := view.Cancel(ctx, e.store, trace.Writer(e.errw), runID, by, note); err != nil {
		return err
	}
	return e.report(ctx, runID, nil, false)
}

// report prints what the run looks like after a decision, and what the
// operator has to do next: approving does not resume the run.
func (e *env) report(ctx context.Context, runID string, a *agentrt.Approval, approved bool) error {
	detail, err := view.DetailPage(ctx, e.store, runID, 1, 0)
	if err != nil {
		return err
	}
	summary := detail.Run
	if e.json {
		bounded := a
		if a != nil {
			b := view.BoundApproval(*a)
			bounded = &b
		}
		// bounded.Hash is untouched by BoundApproval (only Capability,
		// Presentation, and Request.Args are): -json for approve/reject
		// always carries the hash that was actually decided.
		return printJSON(e.out, struct {
			Run      view.RunSummary   `json:"run"`
			Approval *agentrt.Approval `json:"approval,omitempty"`
		}{Run: summary, Approval: bounded})
	}
	if a != nil {
		fmt.Fprintf(e.out, "approval %s: %s by %s\n", trace.Sanitize(a.ID), a.Status, trace.Sanitize(a.DecidedBy))
	}
	fmt.Fprintf(e.out, "run %s: %s", trace.Sanitize(summary.ID), summary.Status)
	if summary.Reason != "" {
		fmt.Fprintf(e.out, " (%s)", trace.Sanitize(string(summary.Reason)))
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

// stampLayout prints date, time, and zone: a time with only a clock reading
// is ambiguous across midnight, and one with no zone is ambiguous between
// the operator's local time and whatever produced the record.
const stampLayout = "2006-01-02 15:04:05 -0700"

func stamp(t time.Time) string { return t.Local().Format(stampLayout) }

func dollars(m agentrt.Micros) string { return fmt.Sprintf("$%.4f", float64(m)/1e6) }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// clip shortens s to at most n bytes on a full UTF-8 character boundary and
// marks the cut; a caller sanitizes s first so the cut never falls inside a
// multi-byte escape this command just wrote in.
func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// isTerminal reports whether f is a character device that is not the null
// device, which is what a terminal is and what a pipe, a file, or
// /dev/null is not. /dev/null is itself a character device, so it would
// otherwise pass the character-device check and make "approve <run>
// </dev/null" prompt, read nothing, and decline rather than refuse. It is
// told apart from a real terminal by identity (os.SameFile against
// os.DevNull), not by path, since a terminal could be reached through a
// different name for the same device and /dev/null through one too. The
// standard library is enough for deciding whether anyone is there to
// confirm.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, null) {
		return false
	}
	return true
}
