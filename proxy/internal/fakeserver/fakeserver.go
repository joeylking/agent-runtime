// Package fakeserver is a small MCP server over stdio that stands in for
// a payment service: a read, a payment, and a refund. The proxy's tests and
// its demonstration run it as a child process, so nothing reaches the
// network and nothing needs Node.
//
// Every payment appends one line to the file named by FAKE_PAYMENTS_LEDGER
// before it answers, so a test counts executions by counting lines. A
// payment's mode argument chooses how it answers: "slow" waits two seconds
// first, "block" waits until a file named like the ledger with ".release"
// added exists, "error" answers isError, and "structured" answers with
// structured content. FAKE_PAYMENTS_DESCRIPTION, when set, replaces the
// read's description, as a server that changed since it was pinned would.
package fakeserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Environment variables the server reads.
const (
	EnvLedger      = "FAKE_PAYMENTS_LEDGER"
	EnvDescription = "FAKE_PAYMENTS_DESCRIPTION"
)

const (
	balanceSchema = `{"type":"object","properties":{"account":{"type":"string"}},"required":["account"],"additionalProperties":false}`
	paySchema     = `{"type":"object","properties":{"to":{"type":"string"},"amount":{"type":"number"},"currency":{"type":"string"},` +
		`"mode":{"type":"string","enum":["slow","block","error","structured"]}},"required":["to","amount"],"additionalProperties":false}`
	refundSchema = `{"type":"object","properties":{"payment":{"type":"string"}},"required":["payment"],"additionalProperties":false}`
)

var ledgerMu sync.Mutex

// Run serves until stdin closes or ctx ends.
func Run(ctx context.Context) error {
	srv := sdk.NewServer(&sdk.Implementation{Name: "fake-payments", Version: "1.0.0"}, nil)
	desc := "Report an account's balance."
	if d := os.Getenv(EnvDescription); d != "" {
		desc = d
	}
	no := false
	srv.AddTool(&sdk.Tool{Name: "balance", Description: desc, InputSchema: json.RawMessage(balanceSchema),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, balance)
	srv.AddTool(&sdk.Tool{Name: "pay", Description: "Send a payment.", InputSchema: json.RawMessage(paySchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &no}}, pay)
	srv.AddTool(&sdk.Tool{Name: "refund", Description: "Refund a payment.", InputSchema: json.RawMessage(refundSchema)}, refund)
	return srv.Run(ctx, &sdk.StdioTransport{})
}

func text(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

func balance(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	var in struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
		return nil, err
	}
	return text(fmt.Sprintf("account %s holds 120.00 EUR", in.Account)), nil
}

func pay(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	var in struct {
		To     string  `json:"to"`
		Amount float64 `json:"amount"`
		Mode   string  `json:"mode"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
		return nil, err
	}
	ledger := os.Getenv(EnvLedger)
	if ledger != "" {
		if err := appendLine(ledger, string(req.Params.Arguments)); err != nil {
			return nil, err
		}
	}
	switch in.Mode {
	case "slow":
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
	case "block":
		for {
			if _, err := os.Stat(ledger + ".release"); err == nil {
				break
			}
			select {
			case <-time.After(20 * time.Millisecond):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	case "error":
		res := text(fmt.Sprintf("card declined for %s", in.To))
		res.IsError = true
		return res, nil
	case "structured":
		return &sdk.CallToolResult{
			Content:           []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("paid %.2f to %s", in.Amount, in.To)}},
			StructuredContent: map[string]any{"paid": in.Amount, "to": in.To},
		}, nil
	}
	return text(fmt.Sprintf("paid %.2f to %s", in.Amount, in.To)), nil
}

func refund(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	return text("refunded"), nil
}

// appendLine adds one line to the ledger and syncs it, so a process killed
// right after has left it.
func appendLine(path, line string) error {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// Count is how many payments the ledger at path records.
func Count(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, b := range raw {
		if b == '\n' {
			n++
		}
	}
	return n
}
