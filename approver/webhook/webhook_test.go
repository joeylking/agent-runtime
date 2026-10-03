package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/approver"
	"github.com/joeylking/agent-runtime/approver/webhook"
	"github.com/joeylking/agent-runtime/scripted"
)

var secret = []byte(strings.Repeat("s", 32))

type echo struct {
	spec  agentrt.ToolSpec
	calls []agentrt.ToolCall
}

func (e *echo) Spec() agentrt.ToolSpec { return e.spec }
func (e *echo) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	e.calls = append(e.calls, c)
	return agentrt.ToolResult{Content: c.Args, Summary: "echoed"}, nil
}

// facts is repo-steward's ProposalFacts: what its publication approval
// presents and binds.
type facts struct {
	ID         string   `json:"id"`
	Hash       string   `json:"hash"`
	Title      string   `json:"title"`
	HeadRef    string   `json:"head_ref"`
	HeadCommit string   `json:"head_commit"`
	BaseRef    string   `json:"base_ref"`
	Files      []string `json:"files"`
	Target     string   `json:"target"`
}

// publication is a policy shaped like repo-steward's evaluatePublish: the
// publish tool always needs an approval of kind publication whose
// capability and presentation name the frozen proposal and its hash.
func publication(title string) agentrt.PolicyFunc {
	return func(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
		if req.Spec.Name != "publish_proposal" {
			return agentrt.DefaultPolicy().Evaluate(ctx, req, view)
		}
		f := facts{ID: "p-1", Hash: strings.Repeat("a", 64), Title: title, HeadRef: "repo-steward/lib-v1.2.4", HeadCommit: strings.Repeat("b", 40), BaseRef: "main", Files: []string{"go.mod", "go.sum"}, Target: "example.com/lib@v1.2.4"}
		cap := map[string]any{"tool": "publish_proposal", "proposal_id": f.ID, "proposal_hash": f.Hash}
		return agentrt.NeedApproval("publication", "publishing proposal p-1 pushes "+f.HeadRef+" and opens a pull request against main", cap, f)
	}
}

// scene is a store with one run paused on a publication approval and a
// handler over it.
type scene struct {
	t       *testing.T
	store   *agentrt.Store
	driver  *agentrt.Driver
	publish *echo
	run     agentrt.Run
	a       agentrt.Approval
	srv     *httptest.Server
	mu      sync.Mutex
	events  []agentrt.Event
}

type option func(*agentrt.Config, *agentrt.Limits, *webhook.Config)

func withClock(now func() time.Time, ttl time.Duration) option {
	return func(c *agentrt.Config, l *agentrt.Limits, _ *webhook.Config) { c.Now, l.ApprovalTTL = now, ttl }
}

func withWebhook(f func(*webhook.Config)) option {
	return func(_ *agentrt.Config, _ *agentrt.Limits, w *webhook.Config) { f(w) }
}

func newScene(t *testing.T, title string, opts ...option) *scene {
	t.Helper()
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &scene{t: t, store: st}
	s.publish = &echo{spec: agentrt.ToolSpec{Name: "publish_proposal", Description: "push and open a pull request", SideEffect: agentrt.RemoteMutation, Timeout: time.Second,
		InputSchema: []byte(`{"type":"object","properties":{"proposal_id":{"type":"string"}},"required":["proposal_id"],"additionalProperties":false}`)}}
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("publish_proposal", `{"proposal_id":"p-1"}`, "publish"), scripted.Complete(`{"published":true}`)}}
	cfg := agentrt.Config{Store: st, Agent: agent, Policy: publication(title), Tools: []agentrt.Tool{s.publish}}
	limits := agentrt.DefaultLimits()
	wcfg := webhook.Config{Secret: secret, Observer: func(e agentrt.Event) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.events = append(s.events, e)
	}}
	for _, o := range opts {
		o(&cfg, &limits, &wcfg)
	}
	if s.driver, err = agentrt.NewDriver(cfg); err != nil {
		t.Fatal(err)
	}
	if s.run, err = s.driver.Start(context.Background(), "upgrade example.com/lib", limits); err != nil {
		t.Fatal(err)
	}
	if s.run.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("status = %s", s.run.Status)
	}
	as, _ := st.ListApprovals(context.Background(), s.run.ID)
	s.a = as[0]
	wcfg.Approver = approver.New(st, nil)
	h, err := webhook.New(wcfg)
	if err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewServer(h)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *scene) do(method, path string, body any) (*http.Response, map[string]any) {
	s.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := webhook.NewRequest(context.Background(), secret, method, s.srv.URL+path, raw)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.send(req)
}

func (s *scene) send(req *http.Request) (*http.Response, map[string]any) {
	s.t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var out map[string]any
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		json.Unmarshal(b, &out)
	} else {
		out = map[string]any{"text": string(b)}
	}
	return res, out
}

func (s *scene) decision(hash, by string) map[string]any {
	return map[string]any{"run_id": s.run.ID, "approval_id": s.a.ID, "hash": hash, "by": by, "note": "n"}
}

func (s *scene) approvePath() string {
	return "/runs/" + s.run.ID + "/approvals/" + s.a.ID + "/approve"
}
func (s *scene) rejectPath() string { return "/runs/" + s.run.ID + "/approvals/" + s.a.ID + "/reject" }
func (s *scene) showPath() string   { return "/runs/" + s.run.ID + "/approvals/" + s.a.ID }

func (s *scene) requireStatus(res *http.Response, out map[string]any, status int, code string) {
	s.t.Helper()
	if res.StatusCode != status || (code != "" && out["code"] != code) {
		s.t.Fatalf("status %d code %v, want %d %s: %v", res.StatusCode, out["code"], status, code, out)
	}
}

func (s *scene) approval() agentrt.Approval {
	a, err := s.store.GetApproval(context.Background(), s.run.ID, s.a.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	return a
}

func TestHandler_GrantThenResumeBindsTheHash(t *testing.T) {
	s := newScene(t, "Upgrade example.com/lib to v1.2.4")
	// The channel shows the approval first; the hash it shows is what the
	// decision is bound to.
	res, shown := s.do("GET", s.showPath(), nil)
	s.requireStatus(res, shown, 200, "")
	hash, _ := shown["hash"].(string)
	if hash != s.a.Hash || shown["kind"] != "publication" || shown["status"] != "pending" {
		t.Fatalf("shown = %v", shown)
	}
	pres := shown["presentation"].(map[string]any)
	if pres["head_ref"] != "repo-steward/lib-v1.2.4" || pres["title"] != "Upgrade example.com/lib to v1.2.4" {
		t.Fatalf("presentation = %v", pres)
	}
	res, out := s.do("POST", s.approvePath(), s.decision(hash, "slack:joey"))
	s.requireStatus(res, out, 202, "")
	if out["status"] != "approved" || out["by"] != "slack:joey" {
		t.Fatalf("approve = %v", out)
	}
	if len(s.publish.calls) != 0 {
		t.Fatal("approve executed the tool; only resume may")
	}
	if a := s.approval(); a.Status != agentrt.ApprovalApproved || a.DecidedBy != "slack:joey" || a.Note != "n" {
		t.Fatalf("approval = %+v", a)
	}
	// Resume recomputes the hash and executes exactly the recorded request.
	got, err := s.driver.Resume(context.Background(), s.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agentrt.StatusCompleted || len(s.publish.calls) != 1 || string(s.publish.calls[0].Args) != `{"proposal_id":"p-1"}` || s.publish.calls[0].StepID != s.a.StepID {
		t.Fatalf("run %s calls %+v", got.Status, s.publish.calls)
	}
	// Decided: no second grant.
	res, out = s.do("POST", s.approvePath(), s.decision(hash, "slack:joey"))
	s.requireStatus(res, out, 409, "run_not_waiting")
	s.mu.Lock()
	defer s.mu.Unlock()
	var decisions []string
	for _, e := range s.events {
		if e.Type == webhook.EventDecision {
			var p struct {
				Action string
				By     string
				Status int
			}
			json.Unmarshal(e.Payload, &p)
			decisions = append(decisions, fmt.Sprintf("%s %s %d", p.Action, p.By, p.Status))
		}
		if e.RunID != s.run.ID {
			t.Fatalf("event run = %q", e.RunID)
		}
	}
	if strings.Join(decisions, ",") != "approve slack:joey 202,approve slack:joey 409" {
		t.Fatalf("logged decisions = %v", decisions)
	}
}

func TestHandler_StaleHashIsRefusedAndNothingDecided(t *testing.T) {
	s := newScene(t, "t")
	res, out := s.do("POST", s.approvePath(), s.decision(strings.Repeat("0", 64), "joey"))
	s.requireStatus(res, out, 409, "approval_changed")
	if a := s.approval(); a.Status != agentrt.ApprovalPending {
		t.Fatalf("approval = %s", a.Status)
	}
	if r, _ := s.store.GetRun(context.Background(), s.run.ID); r.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("run = %s", r.Status)
	}
}

// A decision without a hash is refused here, as 400 bad_request, before
// it reaches the Approver; approver.Local itself refuses it as changed.
func TestHandler_MissingHashIsABadRequest(t *testing.T) {
	s := newScene(t, "t")
	res, out := s.do("POST", s.approvePath(), s.decision("", "joey"))
	s.requireStatus(res, out, 400, "bad_request")
	if a := s.approval(); a.Status != agentrt.ApprovalPending {
		t.Fatalf("approval = %s", a.Status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Type == webhook.EventDecision {
			t.Fatalf("a refused body reached the Approver: %s", e.Payload)
		}
	}
}

// The approval changes under the operator between the show and the
// decision: the stored hash moves, and the decision bound to the shown
// hash is refused inside the deciding transaction.
func TestHandler_HashBindsAgainstAConcurrentChange(t *testing.T) {
	s := newScene(t, "t")
	res, shown := s.do("GET", s.showPath(), nil)
	s.requireStatus(res, shown, 200, "")
	hash := shown["hash"].(string)
	// Another writer re-presents the approval: a different title and the
	// hash it now has.
	if _, err := s.store.DB().ExecContext(context.Background(), `UPDATE approvals SET presentation_json = json_set(presentation_json, '$.title', 'something else'), hash = ? WHERE id = ?`, strings.Repeat("c", 64), s.a.ID); err != nil {
		t.Fatal(err)
	}
	res, out := s.do("POST", s.approvePath(), s.decision(hash, "joey"))
	s.requireStatus(res, out, 409, "approval_changed")
	res, out = s.do("POST", s.rejectPath(), s.decision(hash, "joey"))
	s.requireStatus(res, out, 409, "approval_changed")
	if a := s.approval(); a.Status != agentrt.ApprovalPending {
		t.Fatalf("approval = %s", a.Status)
	}
}

func TestHandler_ExpiryIsReportedAndTheRuntimeExpires(t *testing.T) {
	past := func() time.Time { return time.Unix(1700000000, 0) }
	s := newScene(t, "t", withClock(past, 10*time.Minute))
	res, out := s.do("GET", s.showPath(), nil)
	s.requireStatus(res, out, 410, "expired")
	// Showing wrote nothing.
	if a := s.approval(); a.Status != agentrt.ApprovalPending {
		t.Fatalf("show changed the approval: %s", a.Status)
	}
	res, out = s.do("POST", s.approvePath(), s.decision(s.a.Hash, "joey"))
	s.requireStatus(res, out, 410, "expired")
	// Deciding ran the runtime's own expiry path.
	if a := s.approval(); a.Status != agentrt.ApprovalExpired {
		t.Fatalf("approval = %s", a.Status)
	}
	if r, _ := s.store.GetRun(context.Background(), s.run.ID); r.Status != agentrt.StatusCancelled || r.Reason != agentrt.ReasonApprovalExpired {
		t.Fatalf("run = %s %s", r.Status, r.Reason)
	}
	if len(s.publish.calls) != 0 {
		t.Fatal("tool ran")
	}
	res, out = s.do("GET", s.showPath(), nil)
	s.requireStatus(res, out, 410, "expired")
}

func TestHandler_RejectCancelsRun(t *testing.T) {
	s := newScene(t, "t")
	res, out := s.do("POST", s.rejectPath(), s.decision(s.a.Hash, "joey"))
	s.requireStatus(res, out, 200, "")
	if out["status"] != "rejected" {
		t.Fatalf("reject = %v", out)
	}
	if r, _ := s.store.GetRun(context.Background(), s.run.ID); r.Status != agentrt.StatusCancelled || r.Reason != agentrt.ReasonApprovalRejected {
		t.Fatalf("run = %s %s", r.Status, r.Reason)
	}
	res, out = s.do("POST", s.rejectPath(), s.decision(s.a.Hash, "joey"))
	s.requireStatus(res, out, 409, "run_not_waiting")
	res, out = s.do("POST", "/runs/"+s.run.ID+"/cancel", map[string]any{"run_id": s.run.ID, "by": "joey"})
	s.requireStatus(res, out, 409, "run_finished")
}

func TestHandler_CancelEndsAWaitingRun(t *testing.T) {
	s := newScene(t, "t")
	res, out := s.do("POST", "/runs/"+s.run.ID+"/cancel", map[string]any{"run_id": s.run.ID, "by": "joey", "note": "stop"})
	s.requireStatus(res, out, 200, "")
	if r, _ := s.store.GetRun(context.Background(), s.run.ID); r.Status != agentrt.StatusCancelled || r.Reason != agentrt.ReasonOperatorCancelled {
		t.Fatalf("run = %s %s", r.Status, r.Reason)
	}
	res, out = s.do("GET", "/runs/"+s.run.ID+"/approvals", nil)
	s.requireStatus(res, out, 200, "")
	if n := len(out["pending"].([]any)); n != 0 {
		t.Fatalf("pending on a cancelled run = %d", n)
	}
}

func TestHandler_NotPendingAfterAGrant(t *testing.T) {
	s := newScene(t, "t")
	res, out := s.do("POST", s.approvePath(), s.decision(s.a.Hash, "joey"))
	s.requireStatus(res, out, 202, "")
	// Still waiting, so the run is in the right state; the approval is
	// the thing already decided.
	res, out = s.do("POST", s.approvePath(), s.decision(s.a.Hash, "joey"))
	s.requireStatus(res, out, 409, "not_pending")
	res, out = s.do("GET", s.showPath(), nil)
	s.requireStatus(res, out, 200, "")
	if out["status"] != "approved" || out["decided_by"] != "joey" {
		t.Fatalf("shown = %v", out)
	}
}

func TestHandler_NotFoundAndNoCrossRunAccess(t *testing.T) {
	s := newScene(t, "t")
	other := newScene(t, "other")
	res, out := s.do("GET", "/runs/nope/approvals", nil)
	s.requireStatus(res, out, 404, "not_found")
	res, out = s.do("GET", "/runs/nope/approvals/"+s.a.ID, nil)
	s.requireStatus(res, out, 404, "not_found")
	// The other run's approval id under this run's path is not found, and
	// a decision on it decides nothing.
	res, out = s.do("GET", "/runs/"+s.run.ID+"/approvals/"+other.a.ID, nil)
	s.requireStatus(res, out, 404, "not_found")
	res, out = s.do("POST", "/runs/"+s.run.ID+"/approvals/"+other.a.ID+"/approve", map[string]any{"run_id": s.run.ID, "approval_id": other.a.ID, "hash": other.a.Hash, "by": "joey"})
	s.requireStatus(res, out, 404, "not_found")
	if a := other.approval(); a.Status != agentrt.ApprovalPending {
		t.Fatalf("other approval = %s", a.Status)
	}
	// Nothing lists runs.
	for _, p := range []string{"/runs", "/runs/", "/approvals", "/"} {
		res, out = s.do("GET", p, nil)
		s.requireStatus(res, out, 404, "not_found")
	}
	// A body naming another run or approval than the path is refused
	// before anything is looked up.
	res, out = s.do("POST", s.approvePath(), map[string]any{"run_id": other.run.ID, "approval_id": s.a.ID, "hash": s.a.Hash, "by": "joey"})
	s.requireStatus(res, out, 400, "bad_request")
	res, out = s.do("POST", s.approvePath(), map[string]any{"run_id": s.run.ID, "approval_id": s.a.ID, "by": "joey"})
	s.requireStatus(res, out, 400, "bad_request")
	res, out = s.do("POST", s.approvePath(), map[string]any{"run_id": s.run.ID, "approval_id": s.a.ID, "hash": s.a.Hash})
	s.requireStatus(res, out, 400, "bad_request")
	res, out = s.do("POST", s.approvePath(), "not an object")
	s.requireStatus(res, out, 400, "bad_request")
	if a := s.approval(); a.Status != agentrt.ApprovalPending {
		t.Fatalf("approval = %s", a.Status)
	}
}

func TestHandler_SignatureSkewAndReplay(t *testing.T) {
	s := newScene(t, "t")
	body, _ := json.Marshal(s.decision(s.a.Hash, "joey"))
	nonces := 0
	signed := func(at time.Time, sig string, secret []byte) *http.Request {
		nonces++
		nonce := fmt.Sprint("n", nonces)
		req, _ := http.NewRequest("POST", s.srv.URL+s.approvePath(), bytes.NewReader(body))
		req.Header.Set(webhook.HeaderTimestamp, strconv.FormatInt(at.Unix(), 10))
		req.Header.Set(webhook.HeaderNonce, nonce)
		if sig == "" {
			sig = webhook.Sign(secret, at, nonce, "POST", s.approvePath(), body)
		}
		req.Header.Set(webhook.HeaderSignature, sig)
		return req
	}
	now := time.Now()
	cases := []struct {
		name string
		req  *http.Request
	}{
		{"no headers", func() *http.Request {
			r, _ := http.NewRequest("POST", s.srv.URL+s.approvePath(), bytes.NewReader(body))
			return r
		}()},
		{"wrong secret", signed(now, "", []byte(strings.Repeat("x", 32)))},
		{"garbage signature", signed(now, "v1=00", secret)},
		{"old timestamp", signed(now.Add(-6*time.Minute), "", secret)},
		{"future timestamp", signed(now.Add(6*time.Minute), "", secret)},
		{"signed for another path", signed(now, webhook.Sign(secret, now, "n0", "POST", s.rejectPath(), body), secret)},
		{"signed for another body", signed(now, webhook.Sign(secret, now, "n0", "POST", s.approvePath(), []byte(`{}`)), secret)},
		{"signed for another method", signed(now, webhook.Sign(secret, now, "n0", "GET", s.approvePath(), body), secret)},
		{"signed for another nonce", signed(now, webhook.Sign(secret, now, "n0", "POST", s.approvePath(), body), secret)},
		{"no nonce", func() *http.Request { r := signed(now, "", secret); r.Header.Del(webhook.HeaderNonce); return r }()},
		{"oversized nonce", func() *http.Request {
			n := strings.Repeat("n", 65)
			r := signed(now, webhook.Sign(secret, now, n, "POST", s.approvePath(), body), secret)
			r.Header.Set(webhook.HeaderNonce, n)
			return r
		}()},
	}
	for _, c := range cases {
		res, out := s.send(c.req)
		if res.StatusCode != 401 || out["code"] != "unauthorized" {
			t.Fatalf("%s: %d %v", c.name, res.StatusCode, out)
		}
	}
	if a := s.approval(); a.Status != agentrt.ApprovalPending {
		t.Fatalf("approval = %s after refused requests", a.Status)
	}
	// A valid request at the edge of the window is accepted once. Its
	// exact replay is refused, even though the decision it carries would
	// now be refused anyway; a replayed GET shows the same.
	edge := now.Add(-4 * time.Minute)
	first := signed(edge, "", secret)
	res, out := s.send(first)
	s.requireStatus(res, out, 202, "")
	replay, _ := http.NewRequest("POST", s.srv.URL+s.approvePath(), bytes.NewReader(body))
	replay.Header = first.Header.Clone()
	res, out = s.send(replay)
	s.requireStatus(res, out, 401, "unauthorized")
	if !strings.Contains(out["error"].(string), "already used") {
		t.Fatalf("replay = %v", out)
	}
	get, _ := webhook.NewRequest(context.Background(), secret, "GET", s.srv.URL+s.showPath(), nil)
	res, out = s.send(get)
	s.requireStatus(res, out, 200, "")
	get2, _ := http.NewRequest("GET", s.srv.URL+s.showPath(), nil)
	get2.Header = get.Header.Clone()
	res, out = s.send(get2)
	s.requireStatus(res, out, 401, "unauthorized")
	s.mu.Lock()
	defer s.mu.Unlock()
	refused := 0
	for _, e := range s.events {
		if e.Type == webhook.EventRefused {
			refused++
		}
	}
	if refused != len(cases)+2 {
		t.Fatalf("refused events = %d", refused)
	}
}

func TestHandler_BodyAndRateBounds(t *testing.T) {
	s := newScene(t, "t", withWebhook(func(c *webhook.Config) { c.MaxBody = 256; c.Rate = 1; c.Burst = 5 }))
	big := s.decision(s.a.Hash, "joey")
	big["note"] = strings.Repeat("n", 300)
	res, out := s.do("POST", s.approvePath(), big)
	s.requireStatus(res, out, 413, "body_too_large")
	if a := s.approval(); a.Status != agentrt.ApprovalPending {
		t.Fatalf("approval = %s", a.Status)
	}
	// One token was spent above; four more go through, the sixth is
	// refused before anything is read or verified.
	for i := 0; i < 4; i++ {
		res, out = s.do("GET", s.showPath(), nil)
		s.requireStatus(res, out, 200, "")
	}
	res, out = s.do("GET", s.showPath(), nil)
	s.requireStatus(res, out, 429, "rate_limited")
	unsigned, _ := http.NewRequest("POST", s.srv.URL+s.approvePath(), strings.NewReader(strings.Repeat("x", 10000)))
	res, out = s.send(unsigned)
	s.requireStatus(res, out, 429, "rate_limited")
}

func TestHandler_TextRenderingIsSanitised(t *testing.T) {
	s := newScene(t, "clear \x1b[2J the screen \u202eand reverse\n\nAPPROVAL WAITING forged")
	req, _ := webhook.NewRequest(context.Background(), secret, "GET", s.srv.URL+s.showPath()+"?format=text", nil)
	res, out := s.send(req)
	text := out["text"].(string)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %s: %s", res.StatusCode, res.Header.Get("Content-Type"), text)
	}
	if strings.Contains(text, "\x1b") || strings.Contains(text, "\u202e") {
		t.Fatalf("control characters reached the rendering:\n%s", text)
	}
	// encoding/json already shows the escape visibly; Sanitize catches
	// what it leaves alone.
	for _, want := range []string{`\u001b[2J`, `\u{202e}`, "kind    publication", "tool    publish_proposal", "hash    " + s.a.Hash, "proposal_id (5 bytes)", "head_ref (25 bytes)", "run     " + s.run.ID} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendering lacks %q:\n%s", want, text)
		}
	}
	last := strings.TrimSpace(text[strings.LastIndex(strings.TrimSpace(text), "\n")+1:])
	if last != "----- end of approval "+s.a.ID+" for tool publish_proposal -----" {
		t.Fatalf("last line = %q", last)
	}
	// The forged prompt line is inside a field, indented and on one line,
	// not at the margin where the real header is.
	if strings.Contains(text, "\nAPPROVAL WAITING") {
		t.Fatalf("forged header at the margin:\n%s", text)
	}
	// Accept selects text too, and JSON is the default.
	req, _ = webhook.NewRequest(context.Background(), secret, "GET", s.srv.URL+s.showPath(), nil)
	req.Header.Set("Accept", "text/plain")
	res, out = s.send(req)
	if res.StatusCode != 200 || !strings.Contains(out["text"].(string), "----- end of approval") {
		t.Fatalf("Accept text: %v", out)
	}
	res, out = s.do("GET", s.showPath(), nil)
	if res.StatusCode != 200 || out["id"] != s.a.ID {
		t.Fatalf("JSON: %v", out)
	}
}

func TestHandler_ListIsBoundedToTheRun(t *testing.T) {
	s := newScene(t, "t", withWebhook(func(c *webhook.Config) { c.Limit = 1 }))
	res, out := s.do("GET", "/runs/"+s.run.ID+"/approvals", nil)
	s.requireStatus(res, out, 200, "")
	pending := out["pending"].([]any)
	if len(pending) != 1 || pending[0].(map[string]any)["id"] != s.a.ID || pending[0].(map[string]any)["hash"] != s.a.Hash {
		t.Fatalf("pending = %v", pending)
	}
	if out["run_id"] != s.run.ID {
		t.Fatalf("list = %v", out)
	}
}

func TestNew_RefusesAWeakConfig(t *testing.T) {
	if _, err := webhook.New(webhook.Config{Secret: secret}); err == nil {
		t.Fatal("no approver accepted")
	}
	st, _ := agentrt.OpenStore(":memory:")
	defer st.Close()
	if _, err := webhook.New(webhook.Config{Approver: approver.New(st, nil), Secret: []byte("short")}); err == nil {
		t.Fatal("short secret accepted")
	}
}

// A corrupted row, one whose fields no longer match its own hash, is a
// distinct refusal from one that changed after it was shown.
func TestHandler_CorruptRowIsRefused(t *testing.T) {
	s := newScene(t, "t")
	if _, err := s.store.DB().ExecContext(context.Background(), `UPDATE approvals SET presentation_json = json_set(presentation_json, '$.title', 'edited') WHERE id = ?`, s.a.ID); err != nil {
		t.Fatal(err)
	}
	res, out := s.do("POST", s.approvePath(), s.decision(s.a.Hash, "joey"))
	s.requireStatus(res, out, 409, "approval_hash")
	if a := s.approval(); a.Status != agentrt.ApprovalPending {
		t.Fatalf("approval = %s", a.Status)
	}
}

// The handler mounted under a prefix the server strips: the client signs
// the path the handler sees.
func TestHandler_BehindStripPrefix(t *testing.T) {
	s := newScene(t, "t")
	st := s.store
	h, err := webhook.New(webhook.Config{Approver: approver.New(st, nil), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.StripPrefix("/hooks", h))
	defer srv.Close()
	now := time.Now()
	req, _ := http.NewRequest("GET", srv.URL+"/hooks"+s.showPath(), nil)
	req.Header.Set(webhook.HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	req.Header.Set(webhook.HeaderNonce, "once")
	req.Header.Set(webhook.HeaderSignature, webhook.Sign(secret, now, "once", "GET", s.showPath(), nil))
	res, out := s.send(req)
	s.requireStatus(res, out, 200, "")
}

// The policy's reason for the approval is in the JSON of a show and a
// list, and in the text.
func TestHandler_ShowAndListCarryThePolicyReason(t *testing.T) {
	s := newScene(t, "t")
	const want = "publishing proposal p-1 pushes repo-steward/lib-v1.2.4 and opens a pull request against main"
	res, out := s.do("GET", s.showPath(), nil)
	s.requireStatus(res, out, 200, "")
	if out["reason"] != want {
		t.Fatalf("show reason = %v", out["reason"])
	}
	res, out = s.do("GET", "/runs/"+s.run.ID+"/approvals", nil)
	s.requireStatus(res, out, 200, "")
	if r := out["pending"].([]any)[0].(map[string]any)["reason"]; r != want {
		t.Fatalf("list reason = %v", r)
	}
	res, out = s.do("GET", s.showPath()+"?format=text", nil)
	if res.StatusCode != 200 || !strings.Contains(out["text"].(string), "\nreason  "+want+"\n") {
		t.Fatalf("text = %v", out["text"])
	}
}
