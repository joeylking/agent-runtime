// Command example demonstrates agentrt-proxy with no model and no API key.
// A scripted MCP client plays the host, and a fake payment server bundled
// with the proxy plays the upstream. It shows four things:
//
//  1. A read the policy allows runs and answers.
//  2. A payment waits for an operator, is approved from a second process,
//     and the identical call collects it, once.
//  3. The proxy is killed while a payment is inside the server. After a
//     restart the identical call is told the outcome is unknown and waits
//     for the operator, who rejects it, and nothing runs again.
//  4. A host that lost a response retries: through the proxy the payment
//     runs once, and straight against the server it runs twice.
//
// From the repository root, with a workspace that includes ./proxy (see
// CONTRIBUTING.md):
//
//	go run ./proxy/example
//
// Everything lives in a temporary directory, removed at the end unless
// -keep is given. Every process it starts, the proxies, the server, and
// the operator, is waited for before it exits.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/proxy/internal/cli"
	"github.com/joeylking/agent-runtime/proxy/internal/fakeserver"
	"github.com/joeylking/agent-runtime/trace"
	"github.com/joeylking/agent-runtime/view"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The program runs itself again in each role; roleEnv names the role.
const roleEnv = "AGENTRT_PROXY_DEMO_ROLE"

func main() {
	if code, ok := role(context.Background(), os.Getenv(roleEnv), os.Args[1:]); ok {
		os.Exit(code)
	}
	keep := flag.Bool("keep", false, "keep the demo's directory, with its database, for agentrt to read")
	flag.Parse()
	dir, err := os.MkdirTemp("", "agentrt-proxy-demo-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	err = run(context.Background(), dir, os.Stdout)
	if *keep {
		fmt.Printf("\nkept %s; read it with: go run ./cmd/agentrt -db %s runs\n", dir, filepath.Join(dir, "runs.db"))
	} else {
		os.RemoveAll(dir)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", trace.Sanitize(err.Error()))
		os.Exit(1)
	}
}

// role runs one of the child roles and reports whether it did.
func role(ctx context.Context, name string, args []string) (int, bool) {
	switch name {
	case "server":
		if err := fakeserver.Run(ctx); err != nil {
			return 1, true
		}
		return 0, true
	case "proxy":
		return cli.Run(ctx, args, os.Stdin, os.Stdout, os.Stderr), true
	case "operator":
		if err := operate(ctx, args); err != nil {
			fmt.Fprintln(os.Stderr, "operator:", trace.Sanitize(err.Error()))
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// operate is the second process an operator decides from: it opens the
// database as agentrt does, reads the one approval waiting, and approves
// or rejects it bound to the hash it read.
func operate(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: operator <db> approve|reject [note]")
	}
	store, err := agentrt.OpenExisting(args[0], false)
	if err != nil {
		return err
	}
	defer store.Close()
	runs, err := store.ListRuns(ctx)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Status != agentrt.StatusWaitingForApproval {
			continue
		}
		as, err := store.ListApprovals(ctx, r.ID)
		if err != nil {
			return err
		}
		a := as[len(as)-1]
		if a.Status != agentrt.ApprovalPending {
			continue
		}
		note := ""
		if len(args) > 2 {
			note = args[2]
		}
		fmt.Printf("   operator (pid %d) reads approval %s, kind %s:\n     %s\n", os.Getpid(), a.ID, a.Kind, trace.Sanitize(string(a.Capability)))
		fmt.Printf("   operator: agentrt -db <db> %s %s -approval %s\n", args[1], r.ID, a.ID)
		if args[1] == "approve" {
			return view.ApproveShown(ctx, store, nil, r.ID, a.ID, a.Hash, "demo-operator", note)
		}
		return view.RejectShown(ctx, store, nil, r.ID, a.ID, a.Hash, "demo-operator", note)
	}
	return errors.New("nothing is waiting for an approval")
}

func run(ctx context.Context, dir string, out io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	d := &demo{dir: dir, self: self, out: out, ledger: filepath.Join(dir, "ledger"), db: filepath.Join(dir, "runs.db")}
	defer d.stopAll()
	if err := d.configure(); err != nil {
		return err
	}
	for _, step := range []func(context.Context) error{d.read, d.approval, d.crash, d.replay} {
		if err := step(ctx); err != nil {
			return err
		}
	}
	d.stopAll()
	fmt.Fprintln(out, "\nevery process the demo started has exited.")
	return nil
}

type demo struct {
	dir, self, ledger, db, config string
	out                           io.Writer
	procs                         []*child
}

func (d *demo) say(format string, args ...any) { fmt.Fprintf(d.out, format+"\n", args...) }

// configure writes the proxy's configuration and pins the server.
func (d *demo) configure() error {
	cfg := map[string]any{
		"database":  "runs.db",
		"session":   "demo",
		"hold":      "0s",
		"lease_ttl": "3s",
		"servers": []any{map[string]any{
			"name":     "bank",
			"command":  []string{d.self},
			"env":      []string{roleEnv + "=server", fakeserver.EnvLedger + "=" + d.ledger},
			"manifest": "bank.manifest.json",
			"rules": map[string]any{
				"balance": map[string]any{"side_effect": "read_only", "timeout": "5s"},
				"pay":     map[string]any{"side_effect": "remote_mutation", "timeout": "30s"},
			},
		}},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	d.config = filepath.Join(d.dir, "proxy.json")
	if err := os.WriteFile(d.config, raw, 0o600); err != nil {
		return err
	}
	d.say("configuration %s:\n%s\n", d.config, raw)
	var pinOut, pinErr strings.Builder
	if code := cli.Run(context.Background(), []string{"pin", "-config", d.config}, nil, &pinOut, &pinErr); code != 0 {
		return fmt.Errorf("pin: %s", pinErr.String())
	}
	d.say("$ agentrt-proxy pin -config proxy.json\n%s", pinOut.String())
	return nil
}

// child is one process the demo started.
type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	cs    *sdk.ClientSession
	done  chan struct{}
}

// proxy starts agentrt-proxy as a host would and connects to it.
func (d *demo) proxy(ctx context.Context) (*child, error) {
	cmd := exec.Command(d.self, "-config", d.config)
	cmd.Env = append(os.Environ(), roleEnv+"=proxy")
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	return d.connect(ctx, cmd, stdin, stdout)
}

// server starts the payment server alone, as a host without the proxy
// would.
func (d *demo) server(ctx context.Context) (*child, error) {
	cmd := exec.Command(d.self)
	cmd.Env = append(os.Environ(), roleEnv+"=server", fakeserver.EnvLedger+"="+d.ledger)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	return d.connect(ctx, cmd, stdin, stdout)
}

func (d *demo) connect(ctx context.Context, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser) (*child, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() { cmd.Wait(); close(c.done) }()
	d.procs = append(d.procs, c)
	client := sdk.NewClient(&sdk.Implementation{Name: "scripted-host", Version: "1"}, nil)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cs, err := client.Connect(cctx, &sdk.IOTransport{Reader: stdout, Writer: stdin}, nil)
	if err != nil {
		return nil, err
	}
	c.cs = cs
	return c, nil
}

// stop closes a child's stdin and waits for it, killing it after a while.
func (c *child) stop() {
	select {
	case <-c.done:
		return
	default:
	}
	c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(15 * time.Second):
		c.cmd.Process.Kill()
		<-c.done
	}
}

func (d *demo) stopAll() {
	for _, c := range d.procs {
		c.stop()
	}
}

// call makes one call and prints it and its answer as the model would see
// them.
func (d *demo) call(ctx context.Context, c *child, name, args string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := c.cs.CallTool(cctx, &sdk.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, content := range res.Content {
		if t, ok := content.(*sdk.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	mark := ""
	if res.IsError {
		mark = " (isError)"
	}
	d.say("   model calls %s %s\n   model sees%s: %s", name, args, mark, b.String())
	return b.String(), nil
}

// operator decides the waiting approval from a process of its own.
func (d *demo) operator(decision, note string) error {
	cmd := exec.Command(d.self, d.db, decision, note)
	cmd.Env = append(os.Environ(), roleEnv+"=operator")
	cmd.Stdout, cmd.Stderr = d.out, d.out
	return cmd.Run()
}

func want(got, prefix string) error {
	if !strings.HasPrefix(got, prefix) {
		return fmt.Errorf("expected %q, got %q", prefix, got)
	}
	return nil
}

func (d *demo) read(ctx context.Context) error {
	d.say("1. A read the policy allows runs and answers.")
	p, err := d.proxy(ctx)
	if err != nil {
		return err
	}
	defer p.stop()
	got, err := d.call(ctx, p, "bank_balance", `{"account":"household"}`)
	if err != nil {
		return err
	}
	return want(got, "account household holds")
}

func (d *demo) approval(ctx context.Context) error {
	d.say("\n2. A payment waits for an operator, approved from a second process, collected once.")
	p, err := d.proxy(ctx)
	if err != nil {
		return err
	}
	defer p.stop()
	args := `{"to":"landlord","amount":950}`
	got, err := d.call(ctx, p, "bank_pay", args)
	if err != nil {
		return err
	}
	if err := want(got, "PENDING_APPROVAL"); err != nil {
		return err
	}
	d.say("   payments the server has made: %d", fakeserver.Count(d.ledger))
	if err := d.operator("approve", "rent for October"); err != nil {
		return err
	}
	if got, err = d.call(ctx, p, "bank_pay", args); err != nil {
		return err
	}
	if err := want(got, "paid 950.00 to landlord"); err != nil {
		return err
	}
	d.say("   payments the server has made: %d", fakeserver.Count(d.ledger))
	return nil
}

func (d *demo) crash(ctx context.Context) error {
	d.say("\n3. The proxy is killed while a payment is inside the server; after a restart the outcome is unknown.")
	victim, err := d.proxy(ctx)
	if err != nil {
		return err
	}
	args := `{"to":"plumber","amount":180,"mode":"block"}`
	got, err := d.call(ctx, victim, "bank_pay", args)
	if err != nil {
		return err
	}
	if err := want(got, "PENDING_APPROVAL"); err != nil {
		return err
	}
	if err := d.operator("approve", ""); err != nil {
		return err
	}
	before := fakeserver.Count(d.ledger)
	go victim.cs.CallTool(ctx, &sdk.CallToolParams{Name: "bank_pay", Arguments: json.RawMessage(args)})
	for fakeserver.Count(d.ledger) == before {
		time.Sleep(20 * time.Millisecond)
	}
	d.say("   the server has recorded the payment and has not answered; the proxy is killed with SIGKILL")
	victim.cmd.Process.Signal(syscall.SIGKILL)
	<-victim.done
	p, err := d.proxy(ctx)
	if err != nil {
		return err
	}
	defer p.stop()
	if got, err = d.call(ctx, p, "bank_pay", args); err != nil {
		return err
	}
	if !strings.HasPrefix(got, "IN_PROGRESS") && !strings.HasPrefix(got, "INTERRUPTED") {
		return fmt.Errorf("expected IN_PROGRESS or INTERRUPTED, got %q", got)
	}
	d.say("   waiting out the dead proxy's 3s lease")
	time.Sleep(3500 * time.Millisecond)
	if got, err = d.call(ctx, p, "bank_pay", args); err != nil {
		return err
	}
	if err := want(got, "INTERRUPTED"); err != nil {
		return err
	}
	if _, err := d.call(ctx, p, "gate_status", `{}`); err != nil {
		return err
	}
	if err := d.operator("reject", "the bank shows it went through"); err != nil {
		return err
	}
	if got, err = d.call(ctx, p, "bank_pay", args); err != nil {
		return err
	}
	if err := want(got, "REJECTED"); err != nil {
		return err
	}
	d.say("   payments the server has made: %d (the cut-off one ran once, and nothing ran again)", fakeserver.Count(d.ledger))
	return nil
}

func (d *demo) replay(ctx context.Context) error {
	d.say("\n4. A host lost a response and retries the identical call.")
	p, err := d.proxy(ctx)
	if err != nil {
		return err
	}
	defer p.stop()
	args := `{"to":"grocer","amount":42}`
	if _, err := d.call(ctx, p, "bank_pay", args); err != nil {
		return err
	}
	if err := d.operator("approve", ""); err != nil {
		return err
	}
	before := fakeserver.Count(d.ledger)
	if _, err := d.call(ctx, p, "bank_pay", args); err != nil {
		return err
	}
	d.say("   (the host never received that answer and retries)")
	got, err := d.call(ctx, p, "bank_pay", args)
	if err != nil {
		return err
	}
	if err := want(got, "DUPLICATE"); err != nil {
		return err
	}
	withProxy := fakeserver.Count(d.ledger) - before
	d.say("   through the proxy the server made %d payment", withProxy)

	s, err := d.server(ctx)
	if err != nil {
		return err
	}
	defer s.stop()
	before = fakeserver.Count(d.ledger)
	d.say("   the same two calls straight to the server:")
	for range 2 {
		if _, err := d.call(ctx, s, "pay", args); err != nil {
			return err
		}
	}
	without := fakeserver.Count(d.ledger) - before
	d.say("   without the proxy the server made %d payments", without)
	if withProxy != 1 || without != 2 {
		return fmt.Errorf("expected one payment with the proxy and two without, got %d and %d", withProxy, without)
	}
	return nil
}
