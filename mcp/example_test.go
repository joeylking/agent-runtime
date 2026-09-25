package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// exampleServer is an MCP server in this process, reached over the SDK's
// in-memory transports, so the example needs no subprocess and no port. Each
// call hands out a fresh session: one to pin over, one to load over.
func exampleServer() Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "0"}, nil)
	answer := func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
	}
	srv.AddTool(&sdk.Tool{
		Name: "read_file", Description: "Read a file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, answer)
	srv.AddTool(&sdk.Tool{
		Name: "write_file", Description: "Write a file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string"}},"required":["path","text"]}`),
	}, answer)

	client, server := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), server, nil); err != nil {
		panic(err)
	}
	return Server{Name: "fs", DefaultTimeout: time.Second, transport: client}
}

// Policy is evaluated on a side-effect class and no server may choose its own,
// so a pinned tool with no operator Rule is not registered and the report says
// so. The model never sees it.
func ExampleLoad() {
	ctx := context.Background()
	manifest, err := Pin(ctx, exampleServer())
	if err != nil {
		panic(err)
	}
	tools, report, err := Load(ctx, exampleServer(), manifest, Rules{
		"read_file": {SideEffect: agentrt.ReadOnly},
	})
	if err != nil {
		panic(err)
	}
	defer report.Connection.Close()

	for _, t := range tools {
		fmt.Println("model sees:", t.Spec().Name, t.Spec().SideEffect)
	}
	fmt.Print(report)
	// Output:
	// model sees: fs_read_file read_only
	// fs: registered read_file as fs_read_file (read_only)
	// fs: unclassified write_file: no rule
}
