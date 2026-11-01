package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/agentsview/internal/service"
)

type commitSessionsInput struct {
	SHAs []string `json:"shas" jsonschema:"Commit SHAs or prefixes (7–40 hex characters), at most 200"`
}

func (t *toolset) commitSessions(ctx context.Context, _ *sdk.CallToolRequest, in commitSessionsInput) (*sdk.CallToolResult, service.CommitSessionsResponse, error) {
	rows, err := t.svc.CommitSessions(ctx, in.SHAs)
	return nil, service.CommitSessionsResponse{Commits: rows}, err
}
