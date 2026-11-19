package db

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// SystemPrefixRulesVersion changes when the SQL classifier's inputs or
// representative classifications change. ClickHouse keys cached verdicts on it.
func SystemPrefixRulesVersion() string {
	parts := []string{
		"system-reminder-open:" + strconv.Quote(systemReminderOpenTag),
		"system-reminder-close:" + strconv.Quote(systemReminderCloseTag),
		"legacy-goal-prefix:" + strconv.Quote(legacyGoalContextPrefix),
		"codex-context-prefix:" + strconv.Quote(codexInternalContextTagPrefix),
		"goal-source-attribute:" + strconv.Quote(goalContextSourceAttr),
		"trim-cutset:" + strconv.Quote(systemPrefixTrimCutset),
	}
	contents := make([]string, 0, len(SystemMsgPrefixes)*2+len(systemPrefixTrimCutset)+5)
	for _, prefix := range SystemMsgPrefixes {
		parts = append(parts, "system-prefix:"+strconv.Quote(prefix))
		contents = append(contents, prefix+" classifier version probe")
	}
	for _, r := range systemPrefixTrimCutset {
		contents = append(contents, string(r)+"<task-notification> classifier version probe")
	}
	contents = append(contents,
		systemReminderOpenTag+"probe"+systemReminderCloseTag,
		systemReminderOpenTag+"probe"+systemReminderCloseTag+"<task-notification>",
		legacyGoalContextPrefix+"probe",
		codexInternalContextTagPrefix+" "+goalContextSourceAttr+">probe",
		codexInternalContextTagPrefix+` source="other">probe`,
		codexInternalContextTagPrefix+` source="goal"/unexpected>probe`,
		"<codex_internal_context\vsource=\"goal\">probe",
		"<codex_internal_context source=\"goal\"\vother>probe",
	)
	for _, content := range contents {
		for _, role := range []string{"user", "assistant"} {
			parts = append(parts, fmt.Sprintf(
				"probe:%q:%q:%t", content, role,
				sqlSystemPrefixed(content, role),
			))
		}
	}
	fingerprint := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(fingerprint[:])
}
