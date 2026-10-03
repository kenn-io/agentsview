package parser

import "encoding/json/v2"

// These paths come from the compiled gemini_coder.Step, cortex and
// codeium_common descriptors in the publisher's Antigravity CLI 1.1.24 binary.
// Unknown payload generations retain the existing best-effort legacy body.
func antigravityNativeStepBody(step antigravityStep) (ParsedMessage, bool) {
	var body MessageContentBuilder
	msg := ParsedMessage{Role: step.role, Timestamp: step.timestamp}
	if fields, ok := antigravityNativePayload(step.fields, 19); ok && step.kind == antigravityStepKindUserInput && antigravityNativeHasFields(fields, 2) {
		body.AddText(antigravityNativeString(fields, 2))
	} else if fields, ok := antigravityNativePayload(step.fields, 20); ok && antigravityNativePlannerBody(fields) {
		msg.Role = RoleAssistant
		// Separate native fields have a deterministic reasoning/text/calls order.
		for _, number := range []int{3, 4, 5, 14, 16} {
			if field, present := agProtoFind(fields, number); present && (number != 5 || field.Varint != 0) {
				thinking := antigravityNativeString(fields, 3)
				if !antigravityNativeHasFields(fields, 3) {
					thinking = antigravityNativeString(fields, 16)
				}
				if redacted, ok := agProtoFind(fields, 5); ok && redacted.Varint != 0 {
					thinking = ""
				}
				body.AddThinking(thinking)
				break
			}
		}
		body.AddText(antigravityNativeString(fields, 1))
		msg.SourceUUID = antigravityNativeString(fields, 6)
		for _, field := range fields {
			if field.Number != 7 || field.Wire != pbWireBytes {
				continue
			}
			call := ParsedToolCall{
				ToolUseID: antigravityNativeString(field.Nested, 1),
				ToolName:  antigravityNativeString(field.Nested, 2),
				InputJSON: antigravityNativeString(field.Nested, 3),
			}
			call.Category = NormalizeToolCategory(call.ToolName)
			call.Rendering = formatToolHeader(call.Category, agyToolDetail(call.ToolName, call.InputJSON))
			body.AddToolCall(call)
		}
	} else if fields, ok := antigravityNativePayload(step.fields, 14); ok && antigravityNativeHasFields(fields, 4) {
		addAntigravityNativeOutput(&body, step.fields, antigravityNativeString(fields, 4))
	} else if fields, ok := antigravityNativePayload(step.fields, 28); ok && antigravityNativeCommandOutput(fields) {
		var output string
		if combined, ok := antigravityNativePayload(fields, 21); ok {
			output = antigravityNativeString(combined, 1)
			if output == "" {
				output = antigravityNativeString(combined, 2)
			}
		} else {
			output = antigravityNativeString(fields, 4)
			if stderr := antigravityNativeString(fields, 5); stderr != "" {
				if output != "" {
					output += "\n"
				}
				output += stderr
			}
		}
		addAntigravityNativeOutput(&body, step.fields, output)
	} else if fields, ok := antigravityNativePayload(step.fields, 114); ok && antigravityNativeHasFields(fields, 1) {
		msg.IsSystem = true
		body.AddText(antigravityNativeString(fields, 1))
	} else {
		return ParsedMessage{}, false
	}
	msg.Content = body.Message().Content
	return msg.withBody(body.Message()), true
}

// A recognized outer tag alone cannot certify an unknown inner generation.
// Explicit empty string fields still establish native body presence.
func antigravityNativeHasFields(fields []agProtoField, numbers ...int) bool {
	for _, number := range numbers {
		if field, ok := agProtoFind(fields, number); ok && field.Wire == pbWireBytes {
			return true
		}
	}
	return false
}

func antigravityNativePlannerBody(fields []agProtoField) bool {
	if antigravityNativeHasFields(fields, 1, 3, 4, 7, 14, 16) {
		return true
	}
	redacted, ok := agProtoFind(fields, 5)
	return ok && redacted.Wire == pbWireVarint && redacted.Varint != 0
}

func antigravityNativeCommandOutput(fields []agProtoField) bool {
	if combined, ok := antigravityNativePayload(fields, 21); ok {
		return antigravityNativeHasFields(combined, 1, 2)
	}
	return antigravityNativeHasFields(fields, 4, 5)
}

func antigravityNativePayload(fields []agProtoField, number int) ([]agProtoField, bool) {
	field, ok := agProtoFind(fields, number)
	if !ok || field.Wire != pbWireBytes || (len(field.Bytes) > 0 && field.Nested == nil) {
		return nil, false
	}
	return field.Nested, true
}

func antigravityNativeString(fields []agProtoField, number int) string {
	field, ok := agProtoFind(fields, number)
	if !ok {
		return ""
	}
	value, _ := agProtoString(field)
	return value
}

func addAntigravityNativeOutput(body *MessageContentBuilder, fields []agProtoField, text string) {
	id := ""
	if metadata, ok := antigravityNativePayload(fields, 5); ok {
		id = antigravityNativeString(metadata, 12)
	}
	raw, _ := json.Marshal(text)
	body.AddToolResult(ParsedToolResult{ToolUseID: id, ContentRaw: string(raw), ContentLength: len(text)})
}
