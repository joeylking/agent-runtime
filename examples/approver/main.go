// Command approver runs one scripted run to a publication-shaped approval,
// serves approver/webhook over its database on a loopback port, and acts
// as the chat bot on the other end: it fetches the approval's text
// rendering, prints it as the message an operator would read, posts the
// decision bound to the hash it showed, and then resumes the run the way
// the consumer's own process would. It makes no network calls beyond
// loopback and needs no model.
//
//	go run ./examples/approver [-db path] [-reject]
//
// -db defaults to a fresh file under a per-user cache directory
// (os.UserCacheDir()/agentrt), created 0700. -reject has the bot refuse
// the approval instead, which cancels the run.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/approver"
	"github.com/joeylking/agent-runtime/approver/webhook"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/trace"
)

func main() {
	def, err := defaultDBPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	dbPath := flag.String("db", def, "SQLite file (default: a fresh file under the per-user cache directory)")
	reject := flag.Bool("reject", false, "refuse the approval instead of granting it")
	flag.Parse()
	if err := run(os.Stdout, *dbPath, *reject); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultDBPath() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "agentrt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("approver-%d.db", time.Now().UnixNano())), nil
}

// publish stands in for a consumer's remote tool: it records what it was
// asked to publish and does nothing else.
type publish struct{ calls []json.RawMessage }

func (p *publish) Spec() agentrt.ToolSpec {
	return agentrt.ToolSpec{Name: "publish_proposal", Description: "Push the frozen proposal and open a pull request.", SideEffect: agentrt.RemoteMutation, Timeout: time.Second,
		InputSchema: []byte(`{"type":"object","properties":{"proposal_id":{"type":"string"}},"required":["proposal_id"],"additionalProperties":false}`)}
}

func (p *publish) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	p.calls = append(p.calls, c.Args)
	return agentrt.ToolResult{Content: []byte(`{"pr":1}`), Summary: "opened pull request 1"}, nil
}

// policy is shaped like repo-steward's: publishing always needs an
// approval of kind publication whose presentation names the proposal.
func policy(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
	if req.Spec.Name != "publish_proposal" {
		return agentrt.DefaultPolicy().Evaluate(ctx, req, view)
	}
	facts := map[string]any{"id": "p-1", "hash": strings.Repeat("a", 64), "title": "Upgrade example.com/lib to v1.2.4", "head_ref": "repo-steward/lib-v1.2.4", "base_ref": "main", "files": []string{"go.mod", "go.sum"}}
	return agentrt.NeedApproval("publication", "publishing pushes a branch and opens a pull request", map[string]any{"tool": req.Spec.Name, "proposal_id": "p-1"}, facts)
}

func run(out io.Writer, dbPath string, reject bool) error {
	ctx := context.Background()
	store, err := agentrt.OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	tool := &publish{}
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("publish_proposal", `{"proposal_id":"p-1"}`, "Push the frozen proposal and open the pull request."),
		scripted.Complete(`{"published":true}`),
	}}
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: agent, Policy: agentrt.PolicyFunc(policy), Tools: []agentrt.Tool{tool}, Observer: trace.Writer(out)})
	if err != nil {
		return err
	}
	limits := agentrt.DefaultLimits()
	limits.ApprovalTTL = time.Hour
	r, err := d.Start(ctx, "upgrade example.com/lib", limits)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nrun %s is %s\n", r.ID, r.Status)

	// The operator's side: a secret from configuration, the handler over
	// the database, loopback only. A deployment puts TLS and a network
	// boundary in front of this.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	h, err := webhook.New(webhook.Config{Approver: approver.New(store, trace.Writer(out)), Secret: secret, Observer: trace.Writer(out)})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()

	// The bot's side: the same secret, the run id it was told about, and
	// nothing else.
	bot := bot{secret: secret, base: base, out: out}
	a, err := bot.show(ctx, r.ID)
	if err != nil {
		return err
	}
	action := "approve"
	if reject {
		action = "reject"
	}
	if err := bot.decide(ctx, action, a, "chat:joey", "reviewed in the channel"); err != nil {
		return err
	}

	// The consumer's side: resume the run, which recomputes the hash and
	// executes exactly the recorded request.
	fmt.Fprintln(out)
	got, err := d.Resume(ctx, r.ID)
	if err != nil {
		fmt.Fprintf(out, "resume: %v\n", err)
		got, err = store.GetRun(ctx, r.ID)
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "\nrun %s is %s (%s); publish_proposal ran %d time(s)\n", got.ID, got.Status, got.Reason, len(tool.calls))
	return nil
}

// bot is the whole of a chat-shaped client: it shows what the handler
// renders and posts what the operator decides.
type bot struct {
	secret []byte
	base   string
	out    io.Writer
}

func (b bot) show(ctx context.Context, runID string) (approver.Pending, error) {
	var list struct {
		Pending []approver.Pending `json:"pending"`
	}
	if _, err := b.call(ctx, "GET", "/runs/"+runID+"/approvals", nil, &list); err != nil {
		return approver.Pending{}, err
	}
	if len(list.Pending) != 1 {
		return approver.Pending{}, fmt.Errorf("run %s has %d pending approvals", runID, len(list.Pending))
	}
	a := list.Pending[0]
	var text strings.Builder
	if _, err := b.call(ctx, "GET", "/runs/"+runID+"/approvals/"+a.ID+"?format=text", nil, &text); err != nil {
		return approver.Pending{}, err
	}
	// The message as the channel would show it: the handler's rendering,
	// already escaped, quoted line by line.
	fmt.Fprintf(b.out, "\n[bot] approval waiting on run %s; reply approve or reject\n", runID)
	for _, line := range strings.Split(strings.TrimRight(text.String(), "\n"), "\n") {
		fmt.Fprintf(b.out, "[bot] > %s\n", line)
	}
	return a, nil
}

func (b bot) decide(ctx context.Context, action string, a approver.Pending, by, note string) error {
	body, err := json.Marshal(approver.Decision{RunID: a.RunID, ApprovalID: a.ID, Hash: a.Hash, By: by, Note: note})
	if err != nil {
		return err
	}
	var reply map[string]any
	status, err := b.call(ctx, "POST", "/runs/"+a.RunID+"/approvals/"+a.ID+"/"+action, body, &reply)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.out, "[bot] %s by %s: HTTP %d %v\n", action, by, status, reply["status"])
	if status >= 400 {
		return fmt.Errorf("%s refused: %v", action, reply["error"])
	}
	return nil
}

func (b bot) call(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	req, err := webhook.NewRequest(ctx, b.secret, method, b.base+path, body)
	if err != nil {
		return 0, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, res.Body)
		return res.StatusCode, err
	}
	return res.StatusCode, json.NewDecoder(res.Body).Decode(out)
}
