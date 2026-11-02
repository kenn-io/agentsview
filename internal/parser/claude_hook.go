package parser

import (
	"strings"

	"github.com/tidwall/gjson"
)

// claudeHookSuccess holds the text a Claude Code hook_success attachment
// carries. A missing member and an empty member are both an empty string.
type claudeHookSuccess struct {
	hookName  string
	hookEvent string
	content   string
	stdout    string
	stderr    string
}

// extractClaudeHookSuccess decodes the hook_success payload of one
// transcript line. It decodes the payload only: it does not store, order,
// or display the result.
func extractClaudeHookSuccess(line string) (claudeHookSuccess, bool) {
	if !gjson.Valid(line) {
		return claudeHookSuccess{}, false
	}
	record := gjson.Parse(line)
	if !record.IsObject() {
		return claudeHookSuccess{}, false
	}
	recordType := record.Get("type")
	if recordType.Type != gjson.String || recordType.Str != "attachment" {
		return claudeHookSuccess{}, false
	}
	attachment := record.Get("attachment")
	if !attachment.IsObject() {
		return claudeHookSuccess{}, false
	}
	attachmentType := attachment.Get("type")
	if attachmentType.Type != gjson.String ||
		attachmentType.Str != "hook_success" {
		return claudeHookSuccess{}, false
	}
	payload := claudeHookSuccess{}
	members := []struct {
		key    string
		target *string
	}{
		{key: "hookName", target: &payload.hookName},
		{key: "hookEvent", target: &payload.hookEvent},
		{key: "content", target: &payload.content},
		{key: "stdout", target: &payload.stdout},
		{key: "stderr", target: &payload.stderr},
	}
	for _, member := range members {
		value := attachment.Get(member.key)
		if !value.Exists() {
			continue
		}
		if value.Type != gjson.String {
			return claudeHookSuccess{}, false
		}
		*member.target = value.Str
	}
	if strings.TrimSpace(payload.content) == "" &&
		strings.TrimSpace(payload.stdout) == "" &&
		strings.TrimSpace(payload.stderr) == "" {
		return claudeHookSuccess{}, false
	}
	return payload, true
}

// extractClaudeHookAttachment decodes one transcript line into a
// deferred hook attachment. It reuses the strict hook_success decoder,
// so an unsupported attachment type, a malformed payload, and a hook
// without any output stay skipped. The record's own top-level
// identity is carried as written; it is never synthesized.
func extractClaudeHookAttachment(line string) (claudeAttachment, bool) {
	payload, ok := extractClaudeHookSuccess(line)
	if !ok {
		return claudeAttachment{}, false
	}
	// gjson strings can share the input buffer. Retain only the known
	// fields rather than the rest of a potentially large transcript row.
	payload.hookName = strings.Clone(payload.hookName)
	payload.hookEvent = strings.Clone(payload.hookEvent)
	payload.content = strings.Clone(payload.content)
	payload.stdout = strings.Clone(payload.stdout)
	payload.stderr = strings.Clone(payload.stderr)
	return claudeAttachment{
		kind:             claudeAttachmentHookSuccess,
		timestamp:        extractTimestamp(line),
		hook:             &payload,
		sourceUUID:       strings.Clone(gjson.Get(line, "uuid").Str),
		sourceParentUUID: strings.Clone(gjson.Get(line, "parentUuid").Str),
		isSidechain:      gjson.Get(line, "isSidechain").Bool(),
		promptSource:     strings.Clone(gjson.Get(line, "promptSource").Str),
	}, true
}

// claudeHookSuccessBody renders a decoded hook_success payload as a
// plain, inspectable body. Every recorded channel keeps its own
// section, the recorded text is copied verbatim, and a section appears
// only when its decoded value is not the empty string.
func claudeHookSuccessBody(payload claudeHookSuccess) string {
	sections := make([]string, 0, 5)
	if payload.hookName != "" {
		sections = append(sections, "Hook name: "+payload.hookName)
	}
	if payload.hookEvent != "" {
		sections = append(sections, "Hook event: "+payload.hookEvent)
	}
	if payload.content != "" {
		sections = append(sections, "Content:\n"+payload.content)
	}
	if payload.stdout != "" {
		sections = append(sections, "Stdout:\n"+payload.stdout)
	}
	if payload.stderr != "" {
		sections = append(sections, "Stderr:\n"+payload.stderr)
	}
	return strings.Join(sections, "\n\n")
}

// claudeHookSuccessMessage builds the row a hook_success attachment
// becomes. The row keeps Role=user to match the project's system-row
// convention, so role-keyed analytics do not read it as an assistant
// reply, and it stays a system row so it never becomes a user prompt
// or the session preview. The body carries no tool, thinking, token
// usage, model, or billing identity.
func claudeHookSuccessMessage(a claudeAttachment) ParsedMessage {
	body := claudeHookSuccessBody(*a.hook)
	dialogue := ""
	return ParsedMessage{
		Role:             RoleUser,
		Content:          body,
		DialogueText:     new(dialogue),
		Timestamp:        a.timestamp,
		IsSystem:         true,
		ContentLength:    len(body),
		SourceType:       "attachment",
		SourceSubtype:    "hook_success",
		SourceUUID:       a.sourceUUID,
		SourceParentUUID: a.sourceParentUUID,
		IsSidechain:      a.isSidechain,
		PromptSource:     a.promptSource,
	}
}
