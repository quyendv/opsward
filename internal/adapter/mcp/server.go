// Package mcpadapter exposes the engine's tools over the Model Context Protocol. It is
// the only package that imports the MCP SDK.
package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/version"
)

// AdapterStdio identifies calls that arrived over MCP stdio.
const AdapterStdio = "mcp-stdio"

// NewServer registers every engine tool on a new MCP server. Every call is made as p.
func NewServer(e *engine.Engine, p core.Principal) *mcp.Server {
	return newServer(e, func(*mcp.CallToolRequest) (core.Principal, error) { return p, nil }, nil)
}

// callContext derives the context a call runs with.
type callContext func(context.Context) (context.Context, context.CancelFunc)

// newServer registers every engine tool; principal names the caller of each call, and
// callCtx, when set, bounds it.
func newServer(e *engine.Engine, principal func(*mcp.CallToolRequest) (core.Principal, error), callCtx callContext) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "opsward", Version: version.Version}, nil)
	for _, t := range e.Tools() {
		s.AddTool(&mcp.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: annotations(t.Risk),
		}, handler(e, principal, callCtx, t.Name))
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

func handler(e *engine.Engine, principal func(*mcp.CallToolRequest) (core.Principal, error), callCtx callContext, name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if callCtx != nil {
			var cancel context.CancelFunc
			ctx, cancel = callCtx(ctx)
			defer cancel()
		}
		p, err := principal(req)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		res, err := e.Invoke(ctx, p, name, req.Params.Arguments)
		if err != nil {
			// Tool-level errors go in the result, so the agent can read them and adjust.
			return errorResult(err.Error()), nil
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false) // read by agents, not browsers
		if err := enc.Encode(res.Data); err != nil {
			return errorResult(fmt.Sprintf("encode result: %v", err)), nil
		}
		content := []mcp.Content{&mcp.TextContent{Text: strings.TrimSuffix(buf.String(), "\n")}}
		if len(res.Notes) > 0 {
			content = append(content, &mcp.TextContent{Text: notesText(res.Notes)})
		}
		return &mcp.CallToolResult{Content: content}, nil
	}
}

// notesText renders attached knowledge after the result, in a separate block so that the
// result itself stays plain JSON.
func notesText(notes []core.Note) string {
	var b strings.Builder
	b.WriteString("Knowledge that may apply to this result:\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "- [%s] %s (id: %s): %s\n", n.Severity, n.Title, n.ID, n.Summary)
	}
	b.WriteString("Read a document in full with get_knowledge.")
	return b.String()
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}
