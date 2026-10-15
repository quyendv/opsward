// Package mcpadapter exposes the engine's tools over the Model Context Protocol. It is
// the only package that imports the MCP SDK.
package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/version"
)

// AdapterStdio identifies calls that arrived over MCP stdio.
const AdapterStdio = "mcp-stdio"

// NewServer registers every engine tool on a new MCP server. Every call is made as p.
func NewServer(e *engine.Engine, p core.Principal) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "opsward", Version: version.Version}, nil)
	for _, t := range e.Tools() {
		s.AddTool(&mcp.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: annotations(t.Risk),
		}, handler(e, p, t.Name))
	}
	return s
}

// ServeStdio serves MCP over stdin and stdout as p until ctx is done or the client
// disconnects.
func ServeStdio(ctx context.Context, e *engine.Engine, p core.Principal) error {
	p.Adapter = AdapterStdio
	if err := NewServer(e, p).Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp stdio: %w", err)
	}
	return nil
}

func annotations(r core.Risk) *mcp.ToolAnnotations {
	destructive := r == core.RiskDestructive
	closedWorld := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    r == core.RiskRead,
		DestructiveHint: &destructive,
		OpenWorldHint:   &closedWorld,
	}
}

func handler(e *engine.Engine, p core.Principal, name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := e.Invoke(ctx, p, name, req.Params.Arguments)
		if err != nil {
			// Tool-level errors go in the result, so the agent can read them and adjust.
			return errorResult(err.Error()), nil
		}
		b, err := json.MarshalIndent(res.Data, "", "  ")
		if err != nil {
			return errorResult(fmt.Sprintf("encode result: %v", err)), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
	}
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}
