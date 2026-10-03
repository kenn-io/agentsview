package parser

import (
	"strings"

	"github.com/tidwall/gjson"
)

func kiroNativeAssistantBody(data gjson.Result) ParsedMessage {
	var body MessageContentBuilder
	data.Get("content").ForEach(func(_, block gjson.Result) bool {
		switch block.Get("kind").Str {
		case "text":
			body.addText(strings.TrimSpace(block.Get("data").Str), "\n\n")
		case "toolUse":
			// Reuse the existing native call decoder, including write/Edit mapping.
			_, calls := kiroExtractAssistant(gjson.Parse(`{"content":[` + block.Raw + `]}`))
			for _, call := range calls {
				call.Rendering = formatToolHeader(call.Category, call.ToolName)
				body.AddToolCall(call)
			}
		}
		return true
	})
	return body.Message()
}

func kiroNativeResultBody(results []ParsedToolResult) ParsedMessage {
	var body MessageContentBuilder
	for _, result := range results {
		body.AddToolResult(result)
	}
	return body.Message()
}
