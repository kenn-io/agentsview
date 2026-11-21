package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func argumentHints(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, request)
		call, ok := result.(*mcp.CallToolResult)
		req, isCall := request.(*mcp.CallToolRequest)
		if method != "tools/call" || !ok || call == nil || !call.IsError || !isCall || req.Params == nil || len(call.Content) == 0 {
			return result, err
		}
		first, ok := call.Content[0].(*mcp.TextContent)
		if !ok || !strings.Contains(first.Text, "unexpected additional properties") {
			return result, err
		}
		listed, listErr := next(ctx, "tools/list", &mcp.ListToolsRequest{Session: req.Session, Params: &mcp.ListToolsParams{}})
		list, ok := listed.(*mcp.ListToolsResult)
		if listErr != nil || !ok || list == nil {
			return result, err
		}
		for _, tool := range list.Tools {
			if tool.Name != req.Params.Name {
				continue
			}
			raw, marshalErr := json.Marshal(tool.InputSchema)
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if marshalErr != nil || json.Unmarshal(raw, &schema) != nil {
				return result, err
			}
			names := make([]string, 0, len(schema.Properties))
			for name := range schema.Properties {
				names = append(names, name)
			}
			slices.Sort(names)
			accepted := "none"
			if len(names) > 0 {
				accepted = strings.Join(names, ", ")
			}
			call.Content = append(call.Content, &mcp.TextContent{Text: "Accepted arguments: " + accepted + "."})
			break
		}
		return result, err
	}
}
