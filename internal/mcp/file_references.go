package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/agentsview/internal/service"
)

func (t *toolset) fileReferences(ctx context.Context, _ *mcp.CallToolRequest, in service.FileReferencesInput) (*mcp.CallToolResult, *service.FileReferencesResult, error) {
	out, err := t.svc.FileReferences(ctx, in)
	return nil, out, err
}
