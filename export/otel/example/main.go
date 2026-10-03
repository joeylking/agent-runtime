// Command example exports the runs of an agent-runtime database as
// OpenTelemetry traces, from outside the process that owns it: it opens
// the database read-only, follows the events table, and sends spans to the
// collector the OTEL_* environment names. The consumer needs no code.
//
//	go run ./example -db runs.db [-run id] [-cursor path] [-once] [-v]
//
// With a stock collector on localhost:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 go run ./example -db runs.db
//
// -cursor persists where the follower is, so the next start resumes after
// the last event exported; -once exports what is committed and exits;
// -v prints each event's type to stderr. Ctrl-C stops it after flushing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/export"
	"github.com/joeylking/agent-runtime/export/otel"
)

func main() {
	db := flag.String("db", os.Getenv("AGENTRT_DB"), "the SQLite file (default: $AGENTRT_DB)")
	runID := flag.String("run", "", "export one run only (default: every run)")
	cursor := flag.String("cursor", "", "file holding the follower's position (default: start from the first event, remember nothing)")
	once := flag.Bool("once", false, "export what is committed and exit")
	interval := flag.Duration("interval", export.DefaultInterval, "wait between polls when caught up")
	verbose := flag.Bool("v", false, "print each event's type to stderr")
	flag.Parse()
	if *db == "" {
		fmt.Fprintln(os.Stderr, "error: pass -db or set AGENTRT_DB")
		os.Exit(2)
	}
	if err := run(*db, *runID, *cursor, *once, *interval, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(db, runID, cursorPath string, once bool, interval time.Duration, verbose bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := agentrt.OpenExisting(db, true)
	if err != nil {
		return err
	}
	defer store.Close()

	tp, err := otel.Provider(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Flush with a deadline of its own: ctx may already be cancelled.
		fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tp.Shutdown(fctx); err != nil {
			fmt.Fprintln(os.Stderr, "flush:", err)
		}
	}()

	exporter := &otel.Exporter{TracerProvider: tp, Store: store}
	sink := exporter.Handle
	if verbose {
		sink = func(e agentrt.Event) error {
			fmt.Fprintf(os.Stderr, "%s  %-22s %s\n", e.At.Local().Format("15:04:05.000"), e.Type, e.RunID)
			return exporter.Handle(e)
		}
	}
	follower := &export.Follower{Store: store, RunID: runID, Interval: interval, Once: once}
	if cursorPath != "" {
		follower.Cursor = export.FileCursor{Path: cursorPath}
	}
	err = follower.Follow(ctx, sink)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
