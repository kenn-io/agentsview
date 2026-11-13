package testjsonl

import (
	"fmt"
	"strings"
)

// CodexLineagePage builds a paginated test page with record ordinals.
func CodexLineagePage(thread, timestamp, message string, ordinal int, fields map[string]any) string {
	fields["history_mode"] = "paginated"
	withOrdinal := func(line string, n int) string {
		return strings.TrimSuffix(line, "}") + fmt.Sprintf(`,"ordinal":%d}`, n)
	}
	return JoinJSONL(withOrdinal(CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", timestamp, fields), ordinal), withOrdinal(CodexMsgJSON("user", message, timestamp), ordinal+1))
}

// CodexLineageBase identifies the exclusive history position of a test page.
func CodexLineageBase(thread string, ordinal, bytes int) map[string]any {
	return map[string]any{"thread_id": thread, "end_ordinal_exclusive": ordinal, "end_byte_offset": bytes}
}
