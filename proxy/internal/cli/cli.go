// Package cli is agentrt-proxy's command line: serve, pin, and check. It
// is a package of its own so the command, its tests, and the
// demonstration run the same code in a child process.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/joeylking/agent-runtime/mcp"
	"github.com/joeylking/agent-runtime/proxy"
	"github.com/joeylking/agent-runtime/render"
	"github.com/joeylking/agent-runtime/trace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Usage is the help text. Its second half is the scope the proxy keeps to,
// stated as docs/proxy.md states it.
const Usage = `agentrt-proxy: a local MCP proxy that puts agent-runtime's gate in front of MCP servers.

usage:
  agentrt-proxy -config <file>         serve one MCP host over stdin and stdout
  agentrt-proxy pin -config <file>     pin each server's tools into its manifest
  agentrt-proxy check -config <file>   load everything and report what is offered

An MCP host starts it as its server, with {"command": "agentrt-proxy",
"args": ["-config", "<file>"]}. It connects to the servers the file names,
offers only the tools the operator classified and pinned, and routes every
call through the gate: recorded, checked against the tool's schema, held to
the policy, paused on an approval bound to the exact request when the policy
asks, executed by the gate, and recorded with its outcome. Approvals are
decided outside the host, with agentrt on the same database. stdout carries
only the protocol; everything else goes to stderr.

What it does not do:
  - It governs only the calls that go through it. If the host gives the model
    a shell, file access, or other tools of its own, the model can bypass the
    proxy, and can approve its own request with the operator command or by
    editing the database. It is for hosts without such tools, or with them
    disabled or behind the host's own prompts, or inside a sandbox whose only
    way out is the proxy.
  - It is for one person who is both the host's user and the operator. It
    does not separate them.
  - It never sees model calls, so it sets no token or cost limits, and it
    never sees the user's prompt.
  - Each call is a separate run, so there are no limits or policy over a
    conversation's history.
  - The duplicate rule refuses a deliberate identical repeat of a call that
    changes something, inside the repeat window, as well as a retry.
  - Results are capped and flattened.
  - The pin covers what a server presents, not what it does.
  - The servers run as you. None may reach the configuration, the manifests,
    or the database: one that can write them can rewrite the policy or forge
    an approval. The check of a server's arguments for them is a guard
    against a mistake, not a boundary.

What it keeps: an approval bound by hash to the exact request, durable across
restarts, with policy evaluated again when the request is collected; a call
that changes something whose outcome is unknown, cut off, timed out, or left
without an answer, is not run again as the identical request without an
operator;
the audit record is committed with each state change; tools are pinned and
classified by the operator; an identical call that changes something is not
executed twice within the repeat window.
`

// Run runs the command with args, the arguments after the program name,
// serving a host over stdin and stdout, and returns the exit code: 0, 1 for
// a failure, 2 for a usage error.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("agentrt-proxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := fs.String("config", "", "the configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, Usage)
			return 0
		}
		fmt.Fprintf(stderr, "agentrt-proxy: %v\n\n%s", err, Usage)
		return 2
	}
	if *config == "" || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentrt-proxy: -config <file> is required and nothing follows it\n\n%s", Usage)
		return 2
	}
	cfg, err := proxy.LoadConfig(*config)
	if err != nil {
		return fail(stderr, err)
	}
	switch cmd {
	case "serve":
		err = serve(ctx, cfg, stdin, stdout, stderr)
	case "pin":
		err = pin(ctx, cfg, stdout)
	case "check":
		err = check(ctx, cfg, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agentrt-proxy: unknown command %q\n\n%s", trace.Sanitize(cmd), Usage)
		return 2
	}
	if err != nil {
		return fail(stderr, err)
	}
	return 0
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "agentrt-proxy: error:", trace.Sanitize(err.Error()))
	return 1
}

// serve loads the proxy and answers one host. The load report goes to
// stderr, where a host's log keeps it.
func serve(ctx context.Context, cfg *proxy.Config, stdin io.Reader, stdout, stderr io.Writer) error {
	p, reports, err := proxy.Open(ctx, cfg, stderr)
	for _, r := range reports {
		fmt.Fprint(stderr, prefixLines(r.String(), "agentrt-proxy: "))
	}
	if err != nil {
		return err
	}
	defer p.Close()
	fmt.Fprintf(stderr, "agentrt-proxy: serving session %s on database %s\n", trace.Sanitize(cfg.Session), trace.Sanitize(cfg.Database))
	return p.Serve(ctx, &sdk.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// pin connects to each server, writes its manifest owner-only, and prints
// each tool with the hints the server claims, which classify nothing and
// are there for the operator to disagree with.
func pin(ctx context.Context, cfg *proxy.Config, stdout io.Writer) error {
	for _, sc := range cfg.Servers {
		m, err := mcp.Pin(ctx, sc.Server())
		if err != nil {
			return err
		}
		raw, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(sc.Manifest, append(raw, '\n'), 0o600); err != nil {
			return err
		}
		if err := os.Chmod(sc.Manifest, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "server %s: %d tools pinned in %s, with the server's own hints, which classify nothing:\n",
			trace.Sanitize(sc.Name), len(m.Tools), trace.Sanitize(sc.Manifest))
		names := make([]string, 0, len(m.Tools))
		for name := range m.Tools {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			t := m.Tools[name]
			fmt.Fprintf(stdout, "  %-28s %-26s %s\n", trace.Sanitize(name), hints(t), oneLine(t.Description, 72))
		}
	}
	fmt.Fprintln(stdout, "classify the tools in the configuration's rules, then: agentrt-proxy check -config <file>")
	return nil
}

// check loads everything as serving would and prints what each server
// offers, what was refused, and what is unclassified. It fails when a
// server registers nothing.
func check(ctx context.Context, cfg *proxy.Config, stdout, stderr io.Writer) error {
	p, reports, err := proxy.Open(ctx, cfg, stderr)
	for _, r := range reports {
		fmt.Fprint(stdout, r.String())
	}
	if err != nil {
		return err
	}
	defer p.Close()
	fmt.Fprintf(stdout, "status tool: %s\nsession: %s\ndatabase: %s\n", trace.Sanitize(cfg.StatusToolName()), trace.Sanitize(cfg.Session), trace.Sanitize(cfg.Database))
	return nil
}

func hints(t mcp.PinnedTool) string {
	if t.Annotations == nil {
		return "no annotations"
	}
	var flags []string
	if t.Annotations.ReadOnlyHint {
		flags = append(flags, "readOnly")
	}
	if t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
		flags = append(flags, "destructive")
	}
	if t.Annotations.IdempotentHint {
		flags = append(flags, "idempotent")
	}
	if t.Annotations.OpenWorldHint != nil && *t.Annotations.OpenWorldHint {
		flags = append(flags, "openWorld")
	}
	if len(flags) == 0 {
		return "claims nothing"
	}
	return strings.Join(flags, ",")
}

func oneLine(s string, n int) string {
	return render.Truncate(trace.Sanitize(strings.Join(strings.Fields(s), " ")), n)
}

func prefixLines(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}
