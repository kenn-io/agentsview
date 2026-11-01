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
