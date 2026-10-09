package db

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type filterMatrixDialect struct {
	name                                      string
	dialect                                   QueryDialect
	trueSQL, falseSQL, activity, join, search string
}

var filterMatrixDialects = []filterMatrixDialect{
	{"sqlite", SQLiteQueryDialect(), "1", "0", "COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), s.created_at) >= ?", "m.id = tc.message_id", `tc.file_path LIKE ? ESCAPE '\'`},
	{"postgres", PostgresQueryDialect(), "TRUE", "FALSE", "COALESCE(s.ended_at, s.started_at, s.created_at) >= ?::timestamptz", "m.session_id = tc.session_id AND m.ordinal = tc.message_ordinal", `tc.file_path ILIKE ? ESCAPE E'\\'`},
	{"duckdb", DuckDBQueryDialect(), "TRUE", "FALSE", "COALESCE(s.ended_at, s.started_at, s.created_at) >= CAST(? AS TIMESTAMP)", "m.session_id = tc.session_id AND m.id = tc.message_id", `tc.file_path ILIKE ? ESCAPE '\'`},
	{"clickhouse", ClickHouseQueryDialect(), "true", "false", "COALESCE(s.ended_at, s.started_at, s.created_at) >= parseDateTime64BestEffort(?, 6, 'UTC')", "m.session_id = tc.session_id AND m.ordinal = tc.message_ordinal", "tc.file_path ILIKE ?"},
}

func matrixSQL(d filterMatrixDialect, sql string, offset int) string {
	if d.name != "postgres" {
		return sql
	}
	for strings.Contains(sql, "?") {
		offset++
		sql = strings.Replace(sql, "?", fmt.Sprintf("$%d", offset), 1)
	}
	return sql
}

func TestFilterMatrixAnalytics(t *testing.T) {
	for _, d := range filterMatrixDialects {
		t.Run(d.name, func(t *testing.T) {
			base := "s.message_count > 0 AND s.relationship_type NOT IN ('subagent', 'fork') AND s.deleted_at IS NULL"
			clean := "s.termination_status = 'clean'"
			cases := []struct {
				name string
				f    AnalyticsFilter
				sql  string
				args []any
			}{
				{"empty", AnalyticsFilter{}, base, []any{}},
				{"clean", AnalyticsFilter{Termination: "clean"}, base + " AND " + clean, []any{}},
				{"machine", AnalyticsFilter{Machine: " laptop, server, "}, base + " AND s.machine IN (?,?)", []any{"laptop", "server"}},
				{"blank-csv", AnalyticsFilter{Machine: " , ", Agent: " , ", Model: " , "}, base, []any{}},
				{"project", AnalyticsFilter{Project: "team,tools"}, base + " AND s.project = ?", []any{"team,tools"}},
				{"branch", AnalyticsFilter{GitBranch: EncodeBranchFilterToken("team,tools", "topic")}, base + " AND (s.project = ? AND s.git_branch = ?)", []any{"team,tools", "topic"}},
				{"empty-branch", AnalyticsFilter{GitBranch: EncodeBranchFilterToken("alpha", "") + branchListSep + EncodeBranchFilterToken("alpha", "unknown")}, base + " AND ((s.project = ? AND s.git_branch = ?) OR (s.project = ? AND s.git_branch = ?))", []any{"alpha", "", "alpha", "unknown"}},
				{"agent", AnalyticsFilter{Agent: "codex"}, base + " AND s.agent = ?", []any{"codex"}},
				{"minimum", AnalyticsFilter{MinUserMessages: 3}, base + " AND s.user_message_count >= ?", []any{3}},
				{"automated", AnalyticsFilter{AutomatedScope: "automated"}, base + " AND s.is_automated = " + d.trueSQL, []any{}},
				{"human-one-shot", AnalyticsFilter{ExcludeAutomated: true, ExcludeOneShot: true}, base + " AND s.user_message_count > 1 AND s.is_automated = " + d.falseSQL, []any{}},
				{"interactive", AnalyticsFilter{ExcludeInteractive: true}, base + " AND s.is_automated = " + d.trueSQL, []any{}},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					b := NewQueryBuilder(d.dialect, 2)
					got := BuildAnalyticsWhere(c.f, b, "s.", "", nil)
					assert.Equal(t, matrixSQL(d, c.sql, 2), got)
					assert.Equal(t, c.args, b.Args())
				})
			}
			b := NewQueryBuilder(d.dialect, 2)
			f := AnalyticsFilter{Machine: " laptop,server", Project: "team,tools", GitBranch: EncodeBranchFilterToken("team,tools", "main") + branchListSep + EncodeBranchFilterToken("other", "topic"), Agent: "codex,claude", Model: "model-a, model-b", MinUserMessages: 2, IncludeSubagents: true, IncludeForks: true, ExcludeOneShot: true, ActiveSince: "2026-06-01T00:00:00Z", Termination: "clean,awaiting_user"}
			dates := []string{"s.started_at >= " + b.Add("from"), "s.started_at <= " + b.Add("to")}
			modelSQL := "EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id AND m.model IN (?,?))"
			term := "(s.termination_status = 'clean' OR s.termination_status = 'awaiting_user')"
			if d.name == "clickhouse" {
				modelSQL = "s.id IN (SELECT m.session_id FROM messages m WHERE m.model IN (?,?))"
			}
			want := "s.message_count > 0 AND 1=1 AND s.deleted_at IS NULL AND s.started_at >= ? AND s.started_at <= ? AND s.machine IN (?,?) AND s.project = ? AND ((s.project = ? AND s.git_branch = ?) OR (s.project = ? AND s.git_branch = ?)) AND s.agent IN (?,?) AND " + modelSQL + " AND s.user_message_count >= ? AND ((s.user_message_count > 1 OR s.is_automated = " + d.trueSQL + ") OR s.relationship_type = 'subagent') AND " + d.activity + " AND " + term
			assert.Equal(t, matrixSQL(d, want, 2), BuildAnalyticsWhere(f, b, "s.", "", dates))
			assert.Equal(t, []any{"from", "to", "laptop", "server", "team,tools", "team,tools", "main", "other", "topic", "codex", "claude", "model-a", "model-b", 2, f.ActiveSince}, b.Args())
		})
	}
}

func TestFilterMatrixUsage(t *testing.T) {
	for _, d := range filterMatrixDialects {
		t.Run(d.name, func(t *testing.T) {
			b := NewQueryBuilder(d.dialect, 3)
			assert.Empty(t, BuildUsageSourceFilter(UsageFilter{}, b, "ue.model"))
			assert.Empty(t, BuildUsageSessionFilter(UsageFilter{}, b, ""))
			assert.Empty(t, b.Args())
			f := UsageFilter{Model: " a, b, ", ExcludeModel: " c, ", Agent: " codex, claude, ", ProjectLabels: []string{"team,tools", "other"}, Machine: " laptop, ", GitBranch: EncodeBranchFilterToken("team,tools", "topic"), ExcludeProjectLabels: []string{"excluded,tools"}, ExcludeAgent: " cursor, ", MinUserMessages: 2, ExcludeOneShot: true, AutomatedScope: "human", ActiveSince: "2026-06-01T00:00:00Z", Termination: "clean,awaiting_user"}
			source := BuildUsageSourceFilter(f, b, "ue.model")
			assert.Equal(t, matrixSQL(d, "ue.model IN (?,?) AND ue.model != ?", 3), strings.Join(source, " AND "))
			session := BuildUsageSessionFilter(f, b, "session-1")
			want := "s.agent IN (?,?) AND s.project IN (?,?) AND s.machine = ? AND (s.project = ? AND s.git_branch = ?) AND s.project != ? AND s.agent != ? AND s.id = ? AND s.user_message_count >= ? AND s.user_message_count > 1 AND COALESCE(s.is_automated, " + d.falseSQL + ") = " + d.falseSQL + " AND " + d.activity + " AND (s.termination_status = 'clean' OR s.termination_status = 'awaiting_user')"
			assert.Equal(t, matrixSQL(d, want, 6), strings.Join(session, " AND "))
			assert.Equal(t, []any{"a", "b", "c", "codex", "claude", "team,tools", "other", "laptop", "team,tools", "topic", "excluded,tools", "cursor", "session-1", 2, f.ActiveSince}, b.Args())
			for _, raw := range []string{" a, b, ", " , "} {
				t.Run(raw, func(t *testing.T) {
					b := NewQueryBuilder(d.dialect, 0)
					preds := BuildUsageSourceFilter(UsageFilter{Model: raw}, b, "m.model")
					values, sql := []any{"a", "b"}, "m.model IN (?,?)"
					if raw == " , " {
						values, sql = []any{}, ""
					}
					assert.Equal(t, matrixSQL(d, sql, 0), strings.Join(preds, " AND "))
					assert.Equal(t, values, b.Args())
				})
			}
			b = NewQueryBuilder(d.dialect, 0)
			assert.Empty(t, BuildUsageSessionFilter(UsageFilter{Agent: " , ", ExcludeAgent: " , ", Machine: " , ", Project: " , ", ExcludeProject: " , "}, b, ""))
			assert.Empty(t, b.Args())
			b = NewQueryBuilder(d.dialect, 2)
			assert.Equal(t, matrixSQL(d, "s.project IN (?,?) AND s.project NOT IN (?,?)", 2), strings.Join(BuildUsageSessionFilter(UsageFilter{Project: " a, b, ", ExcludeProject: " c, d, "}, b, ""), " AND "))
			assert.Equal(t, []any{"a", "b", "c", "d"}, b.Args())
			b = NewQueryBuilder(d.dialect, 0)
			preds := BuildUsageSessionFilter(UsageFilter{ExcludeOneShot: true, AutomatedScope: "automated"}, b, "")
			automated := "COALESCE(s.is_automated, " + d.falseSQL + ")"
			assert.Equal(t, "(s.user_message_count > 1 OR "+automated+" = "+d.trueSQL+") AND "+automated+" = "+d.trueSQL, strings.Join(preds, " AND "))
		})
	}
}

func TestFilterMatrixTermination(t *testing.T) {
	for _, d := range filterMatrixDialects {
		t.Run(d.name, func(t *testing.T) {
			activity, ph := "COALESCE(s.ended_at, s.started_at, s.created_at)", "?"
			switch d.name {
			case "sqlite":
				activity = "CAST(strftime('%s', COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), s.created_at)) AS INTEGER)"
			case "duckdb":
				ph = "CAST(? AS TIMESTAMP)"
			case "clickhouse":
				ph = "parseDateTime64BestEffort(?, 6, 'UTC')"
			}
			flagged := "s.termination_status IN ('tool_call_pending', 'truncated')"
			active := activity + " > " + ph
			stale := "(" + activity + " > " + ph + " AND " + activity + " <= " + ph + " AND " + flagged + ")"
			unclean := "(" + activity + " <= " + ph + " AND " + flagged + ")"
			for _, c := range []struct {
				status, sql string
				cutoffs     []time.Duration
			}{
				{"active", active, []time.Duration{10 * time.Minute}},
				{"stale", stale, []time.Duration{60 * time.Minute, 10 * time.Minute}},
				{"unclean", unclean, []time.Duration{60 * time.Minute}},
				{"active,stale,unclean", "(" + active + " OR " + stale + " OR " + unclean + ")", []time.Duration{10 * time.Minute, 60 * time.Minute, 10 * time.Minute, 60 * time.Minute}},
			} {
				for _, report := range []string{"usage", "analytics"} {
					t.Run(report+"/"+c.status, func(t *testing.T) {
						before := time.Now().UTC()
						b := NewQueryBuilder(d.dialect, 4)
						want := matrixSQL(d, c.sql, 4)
						if report == "usage" {
							assert.Equal(t, []string{want}, BuildUsageSessionFilter(UsageFilter{Termination: c.status}, b, ""))
						} else {
							base := "s.message_count > 0 AND s.relationship_type NOT IN ('subagent', 'fork') AND s.deleted_at IS NULL"
							assert.Equal(t, base+" AND "+want, BuildAnalyticsWhere(AnalyticsFilter{Termination: c.status}, b, "s.", "", nil))
						}
						args := b.Args()
						require.Len(t, args, len(c.cutoffs))
						for i, delta := range c.cutoffs {
							var cutoff time.Time
							switch d.name {
							case "sqlite":
								v, ok := args[i].(int64)
								require.True(t, ok)
								cutoff = time.Unix(v, 0)
							case "postgres":
								v, ok := args[i].(time.Time)
								require.True(t, ok)
								cutoff = v
							default:
								v, ok := args[i].(string)
								require.True(t, ok)
								var err error
								cutoff, err = time.Parse(time.RFC3339, v)
								require.NoError(t, err)
							}
							assert.WithinDuration(t, before.Add(-delta), cutoff, 2*time.Second)
						}
					})
				}
			}
		})
	}
}

func TestFilterMatrixRecentEdits(t *testing.T) {
	const query = `WITH ranked AS (
 SELECT s.project AS project, tc.file_path AS file_path,
 tc.session_id AS session_id, tc.tool_name AS tool_name,
 tc.category AS category, tc.tool_use_id AS tool_use_id,
 tc.call_index AS call_index, m.ordinal AS ordinal,
 m.timestamp AS timestamp,
 %s AS rn,
 %s AS edit_count
 FROM tool_calls tc
 JOIN messages m ON %s
 JOIN sessions s ON s.id = tc.session_id
 WHERE tc.category IN ('Edit','Write')
 AND tc.file_path IS NOT NULL AND tc.file_path <> ''
 AND s.deleted_at IS NULL %s
 ), file_page AS (
 SELECT project, file_path, edit_count,
 timestamp AS last_edited_at, session_id AS last_session_id,
 ordinal AS last_ordinal, call_index AS last_call_index
 FROM ranked WHERE rn = 1
 ORDER BY last_edited_at DESC NULLS LAST, last_session_id DESC,
 last_ordinal DESC, last_call_index DESC, file_path DESC
 LIMIT ? OFFSET ?
 )
 SELECT fp.project, fp.file_path, fp.edit_count, %s,
 fp.last_session_id, r.session_id, r.ordinal, r.tool_use_id,
 r.call_index, r.tool_name, r.category, %s
 FROM file_page fp
 JOIN ranked r ON r.project = fp.project AND r.file_path = fp.file_path
 WHERE r.rn <= ?
 ORDER BY fp.last_edited_at DESC NULLS LAST, fp.last_session_id DESC,
 fp.last_ordinal DESC, fp.last_call_index DESC, fp.file_path DESC, r.rn`
	for _, d := range filterMatrixDialects {
		t.Run(d.name, func(t *testing.T) {
			row := "ROW_NUMBER() OVER ( PARTITION BY s.project, tc.file_path ORDER BY m.timestamp DESC NULLS LAST, tc.session_id DESC, m.ordinal DESC, tc.call_index DESC)"
			count := "COUNT(*) OVER (PARTITION BY s.project, tc.file_path)"
			lastEdited, timestamp := "fp.last_edited_at", "r.timestamp"
			if d.name == "clickhouse" {
				row = "toInt64(row_number() OVER ( PARTITION BY s.project, tc.file_path ORDER BY m.timestamp DESC NULLS LAST, tc.session_id DESC, m.ordinal DESC, tc.call_index DESC))"
				count = "toInt64(count() OVER (PARTITION BY s.project, tc.file_path))"
				lastEdited = "if(fp.last_edited_at IS NULL, CAST(NULL AS Nullable(String)), concat(replaceAll(toString(fp.last_edited_at), ' ', 'T'), 'Z'))"
				timestamp = "if(r.timestamp IS NULL, CAST(NULL AS Nullable(String)), concat(replaceAll(toString(r.timestamp), ' ', 'T'), 'Z'))"
			}
			for _, raw := range []RecentEditsParams{{}, {Project: "team,tools", Search: `  a%_\b  `, Limit: 5, Offset: 3, MaxEditsPerFile: 2}, {Limit: 201, Offset: -1, MaxEditsPerFile: -1}} {
				p := NormalizeRecentEditsParams(raw)
				b := NewQueryBuilder(d.dialect, 2)
				got := BuildRecentEditsQuery(p, b, d.join)
				filters, wantArgs := "", []any{}
				if raw.Project != "" {
					filters += " AND s.project = ?"
					wantArgs = append(wantArgs, raw.Project)
				}
				if raw.Search != "" {
					filters += " AND " + d.search
					wantArgs = append(wantArgs, `%a\%\_\\b%`)
				}
				if raw.Limit != 5 {
					wantArgs = append(wantArgs, 51, 0, 20)
				} else {
					wantArgs = append(wantArgs, 6, 3, 2)
				}
				want := matrixSQL(d, fmt.Sprintf(query, row, count, d.join, filters, lastEdited, timestamp), 2)
				assert.Equal(t, strings.Join(strings.Fields(want), " "), strings.Join(strings.Fields(got), " "))
				assert.Equal(t, wantArgs, b.Args())
			}
		})
	}
}
