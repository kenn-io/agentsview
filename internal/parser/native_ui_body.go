package parser

import "encoding/json/v2"

// composeNativeUIMessage handles fresh Roo/Kilo UI records. Thinking and result
// flags come from native message types, never from marker-looking prose.
func composeNativeUIMessage(message ParsedMessage) ParsedMessage {
	var composer MessageContentBuilder
	if message.HasThinking {
		composer.AddThinking(message.ThinkingText)
	} else if message.SourceSubtype == SourceSubtypeToolResult || len(message.ToolResults) > 0 {
		message.SourceSubtype = SourceSubtypeToolResult
		result := ParsedToolResult{ContentLength: message.ContentLength}
		if len(message.ToolResults) > 0 {
			result = message.ToolResults[0]
		}
		raw, _ := json.Marshal(message.Content)
		result.ContentRaw = string(raw)
		composer.AddToolResult(result)
	} else {
		composer.AddText(message.Content)
	}
	for _, call := range message.ToolCalls {
		composer.AddToolCall(call)
	}
	body := composer.Message()
	body.ContentLength = message.sanitizedHistoricalWorkLength()
	message.Content = body.Content
	return message.withBody(body)
}
