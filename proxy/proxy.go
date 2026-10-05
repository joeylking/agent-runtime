// Package proxy is a local MCP proxy over stdio that puts the runtime's
// gate in front of MCP servers it does not own. An MCP host launches it as
// its server; it connects as a client to the real servers through the mcp
// package, which pins their tools and takes their side-effect classes from
// an operator, and routes every tools/call through an agentrt.Gate: the
// call is recorded, validated, held to policy, paused on a hash-bound
// durable approval when policy asks, executed by the gate itself, and its
// outcome recorded. It opens no network listener.
//
// Each tool call is its own run, with the id "<session>.<suffix>", because
// MCP gives a server no notion of a conversation: a host may start one
// proxy per session, per call, or several at once. The rules that span
// calls are the proxy's own, kept in an index of its calls in the same
// database: a call that matches a run waiting on an approval collects it,
// one whose identical request is running is told so, one whose earlier
// attempt's outcome is unknown, cut off by a crash, timed out, or left
// without an answer, waits for an operator, a request rejected
// or expired within the repeat window is not asked for again, and a call
// to a tool that is not read-only that already executed within the window
// is refused as a duplicate.
//
// What it does not do is in docs/proxy.md and is the point of using it
// with care: it governs only the calls that go through it, a model with a
// shell or file access can approve its own request, it does not separate
// the host's user from the operator, it never sees model calls, and it has
// no policy over a conversation's history.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/mcp"
	"github.com/joeylking/agent-runtime/trace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is what the proxy reports to a host as its server version.
const Version = "0.1.0"

// serverName is what the proxy calls itself to a host. It is not the
// command's name: a host may show it to the model, and nothing the model
// sees names the operator command.
const serverName = "gated-tools"

// limits are every run's, fixed: a call's run needs its one tool step and
// at most two more, its re-run after an unknown outcome and the proxy's
// closing decision. An approved re-run whose outcome is unknown again is
// asked about again in the same run, once; the next such re-run meets the
// step limit, which ends the run, and the proxy asks the same question in
// a fresh run of the request at once (rerun, reopen); if that cannot be
// asked, the next mutating call to the tool asks it (unknownBlock).
// ApprovalTTL and GrantTTL come from the configuration.
func limits(cfg *Config) agentrt.Limits {
	return agentrt.Limits{
		MaxSteps:                   3,
		MaxConsecutiveToolFailures: 3,
		LoopThreshold:              3,
		ApprovalTTL:                cfg.ApprovalTTL.or(DefaultApprovalTTL),
		GrantTTL:                   cfg.GrantTTL.or(DefaultGrantTTL),
	}
}

// tool is one registered upstream tool and what the proxy applies to it.
type tool struct {
	name    string
	server  string
	remote  string
	class   agentrt.SideEffect
	spec    agentrt.ToolSpec
	fixed   map[string]json.RawMessage
	outcome agentrt.PolicyOutcome
	window  time.Duration
	inner   agentrt.Tool
	// known matches the recorded errors of calls the server answered
	// (knownError).
	known *regexp.Regexp
}

// gateTool is a tool as the gate registers it: terminal, so a call that
// succeeds completes its run in the transaction that records the result.
type gateTool struct{ t *tool }

func (g gateTool) Spec() agentrt.ToolSpec { return g.t.spec }

func (g gateTool) Call(ctx context.Context, call agentrt.ToolCall) (agentrt.ToolResult, error) {
	return g.t.inner.Call(ctx, call)
}

// Proxy is the loaded proxy: the upstream connections, the store, the
// gate, and the index. Serve runs it over one host connection; Close
// releases it.
type Proxy struct {
	cfg        *Config
	session    string
	store      *agentrt.Store
	idx        *index
	gate       *agentrt.Gate
	decider    decider
	tools      map[string]*tool
	order      []string
	reports    []*mcp.Report
	conns      []*mcp.Connection
	limits     agentrt.Limits
	hold       time.Duration
	maxPending int
	statusName string
	log        io.Writer
	now        func() time.Time
	poll       time.Duration

	// mu makes a call's reading of the index and the runs, and what it
	// begins or collects, one step within this process; the index's
	// transactions do the same across processes. Executing and holding
	// happen outside it, so calls with different requests run in
	// parallel.
	mu sync.Mutex

	life     sync.Mutex
	closing  bool
	inflight sync.WaitGroup
	// stopping is closed when the host connection ends, which ends every
	// hold for an approval at once.
	stopping chan struct{}

	// reruns names, while the proxy proposes a request that re-runs an
	// attempt whose outcome is unknown, that attempt by run id, for the
	// policy (rerunOf).
	reruns struct {
		sync.Mutex
		m map[string]*attempt
	}
}

// options are what a test changes.
type options struct {
	log  io.Writer
	now  func() time.Time
	poll time.Duration
}

// Open loads every upstream server against its manifest and rules, opens
// the database, applies the proxy's own migrations, and builds the gate.
// The load reports are returned even when Open fails, for an operator to
// read. Text a server chose is escaped wherever the proxy logs it to log.
func Open(ctx context.Context, cfg *Config, log io.Writer) (*Proxy, []*mcp.Report, error) {
	return open(ctx, cfg, options{log: log})
}

func open(ctx context.Context, cfg *Config, o options) (*Proxy, []*mcp.Report, error) {
	if o.log == nil {
		o.log = io.Discard
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.poll <= 0 {
		o.poll = 200 * time.Millisecond
	}
	p := &Proxy{
		cfg:        cfg,
		session:    cfg.Session,
		decider:    classPolicy(cfg.policy()),
		tools:      map[string]*tool{},
		limits:     limits(cfg),
		hold:       cfg.Hold.or(DefaultHold),
		maxPending: cfg.maxPending(),
		statusName: cfg.StatusToolName(),
		log:        o.log,
		now:        o.now,
		poll:       o.poll,
		stopping:   make(chan struct{}),
	}
	p.reruns.m = map[string]*attempt{}
	fail := func(err error) (*Proxy, []*mcp.Report, error) {
		p.Close()
		return nil, p.reports, err
	}
	if err := checkDatabase(cfg.Database); err != nil {
		return fail(err)
	}
	if err := cfg.checkReach(); err != nil {
		return fail(err)
	}
	window := cfg.RepeatWindow.or(DefaultRepeatWindow)
	var gateTools []agentrt.Tool
	for _, sc := range cfg.Servers {
		manifest, err := readManifest(sc.Manifest)
		if err != nil {
			return fail(fmt.Errorf("server %q: manifest: %w", sc.Name, err))
		}
		rules, err := sc.rules()
		if err != nil {
			return fail(err)
		}
		loaded, report, err := mcp.Load(ctx, sc.Server(), manifest, rules)
		if report != nil {
			p.reports = append(p.reports, report)
		}
		if err != nil {
			return fail(err)
		}
		p.conns = append(p.conns, report.Connection)
		// Load registers its tools in the order of report.Registered.
		for i, at := range loaded {
			reg := report.Registered[i]
			spec := at.Spec()
			spec.Terminal = true
			if spec.Name == p.statusName {
				return fail(fmt.Errorf("server %q registers tool %q under %q, the status tool's name: rename one (status_tool or the rule's rename)", sc.Name, reg.Tool, spec.Name))
			}
			if _, dup := p.tools[spec.Name]; dup {
				return fail(fmt.Errorf("server %q registers %q, which another server already registered", sc.Name, spec.Name))
			}
			rule := sc.Rules[reg.Tool]
			t := &tool{
				name: spec.Name, server: sc.Name, remote: reg.Tool, class: spec.SideEffect, spec: spec,
				fixed: rule.Fixed, outcome: rule.Outcome, window: rule.RepeatWindow.or(window), inner: at,
			}
			t.known = knownError(t)
			p.tools[spec.Name] = t
			p.order = append(p.order, spec.Name)
			gateTools = append(gateTools, gateTool{t})
		}
	}
	store, err := agentrt.OpenStore(cfg.Database)
	if err != nil {
		return fail(err)
	}
	p.store = store
	// Checked again on the file the store now has open, which exists.
	if err := checkDatabase(cfg.Database); err != nil {
		return fail(err)
	}
	if p.idx, err = migrateIndex(ctx, store); err != nil {
		return fail(err)
	}
	gcfg := agentrt.GateConfig{
		Store:      store,
		Policy:     gatePolicy{p},
		Tools:      gateTools,
		Reconcile:  p.reconcile,
		Now:        o.now,
		LeaseOwner: "agentrt-proxy:" + cfg.Session,
	}
	if cfg.LeaseTTL != nil {
		gcfg.LeaseTTL = cfg.LeaseTTL.Duration
	}
	if p.gate, err = agentrt.NewGate(gcfg); err != nil {
		return fail(err)
	}
	return p, p.reports, nil
}

// checkDatabase refuses a database file that another user owns, or that
// its group or others can write: whoever can write it can forge an
// approval. A symbolic link is refused rather than followed, as agentrt,
// which opens the file for the operator, refuses one. It is checked before
// the store opens the file, so the store never follows a link, and again
// after, on the file as it then is. A file that does not exist yet is
// created owner-only by the store. The directory holding it is not
// checked, by the proxy or by the store: an operator who keeps it where
// others can replace files keeps every file there at their mercy.
func checkDatabase(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("database: %s is a symbolic link; name the database file itself", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database: %s is not a regular file", path)
	}
	if err := mcp.OwnedPrivately(path, info); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	return nil
}

// Reports are the upstream servers' load reports.
func (p *Proxy) Reports() []*mcp.Report { return p.reports }

// Close closes the upstream connections and the database. Serve has
// already waited for the calls in flight.
func (p *Proxy) Close() error {
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
	if p.store != nil {
		err := p.store.Close()
		p.store = nil
		return err
	}
	return nil
}

// Serve answers one host over t until the host closes the connection or
// ctx is cancelled. It then accepts no more calls, waits for every call in
// flight to finish, each within its tool's timeout, and returns. The
// upstream connections and the database stay open until Close.
func (p *Proxy) Serve(ctx context.Context, t sdk.Transport) error {
	srv := sdk.NewServer(&sdk.Implementation{Name: serverName, Version: Version}, nil)
	for _, name := range p.order {
		tl := p.tools[name]
		srv.AddTool(&sdk.Tool{Name: tl.name, Description: tl.spec.Description, InputSchema: json.RawMessage(tl.spec.InputSchema)}, p.handler(tl))
	}
	srv.AddTool(&sdk.Tool{Name: p.statusName, Description: statusDescription, InputSchema: json.RawMessage(statusSchema)}, p.statusHandler)
	err := srv.Run(ctx, t)
	p.life.Lock()
	if !p.closing {
		p.closing = true
		close(p.stopping)
	}
	p.life.Unlock()
	p.inflight.Wait()
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// enter admits a call unless Serve is shutting down.
func (p *Proxy) enter() bool {
	p.life.Lock()
	defer p.life.Unlock()
	if p.closing {
		return false
	}
	p.inflight.Add(1)
	return true
}

func (p *Proxy) handler(t *tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if !p.enter() {
			return nil, errors.New("the proxy is shutting down")
		}
		defer p.inflight.Done()
		var token any
		if req.Params != nil {
			token = req.Params.GetProgressToken()
		}
		return p.handle(ctx, req.Session, token, t, arguments(req.Params)), nil
	}
}

// arguments are a call's arguments as the gate records them: absent or
// null arguments are the empty object, as a host sends for a tool that
// takes none.
func arguments(params *sdk.CallToolParamsRaw) json.RawMessage {
	if params == nil {
		return json.RawMessage("{}")
	}
	raw := json.RawMessage(strings.TrimSpace(string(params.Arguments)))
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}

func (p *Proxy) logf(format string, args ...any) {
	fmt.Fprintln(p.log, "agentrt-proxy: "+trace.Sanitize(fmt.Sprintf(format, args...)))
}

// operatorHint tells the operator, on stderr only, how to decide an
// approval.
func (p *Proxy) operatorHint(a *agentrt.Approval, t *tool) {
	p.logf("approval %s (%s) for %s waits for an operator: run %s", a.ID, a.Kind, t.name, a.RunID)
	p.logf("  decide with: agentrt -db %q approve %s -approval %s   (or reject)", p.cfg.Database, a.RunID, a.ID)
}

func newSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("agentrt-proxy: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
