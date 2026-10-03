package webhook_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/approver"
	"github.com/joeylking/agent-runtime/approver/webhook"
	"github.com/joeylking/agent-runtime/scripted"
)

// A chat bot holding the secret shows an approval as text, posts the
// operator's decision bound to the hash it showed, and the consumer's own
// process resumes the run. A second decision on the same approval is
// refused, and a request signed with another secret never reaches the
// approver.
func ExampleHandler() {
	ctx := context.Background()
	store, err := agentrt.OpenStore(":memory:")
	if err != nil {
		panic(err)
	}
	defer store.Close()
	push := &echo{spec: agentrt.ToolSpec{Name: "push", Description: "push", SideEffect: agentrt.RemoteMutation, Timeout: time.Second,
		InputSchema: []byte(`{"type":"object","properties":{"branch":{"type":"string"}},"additionalProperties":false}`)}}
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"branch":"release"}`, "publish"), scripted.Complete(`{}`)}}
	n := 0
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{push},
		NewID: func() string { n++; return fmt.Sprintf("id-%d", n) }})
	if err != nil {
		panic(err)
	}
	run, err := d.Start(ctx, "release", agentrt.DefaultLimits())
	if err != nil {
		panic(err)
	}

	secret := []byte("an operator-configured secret of 32+ bytes")
	h, err := webhook.New(webhook.Config{Approver: approver.New(store, nil), Secret: secret})
	if err != nil {
		panic(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// The bot lists the run's pending approvals, then shows the one it
	// found as text for the channel.
	var list struct {
		Pending []approver.Pending `json:"pending"`
	}
	call(ctx, secret, "GET", srv.URL+"/runs/"+run.ID+"/approvals", nil, &list)
	a := list.Pending[0]
	var text strings.Builder
	call(ctx, secret, "GET", srv.URL+"/runs/"+run.ID+"/approvals/"+a.ID+"?format=text", nil, &text)
	for _, line := range strings.Split(strings.TrimSpace(text.String()), "\n") {
		// The run id and the hash are stable too, but long; the rest is
		// the rendering.
		if !strings.HasPrefix(line, "APPROVAL") && !strings.HasPrefix(line, "run ") && !strings.HasPrefix(line, "hash ") {
			fmt.Println(line)
		}
	}

	// The operator says yes in the channel; the bot posts the decision
	// with the hash it showed.
	decision, _ := json.Marshal(approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash, By: "chat:joey", Note: "ship it"})
	var out map[string]any
	status := call(ctx, secret, "POST", srv.URL+"/runs/"+run.ID+"/approvals/"+a.ID+"/approve", decision, &out)
	fmt.Println(status, out["status"], out["by"])
	status = call(ctx, secret, "POST", srv.URL+"/runs/"+run.ID+"/approvals/"+a.ID+"/approve", decision, &out)
	fmt.Println(status, out["code"])
	status = call(ctx, []byte("some other secret, also 32+ bytes long"), "POST", srv.URL+"/runs/"+run.ID+"/approvals/"+a.ID+"/reject", decision, &out)
	fmt.Println(status, out["code"])

	got, err := d.Resume(ctx, run.ID)
	fmt.Println(got.Status, len(push.calls), string(push.calls[0].Args), err)
	// Output:
	// kind    remote_mutation
	// tool    push
	// reason  side effect remote_mutation is require_approval by policy
	// arguments (the tool call the model made; one field per line, byte count is the field's own size)
	//   branch (9 bytes)
	//     "release"
	// presentation (supplied by the consumer's policy; it may contain model-chosen text)
	//   args (20 bytes)
	//     {
	//       "branch": "release"
	//     }
	//   tool (6 bytes)
	//     "push"
	// ----- end of approval id-3 for tool push -----
	// 202 approved chat:joey
	// 409 not_pending
	// 401 unauthorized
	// COMPLETED 1 {"branch":"release"} <nil>
}

// call is the whole of a client: a signed request and its decoded reply.
func call(ctx context.Context, secret []byte, method, url string, body []byte, out any) int {
	req, err := webhook.NewRequest(ctx, secret, method, url, body)
	if err != nil {
		panic(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
	if w, ok := out.(io.Writer); ok {
		io.Copy(w, res.Body)
		return res.StatusCode
	}
	json.NewDecoder(res.Body).Decode(out)
	return res.StatusCode
}
