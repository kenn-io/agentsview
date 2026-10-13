package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// CodexCloudDelegatedParent returns the parent thread a delegated dot task
// names in its first user message, given that message's item object, or
// "" when the item is not a user message that opens with the tag. The
// codex-cloud fetcher uses it to link a thread to its usage root, with the
// same rule the parser uses for the session's parent.
func CodexCloudDelegatedParent(threadID string, item jsontext.Value) string {
	var it codexCloudItem
	if err := json.Unmarshal(item, &it); err != nil || it.Type != "userMessage" {
		return ""
	}
	match := codexCloudDelegationRe.FindStringSubmatch(codexCloudUserText(it.Content))
	if match == nil || match[1] == threadID {
		return ""
	}
	return match[1]
}

// CodexCloudSpawnParent returns the parent a spawned sub-agent thread's
// source names, or "".
func CodexCloudSpawnParent(source jsontext.Value) string {
	return codexCloudSpawnParent(source)
}
