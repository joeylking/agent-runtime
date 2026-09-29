// Command agentrt is the operator surface over a runs database: list runs,
// show one, print its event log, and approve, reject, or cancel. It reads
// and decides; it never executes a step, and it never creates or migrates
// the database it is pointed at.
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

global flags:
  -db path              SQLite database (default: $AGENTRT_DB)
  -json                 emit JSON instead of aligned text

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

agentrt never creates a missing database and never migrates one on a
different schema version than this build: that is the consumer's job.

There is no resume command. Resuming a run executes the approved request and
continues the loop, which needs the consumer's agent, its tools, and its
policy, so resume belongs in the consumer's own command:

  run, err := driver.Resume(ctx, runID)

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
		if errors.Is(err, agentrt.ErrSchemaVersion) {
			fmt.Fprint(errw, schemaMessage(err, path))
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
	default:
		return nil, fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}

func parseRuns(args []string) (*command, error) {
	fs := newFlagSet("runs")
	if err := fs.Parse(args); err != nil {
		return nil, parseErr(err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: runs: unexpected argument %q", errUsage, fs.Arg(0))
	}
	return &command{readOnly: true, exec: func(ctx context.Context, e *env) error { return cmdRuns(ctx, e) }}, nil
}

func parseShow(args []string) (*command, error) {
	runID, rest, err := positional(args, "show: expected a run id")
	if err != nil {
		return nil, err
	}
	fs := newFlagSet("show")
	if err := fs.Parse(rest); err != nil {
		return nil, parseErr(err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: show: unexpected argument %q", errUsage, fs.Arg(0))
	}
	return &command{readOnly: true, exec: func(ctx context.Context, e *env) error { return cmdShow(ctx, e, runID) }}, nil
}

func parseEvents(args []string) (*command, error) {
	runID, rest, err := positional(args, "events: expected a run id")
	if err != nil {
		return nil, err
	}
	fs := newFlagSet("events")
	if err := fs.Parse(rest); err != nil {
		return nil, parseErr(err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: events: unexpected argument %q", errUsage, fs.Arg(0))
	}
	return &command{readOnly: true, exec: func(ctx context.Context, e *env) error { return cmdEvents(ctx, e, runID) }}, nil
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

func cmdRuns(ctx context.Context, e *env) error {
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			trace.Sanitize(r.ID), trace.Sanitize(string(r.Status)), dash(trace.Sanitize(string(r.Reason))),
			r.Steps, stamp(r.CreatedAt), dash(trace.Sanitize(r.PendingApprovalID)), clip(trace.Sanitize(r.Goal), 60))
	}
	return tw.Flush()
}

func cmdShow(ctx context.Context, e *env, runID string) error {
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
	pending := pendingOnly(approvals)
	if e.json {
		var waiting *agentrt.Approval
		if len(pending) == 1 {
			waiting = &pending[0]
		}
		return printJSON(e.out, struct {
			Run       view.RunSummary    `json:"run"`
			Steps     []view.StepSummary `json:"steps"`
			Approvals []agentrt.Approval `json:"approvals,omitempty"`
			Waiting   *agentrt.Approval  `json:"waiting_approval,omitempty"`
		}{Run: summary, Steps: steps, Approvals: approvals, Waiting: waiting})
	}
	fmt.Fprintf(e.out, "run     %s\n", trace.Sanitize(summary.ID))
	fmt.Fprintf(e.out, "goal    %s\n", trace.Sanitize(summary.Goal))
	fmt.Fprintf(e.out, "status  %s", trace.Sanitize(string(summary.Status)))
	if summary.Reason != "" {
		fmt.Fprintf(e.out, " (%s)", trace.Sanitize(string(summary.Reason)))
	}
	if summary.ReasonDetail != "" {
		fmt.Fprintf(e.out, ": %s", trace.Sanitize(summary.ReasonDetail))
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
	// Every pending approval is shown, not just one: a run can be waiting on
	// more than one, and hiding the rest would leave an operator deciding
	// blind on whichever they happen to find.
	for _, a := range pending {
		printWaiting(e, a)
	}
	if len(steps) == 0 {
		return nil
	}
	fmt.Fprintln(e.out)
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tTOOL\tDECISION\tSTATUS\tPOLICY\tOBSERVATION\tSUMMARY")
	for _, s := range steps {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Index, dash(trace.Sanitize(s.Tool)), dash(string(s.Decision)), s.Status,
			dash(string(s.Policy)), dash(string(s.Observation)), clip(trace.Sanitize(s.Summary), 60))
	}
	return tw.Flush()
}

// pendingOnly filters to approvals still awaiting a decision.
func pendingOnly(approvals []agentrt.Approval) []agentrt.Approval {
	out := make([]agentrt.Approval, 0, len(approvals))
	for _, a := range approvals {
		if a.Status == agentrt.ApprovalPending {
			out = append(out, a)
		}
	}
	return out
}

// printWaiting puts the decision in front of the operator: what kind of
// approval is asked for and exactly what they would be granting. Every
// field is model- or server-supplied and is sanitized before it reaches the
// terminal.
func printWaiting(e *env, a agentrt.Approval) {
	fmt.Fprintf(e.out, "\nAPPROVAL WAITING  %s\n", trace.Sanitize(a.ID))
	fmt.Fprintf(e.out, "kind    %s\n", trace.Sanitize(a.Kind))
	fmt.Fprintf(e.out, "tool    %s %s\n", trace.Sanitize(a.Request.Spec.Name), trace.Sanitize(string(a.Request.Args)))
	if !a.ExpiresAt.IsZero() {
		fmt.Fprintf(e.out, "expires %s\n", stamp(a.ExpiresAt))
	}
	fmt.Fprintln(e.out, "presentation")
	var buf bytes.Buffer
	if err := json.Indent(&buf, a.Presentation, "  ", "  "); err != nil {
		fmt.Fprintf(e.out, "  %s\n", trace.Sanitize(string(a.Presentation)))
	} else {
		fmt.Fprintf(e.out, "  %s\n", sanitizeBlock(buf.String()))
	}
	fmt.Fprintf(e.out, "decide with: agentrt -db %q approve %s -approval %s\n", e.path, a.RunID, a.ID)
}

// sanitizeBlock sanitizes a multi-line block (the pretty-printed
// presentation) line by line, so the newlines json.Indent inserted for
// structure survive while a control character within a line is still
// escaped. Those newlines are never attacker data: a raw newline inside a
// JSON string value is escaped by the encoder as the two characters \n, not
// as a byte 0x0A, so every 0x0A actually in the text is this command's own
// formatting.
func sanitizeBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = trace.Sanitize(l)
	}
	return strings.Join(lines, "\n")
}

func cmdEvents(ctx context.Context, e *env, runID string) error {
	if _, err := e.store.GetRun(ctx, runID); err != nil {
		return err
	}
	events, err := e.store.ListEvents(ctx, runID)
	if err != nil {
		return err
	}
	var observer agentrt.Observer
	var errFn func() error
	if e.json {
		observer, errFn = trace.JSONLWithErr(e.out)
	} else {
		observer, errFn = trace.WriterWithErr(e.out)
	}
	for _, ev := range events {
		observer(ev)
	}
	if err := errFn(); err != nil {
		return fmt.Errorf("events: writing output: %w", err)
	}
	return nil
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
	switch {
	case approvalID != "":
		// Naming the approval by id is itself the deliberate act.
	case yes:
		// -yes is explicit consent.
	case e.interactive && !e.json:
		// A prompt has nowhere to go that keeps -json's stdout pure JSON.
		ok, err := confirm(e, fmt.Sprintf("%s approval %s of run %s? [y/N] ", verb, a.ID, runID))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s: declined at the prompt", verb)
		}
	default:
		return fmt.Errorf("%w: %s: refusing without -approval, -yes, or an interactive terminal to confirm at; pass -approval %s or -yes", errUsage, verb, a.ID)
	}
	obs := trace.Writer(e.errw)
	if approve {
		err = view.Approve(ctx, e.store, obs, runID, a.ID, by, note)
	} else {
		err = view.Reject(ctx, e.store, obs, runID, a.ID, by, note)
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

// confirm prints prompt and reads one line from e.stdin, treating "y" or
// "yes" (case-insensitively) as consent and anything else, including no
// input at all, as a decline.
func confirm(e *env, prompt string) (bool, error) {
	fmt.Fprint(e.out, prompt)
	line, _ := bufio.NewReader(e.stdin).ReadString('\n')
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

// isTerminal reports whether f is a character device, which is what a
// terminal is and what a pipe or a file is not. The standard library is
// enough for deciding whether anyone is there to confirm.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
