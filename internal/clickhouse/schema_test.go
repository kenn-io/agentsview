package clickhouse

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestEveryTableIsVersionedReplacingMergeTree(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range mirrorTables {
		t.Run(spec.name, func(t *testing.T) {
			require.False(t, seen[spec.name], "duplicate table spec")
			seen[spec.name] = true
			ddl := spec.createSQL()
			assert.Contains(t, ddl, "ENGINE = ReplacingMergeTree(push_version)")
			assert.Contains(t, ddl, "push_version UInt64")
			require.NotEmpty(t, spec.orderBy)
			assert.Contains(t, ddl, "ORDER BY ("+strings.Join(spec.orderBy, ", ")+")")
			names := map[string]columnSpec{}
			for _, c := range spec.columns {
				assert.NotEqual(t, pushVersionCol, c.name, "push_version is appended by createSQL")
				_, dup := names[c.name]
				assert.False(t, dup, "duplicate column %s", c.name)
				names[c.name] = c
			}
			for _, key := range spec.orderBy {
				c, ok := names[key]
				require.True(t, ok, "ordering key %s is not a column", key)
				assert.False(t, strings.HasPrefix(c.typ, "Nullable("),
					"ordering key %s must not be Nullable", key)
			}
		})
	}
}

func TestAddColumnSQLIsIdempotent(t *testing.T) {
	spec, ok := tableByName("sessions")
	require.True(t, ok)
	got := spec.addColumnSQL(colDefault("outcome", tString, "'unknown'"))
	assert.Equal(t,
		"ALTER TABLE sessions ADD COLUMN IF NOT EXISTS outcome String DEFAULT 'unknown'", got)
}

// Every sessions column is either covered by the fingerprint or listed as
// derived with a reason, so a column added to the mirror cannot silently
// escape change detection.
func TestSessionFingerprintCoversEveryMirroredColumn(t *testing.T) {
	spec, ok := tableByName("sessions")
	require.True(t, ok)
	covered := map[string]bool{}
	for _, name := range sessionFingerprintColumns {
		assert.False(t, covered[name], "fingerprint column %s listed twice", name)
		covered[name] = true
	}
	for _, c := range spec.columns {
		if covered[c.name] {
			continue
		}
		_, derived := derivedSessionColumns[c.name]
		assert.True(t, derived, "sessions.%s is neither fingerprinted nor listed as derived", c.name)
	}
	for name := range covered {
		_, exists := tableColumn(spec, name)
		assert.True(t, exists, "fingerprint column %s is not a sessions column", name)
	}
	fields := sessionFingerprintFields(db.Session{}, "m")
	assert.Len(t, fields, len(sessionFingerprintColumns),
		"sessionFingerprintFields and sessionFingerprintColumns must stay parallel")
}

func tableColumn(spec tableSpec, name string) (columnSpec, bool) {
	for _, c := range spec.columns {
		if c.name == name {
			return c, true
		}
	}
	return columnSpec{}, false
}

// Row builders must produce exactly one value per insert column so a
// schema edit without a matching row edit fails here, not on a live
// server.
func TestRowBuildersMatchInsertColumns(t *testing.T) {
	sess := db.Session{ID: "s", CreatedAt: "2026-01-01T00:00:00Z"}
	msg := db.Message{ID: 7, SessionID: "s", Ordinal: 1, Timestamp: "2026-01-01T00:00:00Z"}
	tc := db.ToolCall{ToolName: "read", ResultEvents: []db.ToolResultEvent{{Content: "x"}}}
	s := &Sync{machine: "m", archiveID: "a"}
	rows := map[string][]any{
		"sessions":           s.sessionRow(sessionPayload{session: sess, messages: []db.Message{msg}}, "fp", 1),
		"messages":           messageRow(msg, 1),
		"tool_calls":         toolCallRow(msg, tc, 0, 1),
		"tool_result_events": toolResultEventRow(msg, 0, tc.ResultEvents[0], 1),
		"usage_events":       usageEventRow(db.UsageEvent{SessionID: "s"}, 1),
		"secret_findings":    secretFindingRow(db.SecretFinding{SessionID: "s"}, 0, 1),
		"pinned_messages":    pinnedMessageRow(db.PinnedMessage{SessionID: "s"}, 1),
	}
	for table, row := range rows {
		spec, ok := tableByName(table)
		require.True(t, ok, table)
		assert.Len(t, row, len(spec.columns)+1, "%s row width", table)
		assert.Equal(t, uint64(1), row[len(row)-1], "%s push_version is last", table)
	}
}

func TestLastMessageAtAndTimeValue(t *testing.T) {
	assert.Nil(t, timeValue(""))
	assert.Nil(t, timeValue("not a time"))
	got := timeValue("2026-01-10T00:01:00.000Z")
	require.NotNil(t, got)
	assert.Equal(t, "2026-01-10T00:01:00Z", got.UTC().Format("2006-01-02T15:04:05Z"))

	latest := lastMessageAt([]db.Message{
		{Timestamp: "2026-01-10T00:01:00.000Z"},
		{Timestamp: ""},
		{Timestamp: "2026-01-10T00:03:00.000Z"},
		{Timestamp: "2026-01-10T00:02:00.000Z"},
	})
	require.NotNil(t, latest)
	assert.Equal(t, "2026-01-10T00:03:00Z", latest.Format("2006-01-02T15:04:05Z"))
	assert.Nil(t, lastMessageAt(nil))
}

func TestCanonicalPushScope(t *testing.T) {
	assert.Empty(t, canonicalPushScope(nil, nil))
	assert.Equal(t, canonicalPushScope([]string{"b", "a"}, nil), canonicalPushScope([]string{"a", "b"}, nil))
	assert.NotEqual(t, canonicalPushScope([]string{"a"}, nil), canonicalPushScope(nil, []string{"a"}))
}
