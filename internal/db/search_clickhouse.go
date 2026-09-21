package db

import (
	"fmt"
	"strings"
)

// ClickHouseSystemPrefixSQL is the ClickHouse form of SystemPrefixSQL.
//
// ClickHouse evaluates correlated subqueries unreliably and has no
// character-set LTRIM, so this form cannot share systemPrefixSQL's recursive
// EXISTS. It reproduces the same classification with array functions: the
// trimmed content is split on the reminder close tag, the leading run of
// segments that open with a reminder tag is dropped, and the remainder is
// classified exactly like the SQLite form's terminal remainder. Leading
// whitespace is stripped with a regex over the same character set the other
// dialects pass to LTRIM.
func ClickHouseSystemPrefixSQL(contentCol, roleCol string) string {
	trimmed := clickhouseLTrimSQL(contentCol)
	return "NOT (" + roleCol + " = 'user' AND (" +
		clickhouseTerminalRemainderSQL(trimmed) + " OR " +
		clickhouseReminderTerminalSQL(trimmed) + "))"
}

func clickhouseLTrimSQL(expr string) string {
	return "replaceRegexpOne(" + expr + ", '^[" + systemPrefixTrimCutset + "]+', '')"
}

// clickhouseTerminalRemainderSQL matches content that starts with one of the
// known system prefixes or a goal-context wrapper.
func clickhouseTerminalRemainderSQL(content string) string {
	parts := make([]string, 0, len(SystemMsgPrefixes)+1)
	for _, p := range SystemMsgPrefixes {
		parts = append(parts, fmt.Sprintf("startsWith(%s, '%s')", content, p))
	}
	parts = append(parts, clickhouseGoalContextSQL(content))
	return "(" + strings.Join(parts, " OR ") + ")"
}

func clickhouseGoalContextSQL(content string) string {
	openTag := fmt.Sprintf("substring(%[1]s, 1, position(%[1]s, '>'))", content)
	normalized := "replaceRegexpAll(" + openTag + ", '[\t\n\v\f\r]', ' ')"
	attr := make([]string, 0, 3)
	for _, tail := range []string{" ", ">", "/>"} {
		attr = append(attr, fmt.Sprintf("position(%s, '%s%s') > 0",
			normalized, goalContextSourceAttrSQLPrefix, tail))
	}
	return fmt.Sprintf("(startsWith(%s, '%s') OR (startsWith(%s, '%s') AND (%s)))",
		content, legacyGoalContextPrefix,
		content, codexInternalContextTagPrefix,
		strings.Join(attr, " OR "))
}

// clickhouseReminderTerminalSQL matches content that opens with one or more
// complete system-reminder blocks whose remainder is empty or itself a
// system prefix. A reminder without a close tag never matches, mirroring
// stripLeadingSystemReminderBlocks.
func clickhouseReminderTerminalSQL(trimmed string) string {
	segments := fmt.Sprintf("splitByString('%s', %s)", systemReminderCloseTag, trimmed)
	firstPlain := fmt.Sprintf(
		"arrayFirstIndex(x -> NOT startsWith(%s, '%s'), %s)",
		clickhouseLTrimSQL("x"), systemReminderOpenTag, segments)
	remainder := clickhouseLTrimSQL(fmt.Sprintf(
		"arrayStringConcat(arraySlice(%s, greatest(%s, 1)), '%s')",
		segments, firstPlain, systemReminderCloseTag))
	return fmt.Sprintf("(startsWith(%s, '%s') AND %s > 0 AND (%s = '' OR %s))",
		trimmed, systemReminderOpenTag, firstPlain, remainder,
		clickhouseTerminalRemainderSQL(remainder))
}
