// Command mcp runs one agent against the stock MCP filesystem server with
// every control in the runtime switched on: reads are allowed, a write stops
// the run for an operator, and the trace shows what was refused at load, what
// paused, what was approved, and what executed.
//
// The server is pinned before it is used. `pin` records what each tool
// presents and prints the server's own annotations, which are the hints the
// operator classifies against and never the classification itself. `run`
// reads the operator's rules.json, loads the pinned tools that have a rule,
// and prints the report: what registered, what was refused and why, and every
// tool left unclassified, which is fail-closed and the point of the demo.
//
//	go run ./examples/mcp pin
//	go run ./examples/mcp run
//	go run ./cmd/agentrt -db <path> approve <run>
//	go run ./examples/mcp run -resume <run>
//
// It needs Node for `npx @modelcontextprotocol/server-filesystem` and a local
// Ollama server. The sandbox, the manifest, and the database default to fixed
// paths in the temporary directory so the four commands address the same run;
// rules.json is read from this directory, so both commands are run from the
// repository root. The shipped rules.json classifies the tools the server
// offered when it was written: a server that offers a different set is pinned
// again and the rules edited, which Load says in as many words rather than
// silently dropping a tool.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/mcp"
	"github.com/joeylking/agent-runtime/providers"
	"github.com/joeylking/agent-runtime/providers/ollama"
	"github.com/joeylking/agent-runtime/render"
	"github.com/joeylking/agent-runtime/trace"
)

const goal = "Read README.md in the sandbox, then write notes.md holding a three-line summary of it."

// system is the whole prompt. The runtime supplies the tools, the renderer
// supplies the transcript, and the policy is not negotiable, so there is
// nothing else to say to the model.
const system = `You work inside one sandbox directory through filesystem tools.
- Every reply is exactly one tool call.
- Every path is absolute and inside the sandbox.
- Read a file before you summarise it.
- Write exactly one file, notes.md, whose content is three lines of prose and nothing else.
- Once notes.md is written, call finish with its path and those three lines.
A denied or failed call is answered with its reason; read it and adapt. A
write may stop for an operator: when it does, the call you already made is the
one that runs, so do not make it again.`

// maxOutputTokens is generous because a local model narrates its reasoning
// into the reply before the tool call, and a reply cut off mid-call costs a
// step: the runtime records it as truncated and nudges on the next one.
const maxOutputTokens = 4096

// sandboxREADME is what the agent summarises. The example writes it when the
// sandbox does not exist, so each phase is one command.
const sandboxREADME = `# Sandbox

This directory is the only part of the filesystem the MCP server can reach,
and everything the agent does to it passes the runtime's policy first.

- Reads here are allowed without an operator.
- A write here pauses the run until an operator approves that exact call.
- Every decision, denial, approval, and result is in the run's event log.
`

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// dispatch runs one of the two phases. They are separate commands because
// pinning is something an operator does once, reads, and keeps.
func dispatch(args []string) error {
	tmp := os.TempDir()
	if len(args) == 0 {
		return errors.New("usage: go run ./examples/mcp <pin|run> [flags], or -h on either")
	}
	switch args[0] {
	case "pin":
		fs := flag.NewFlagSet("pin", flag.ExitOnError)
		dir := fs.String("dir", filepath.Join(tmp, "agentrt-mcp-sandbox"), "sandbox the server is given, created with a README.md when absent")
		manifest := fs.String("manifest", filepath.Join(tmp, "agentrt-mcp-manifest.json"), "manifest to write for the operator to review")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return pin(*dir, *manifest)
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		dir := fs.String("dir", filepath.Join(tmp, "agentrt-mcp-sandbox"), "sandbox the server is given, created with a README.md when absent")
		manifest := fs.String("manifest", filepath.Join(tmp, "agentrt-mcp-manifest.json"), "manifest written by pin")
		rules := fs.String("rules", filepath.Join("examples", "mcp", "rules.json"), "the operator's classification of the server's tools")
		db := fs.String("db", filepath.Join(tmp, "agentrt-mcp.db"), "SQLite file")
		model := fs.String("model", "ollama:qwen3:30b-a3b", "ollama:<model> to run")
		resume := fs.String("resume", "", "resume this run instead of starting one")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return run(config{dir: *dir, manifest: *manifest, rules: *rules, db: *db, model: *model, resume: *resume})
	default:
		return fmt.Errorf("unknown command %q: use pin or run", args[0])
	}
}

// server describes the stock filesystem server over stdio, rooted at the
// sandbox and nowhere else. The name prefixes what the model sees.
func server(dir string) mcp.Server {
	return mcp.Server{
		Name:           "fs",
		Command:        []string{"npx", "-y", "@modelcontextprotocol/server-filesystem", dir},
		DefaultTimeout: 5 * time.Second,
	}
}

// pin starts the server, records what it presents, and prints one line per
// tool with the server's own annotations, which is what the operator is
// classifying against.
func pin(dir, manifestPath string) error {
	if err := sandbox(dir); err != nil {
		return err
	}
	// Not a bounded context: npx may be fetching the server on the first
	// run, and the session outlives the call that opened it.
	m, err := mcp.Pin(context.Background(), server(dir))
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("sandbox:  %s\nmanifest: %s\n\n%d tools, with the server's own hints, which classify nothing:\n\n", dir, manifestPath, len(m.Tools))
	for _, name := range sortedTools(m.Tools) {
		t := m.Tools[name]
		fmt.Printf("  %-28s %-26s %s\n", name, hints(t), oneLine(t.Description, 72))
	}
	fmt.Printf("\nclassify them in rules.json, then: go run ./examples/mcp run -dir %s -manifest %s\n", dir, manifestPath)
	return nil
}

// hints renders the flags a server actually asserts. A hint is only worth
// printing when the server claims something: silence is not a claim that a
// tool is safe, which is why an absent annotation never contradicts an
// operator.
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

type config struct {
	dir, manifest, rules, db, model, resume string
}

func run(cfg config) error {
	if err := sandbox(cfg.dir); err != nil {
		return err
	}
	manifest, err := readManifest(cfg.manifest)
	if err != nil {
		return err
	}
	rules, err := readRules(cfg.rules)
	if err != nil {
		return err
	}
	m, err := ollama.New(ollama.Config{Model: modelID(cfg.model), Think: true})
	if err != nil {
		return err
	}
	prices := ollama.Free(m)
	if _, err := providers.PriceFor(prices, m.Name()); err != nil {
		return err
	}
	store, err := agentrt.OpenStore(cfg.db)
	if err != nil {
		return err
	}
	defer store.Close()

	// Not a bounded context, for the same reason as Pin: the session lives
	// as long as the run.
	ctx := context.Background()
	tools, report, err := mcp.Load(ctx, server(cfg.dir), manifest, rules)
	if err != nil {
		if report != nil {
			fmt.Print(report)
		}
		return err
	}
	defer report.Connection.Close()
	fmt.Printf("sandbox:  %s\nmanifest: %s\nrules:    %s\ndatabase: %s\nmodel:    %s\n\nwhat the server offered and what the operator allowed:\n\n%s\n",
		cfg.dir, cfg.manifest, cfg.rules, cfg.db, m.Name(), indent(report.String()))

	driver, err := agentrt.NewDriver(agentrt.Config{
		Store:    store,
		Agent:    &modelAgent{dir: cfg.dir},
		Policy:   approveEveryWrite(),
		Tools:    append(tools, finish()),
		Observer: trace.Writer(os.Stderr),
		Model:    &agentrt.ModelConfig{Model: m, Prices: prices},
	})
	if err != nil {
		return err
	}

	var r agentrt.Run
	if cfg.resume != "" {
		r, err = driver.Resume(ctx, cfg.resume)
	} else {
		limits := agentrt.DefaultLimits()
		limits.MaxSteps, limits.MaxModelCalls, limits.MaxOutputTokensPerCall = 12, 12, maxOutputTokens
		r, err = driver.Start(ctx, goal, limits)
	}
	if err != nil {
		return err
	}
	// The run returned by a pause predates the last step's accounting, so
	// the totals are read back.
	if fresh, ferr := store.GetRun(ctx, r.ID); ferr == nil {
		r = fresh
	}
	printOutcome(r, cfg, store)
	return nil
}

// printOutcome prints the run's outcome and, when it is waiting, the exact
// commands that grant the approval and continue the run.
func printOutcome(r agentrt.Run, cfg config, store *agentrt.Store) {
	fmt.Printf("\nrun %s: %s", r.ID, r.Status)
	if r.Reason != "" {
		fmt.Printf(" (%s)", r.Reason)
	}
	fmt.Printf(" steps=%d calls=%d tokens=%d/%d result=%s\n",
		r.StepCount, r.ModelCalls, r.Usage.InputTokens, r.Usage.OutputTokens, r.Result)
	if r.Status != agentrt.StatusWaitingForApproval {
		fmt.Printf("\nthe audit trail:\n  go run ./cmd/agentrt -db %s events %s\n", cfg.db, r.ID)
		return
	}
	approvals, err := store.ListApprovals(context.Background(), r.ID)
	if err != nil {
		return
	}
	for _, a := range approvals {
		if a.Status == agentrt.ApprovalPending {
			fmt.Printf("\nwaiting for approval %s: %s\n  %s\n", a.ID, a.Kind, a.Presentation)
		}
	}
	fmt.Printf("\ngrant it, then continue the run:\n  go run ./cmd/agentrt -db %s approve %s\n  go run ./examples/mcp run -dir %s -manifest %s -rules %s -db %s -resume %s\n",
		cfg.db, r.ID, cfg.dir, cfg.manifest, cfg.rules, cfg.db, r.ID)
}

// sandbox creates the directory the server is rooted at, with the README the
// agent is asked to summarise, so a first run is one command. An existing
// directory is left exactly as it is.
func sandbox(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "README.md"), []byte(sandboxREADME), 0o644)
}

func readManifest(path string) (*mcp.Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w (run `go run ./examples/mcp pin` first)", err)
	}
	var m mcp.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	return &m, nil
}

// rule is one line of the operator's file: the classification of one tool,
// under the name the server gives it. It is the operator's own format, not
// the runtime's, because a rule is configuration an operator edits.
type rule struct {
	SideEffect        agentrt.SideEffect         `json:"side_effect"`
	Timeout           string                     `json:"timeout,omitempty"`
	Terminal          bool                       `json:"terminal,omitempty"`
	Description       string                     `json:"description,omitempty"`
	Deny              []string                   `json:"deny,omitempty"`
	Fixed             map[string]json.RawMessage `json:"fixed,omitempty"`
	Rename            string                     `json:"rename,omitempty"`
	AllowHintMismatch bool                       `json:"allow_hint_mismatch,omitempty"`
}

// readRules converts the operator's file to mcp.Rules. A tool that is not in
// the file has no rule and is not registered, which is how the file says no.
func readRules(path string) (mcp.Rules, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	var file map[string]rule
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("rules %s: %w", path, err)
	}
	out := make(mcp.Rules, len(file))
	for name, r := range file {
		var timeout time.Duration
		if r.Timeout != "" {
			if timeout, err = time.ParseDuration(r.Timeout); err != nil {
				return nil, fmt.Errorf("rules %s: tool %q: timeout: %w", path, name, err)
			}
		}
		out[name] = mcp.Rule{
			SideEffect:        r.SideEffect,
			Timeout:           timeout,
			Terminal:          r.Terminal,
			Description:       r.Description,
			Deny:              r.Deny,
			Fixed:             r.Fixed,
			Rename:            r.Rename,
			AllowHintMismatch: r.AllowHintMismatch,
		}
	}
	return out, nil
}

func modelID(name string) string {
	id, _ := strings.CutPrefix(name, "ollama:")
	return id
}

// modelAgent is the whole agent: render the recorded steps, ask the model,
// map the reply back to a decision. Nothing domain-specific is left but the
// opening message.
type modelAgent struct{ dir string }

// Decide implements agentrt.Agent.
func (a *modelAgent) Decide(ctx context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	msgs := render.Messages(in, render.Options{Opening: a.opening})
	resp, err := in.Model.Generate(ctx, agentrt.ModelRequest{System: system, Messages: msgs, Tools: in.Tools, MaxOutputTokens: maxOutputTokens})
	if err != nil {
		return agentrt.Decision{}, err
	}
	return render.Decide(resp), nil
}

// opening is the one place a domain fact belongs: where the sandbox is and
// what the two files in it are called.
func (a *modelAgent) opening(in agentrt.StepInput) string {
	return fmt.Sprintf("The sandbox is %s and the tools reach nothing outside it. The file to read is %s and the file to write is %s. %s",
		a.dir, filepath.Join(a.dir, "README.md"), filepath.Join(a.dir, "notes.md"), in.Run.Goal)
}

// write is the part of a write call an operator is shown. It is decoded from
// the arguments the runtime already validated.
type write struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// approveEveryWrite allows reads, sends every local mutation to an operator
// with the path and a preview of what would land in the file, and denies
// everything else. A remote or destructive tool is denied rather than
// approvable, because no rule in this example classifies one: reaching that
// branch means the operator's file changed and the policy has not.
func approveEveryWrite() agentrt.Policy {
	return agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, _ agentrt.RunView) (agentrt.PolicyDecision, error) {
		switch req.Spec.SideEffect {
		case agentrt.ReadOnly:
			return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "a read-only tool needs no authorization"}, nil
		case agentrt.LocalMutation:
			var w write
			if err := json.Unmarshal(req.Args, &w); err != nil {
				return agentrt.PolicyDecision{}, fmt.Errorf("write arguments: %w", err)
			}
			return agentrt.NeedApproval("file_write", "a write inside the sandbox needs an operator",
				map[string]any{"tool": req.Spec.Name, "args": json.RawMessage(req.Args)},
				map[string]any{
					"question": "May the agent write this file?",
					"call":     req.Spec.Name,
					"path":     w.Path,
					"lines":    len(strings.Split(strings.TrimRight(w.Content, "\n"), "\n")),
					"bytes":    len(w.Content),
					"preview":  oneLine(w.Content, 240),
				})
		}
		return agentrt.PolicyDecision{Outcome: agentrt.Deny, Reason: "side effect " + string(req.Spec.SideEffect) + " is not allowed in this example"}, nil
	})
}

// finish is the terminal tool the model ends on, in memory rather than on the
// server: render.Decide never produces a complete decision, so a model-driven
// run needs one tool to finish with.
func finish() agentrt.Tool {
	return terminal{spec: agentrt.ToolSpec{
		Name:        "finish",
		Description: "Report the notes file you wrote and the summary it holds, and end the run.",
		InputSchema: []byte(`{"type":"object","properties":{"notes_path":{"type":"string"},"summary":{"type":"string"}},"required":["notes_path","summary"],"additionalProperties":false}`),
		SideEffect:  agentrt.ReadOnly,
		Timeout:     time.Second,
		Terminal:    true,
	}}
}

type terminal struct{ spec agentrt.ToolSpec }

func (t terminal) Spec() agentrt.ToolSpec { return t.spec }

func (t terminal) Call(_ context.Context, call agentrt.ToolCall) (agentrt.ToolResult, error) {
	var args struct {
		NotesPath string `json:"notes_path"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return agentrt.ToolResult{}, err
	}
	return agentrt.ToolResult{Content: call.Args, Summary: fmt.Sprintf("wrote %s, %d bytes of summary", args.NotesPath, len(args.Summary))}, nil
}

// oneLine collapses whitespace and cuts to n bytes, so a tool description or
// a file preview is one aligned line.
func oneLine(s string, n int) string {
	return render.Truncate(strings.Join(strings.Fields(s), " "), n)
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}

func sortedTools(tools map[string]mcp.PinnedTool) []string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
