package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func argumentHints(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, request)
		call, ok := result.(*mcp.CallToolResult)
		req, isCall := request.(*mcp.CallToolRequest)
		if method != "tools/call" || !ok || call == nil || !call.IsError || !isCall {
			return result, err
		}
		var arguments map[string]jsontext.Value
		if json.Unmarshal(req.Params.Arguments, &arguments, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)) != nil {
			return result, err
		}
		listed, listErr := next(ctx, "tools/list", &mcp.ListToolsRequest{Session: req.Session})
		list, ok := listed.(*mcp.ListToolsResult)
		if listErr != nil || !ok {
			return result, err
		}
		for _, tool := range list.Tools {
			if tool.Name != req.Params.Name {
				continue
			}
			raw, marshalErr := json.Marshal(tool.InputSchema)
			var schema struct {
				Properties map[string]jsontext.Value `json:"properties"`
			}
			if marshalErr != nil || json.Unmarshal(raw, &schema) != nil {
				return result, err
			}
			var unknown []string
			for name := range arguments {
				if _, ok := schema.Properties[name]; !ok {
					unknown = append(unknown, name)
				}
			}
			if len(unknown) == 0 {
				return result, err
			}
			slices.Sort(unknown)
			names := make([]string, 0, len(schema.Properties))
			for name := range schema.Properties {
				names = append(names, name)
			}
			slices.Sort(names)
			accepted := "none"
			if len(names) > 0 {
				accepted = strings.Join(names, ", ")
			}
			call.Content = append(call.Content, &mcp.TextContent{Text: "Unknown arguments: " + strings.Join(unknown, ", ") + ". Accepted arguments: " + accepted + "."})
			break
		}
		return result, err
	}
}
