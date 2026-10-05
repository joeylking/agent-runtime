// Command agentrt-proxy is a local MCP proxy over stdio that routes every
// tool call an MCP host makes through agent-runtime's gate. Run it with -h
// for what it does and does not do; docs/proxy.md is the full account.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/joeylking/agent-runtime/proxy/internal/cli"
)

func main() {
	// SIGINT and SIGTERM stop the proxy as a closed stdin does: no new
	// call is accepted and the calls in flight finish and are recorded.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
