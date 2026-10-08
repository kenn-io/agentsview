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
	name                                                     string
	dialect                                                  QueryDialect
	trueSQL, falseSQL, usageFalseSQL, activity, join, search string
}

var filterMatrixDialects = []filterMatrixDialect{
	{"sqlite", SQLiteQueryDialect(), "1", "0", "0", "COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), s.created_at) >= ?", "m.id = tc.message_id", `tc.file_path LIKE ? ESCAPE '\'`},
	{"postgres", PostgresQueryDialect(), "TRUE", "FALSE", "false", "COALESCE(s.ended_at, s.started_at, s.created_at) >= ?::timestamptz", "m.session_id = tc.session_id AND m.ordinal = tc.message_ordinal", `tc.file_path ILIKE ? ESCAPE E'\\'`},
	{"duckdb", DuckDBQueryDialect(), "TRUE", "FALSE", "FALSE", "COALESCE(s.ended_at, s.started_at, s.created_at) >= CAST(? AS TIMESTAMP)", "m.session_id = tc.session_id AND m.id = tc.message_id", `tc.file_path ILIKE ? ESCAPE '\'`},
	{"clickhouse", ClickHouseQueryDialect(), "true", "false", "false", "COALESCE(s.ended_at, s.started_at, s.created_at) >= parseDateTime64BestEffort(?, 6, 'UTC')", "m.session_id = tc.session_id AND m.ordinal = tc.message_ordinal", "tc.file_path ILIKE ?"},
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
			cases := []struct {
				name string
				f    AnalyticsFilter
				sql  string
				args []any
			}{
				{"empty", AnalyticsFilter{}, base, []any{}},
				{"machine", AnalyticsFilter{Machine: " laptop, server, "}, base + " AND s.machine IN (?,?)", []any{"laptop", "server"}},
				{"blank-csv", AnalyticsFilter{Machine: " , ", Agent: " , ", Model: " , "}, base, []any{}},
				{"project", AnalyticsFilter{Project: "team,tools"}, base + " AND s.project = ?", []any{"team,tools"}},
				{"branch", AnalyticsFilter{GitBranch: EncodeBranchFilterToken("team,tools", "topic")}, base + " AND (s.project = ? AND s.git_branch = ?)", []any{"team,tools", "topic"}},
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
			f := UsageFilter{Model: "a,b", ExcludeModel: "c", Agent: "codex,claude", ProjectLabels: []string{"team,tools", "other"}, Machine: "laptop", GitBranch: EncodeBranchFilterToken("team,tools", "topic"), ExcludeProjectLabels: []string{"excluded,tools"}, ExcludeAgent: "cursor", MinUserMessages: 2, ExcludeOneShot: true, AutomatedScope: "human", ActiveSince: "2026-06-01T00:00:00Z", Termination: "clean,awaiting_user"}
			source := BuildUsageSourceFilter(f, b, "ue.model")
			assert.Equal(t, matrixSQL(d, "ue.model IN (?,?) AND ue.model != ?", 3), strings.Join(source, " AND "))
			session := BuildUsageSessionFilter(f, b, "session-1")
			want := "s.agent IN (?,?) AND s.project IN (?,?) AND s.machine = ? AND (s.project = ? AND s.git_branch = ?) AND s.project != ? AND s.agent != ? AND s.id = ? AND s.user_message_count >= ? AND s.user_message_count > 1 AND COALESCE(s.is_automated, " + d.usageFalseSQL + ") = " + d.falseSQL + " AND " + d.activity + " AND (s.termination_status = 'clean' OR s.termination_status = 'awaiting_user')"
			assert.Equal(t, matrixSQL(d, want, 6), strings.Join(session, " AND "))
			assert.Equal(t, []any{"a", "b", "c", "codex", "claude", "team,tools", "other", "laptop", "team,tools", "topic", "excluded,tools", "cursor", "session-1", 2, f.ActiveSince}, b.Args())
			for _, raw := range []string{" a, b, ", " , "} {
				t.Run(raw, func(t *testing.T) {
					b := NewQueryBuilder(d.dialect, 0)
					preds := BuildUsageSourceFilter(UsageFilter{Model: raw}, b, "m.model")
					values := []any{" a", " b", " "}
					sql := "m.model IN (?,?,?)"
					if raw == " , " {
						values, sql = []any{" ", " "}, "m.model IN (?,?)"
					}
					if d.name == "duckdb" || d.name == "clickhouse" {
						values, sql = []any{"a", "b"}, "m.model IN (?,?)"
						if raw == " , " {
							values, sql = []any{}, ""
						}
					}
					assert.Equal(t, matrixSQL(d, sql, 0), strings.Join(preds, " AND "))
					assert.Equal(t, values, b.Args())
				})
			}
			b = NewQueryBuilder(d.dialect, 0)
			preds := BuildUsageSessionFilter(UsageFilter{ExcludeOneShot: true, AutomatedScope: "automated"}, b, "")
			automated := "COALESCE(s.is_automated, " + d.usageFalseSQL + ")"
			assert.Equal(t, "(s.user_message_count > 1 OR "+automated+" = "+d.trueSQL+") AND "+automated+" = "+d.trueSQL, strings.Join(preds, " AND "))
		})
	}
}

func TestFilterMatrixTermination(t *testing.T) {
	for _, d := range filterMatrixDialects {
		t.Run(d.name, func(t *testing.T) {
			for _, entry := range []string{"usage", "analytics"} {
				t.Run(entry, func(t *testing.T) {
					before := time.Now().UTC()
					b := NewQueryBuilder(d.dialect, 4)
					var preds []string
					if entry == "analytics" {
						preds = []string{BuildAnalyticsWhere(AnalyticsFilter{Termination: "active,stale,unclean"}, b, "s.", "", nil)}
					} else {
						preds = BuildUsageSessionFilter(UsageFilter{Termination: "active,stale,unclean"}, b, "")
					}
					require.Len(t, preds, 1)
					args := b.Args()
					require.Len(t, args, 4)
					for i, delta := range []time.Duration{10 * time.Minute, 60 * time.Minute, 10 * time.Minute, 60 * time.Minute} {
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
					assert.Contains(t, preds[0], "s.termination_status IN ('tool_call_pending', 'truncated')")
					assert.Contains(t, preds[0], "s.ended_at")
					assert.NotContains(t, preds[0], "COALESCE(ended_at")
					assert.NotContains(t, preds[0], "NULLIF(ended_at")
					if d.name == "postgres" {
						for i := 5; i <= 8; i++ {
							assert.Contains(t, preds[0], fmt.Sprintf("$%d", i))
						}
						assert.NotContains(t, preds[0], "::timestamptz")
					}
				})
			}
			b := NewQueryBuilder(d.dialect, 0)
			got := BuildAnalyticsWhere(AnalyticsFilter{Termination: "clean"}, b, "s.", "", nil)
			ending := "s.termination_status = 'clean'"
			if d.name == "duckdb" || d.name == "clickhouse" {
				ending = "(" + ending + ")"
			}
			assert.True(t, strings.HasSuffix(got, " AND "+ending), got)
			assert.Empty(t, b.Args())
		})
	}
}

func TestFilterMatrixRecentEdits(t *testing.T) {
	for _, d := range filterMatrixDialects {
		t.Run(d.name, func(t *testing.T) {
			for _, p := range []RecentEditsParams{{}, {Project: "team,tools", Search: `  a%_\b  `, Limit: 5, Offset: 3, MaxEditsPerFile: 2}, {Limit: 201, Offset: -1, MaxEditsPerFile: -1}} {
				b := NewQueryBuilder(d.dialect, 2)
				got := BuildRecentEditsQuery(p, b, d.join)
				n := 2
				wantArgs := []any{}
				if p.Project != "" {
					assert.Contains(t, got, matrixSQL(d, "AND s.project = ?", n))
					wantArgs = append(wantArgs, p.Project)
					n++
				}
				if p.Search != "" {
					assert.Contains(t, got, matrixSQL(d, "AND "+d.search, n))
					wantArgs = append(wantArgs, `%a\%\_\\b%`)
					n++
				}
				if p.Limit != 5 {
					wantArgs = append(wantArgs, 51, 0, 20)
				} else {
					wantArgs = append(wantArgs, 6, 3, 2)
				}
				assert.Equal(t, wantArgs, b.Args())
				assert.Contains(t, got, "JOIN messages m ON "+d.join)
				assert.Contains(t, got, matrixSQL(d, "LIMIT ? OFFSET ?", n))
				assert.Contains(t, got, matrixSQL(d, "WHERE r.rn <= ?", n+2))
				assert.Contains(t, got, "s.deleted_at IS NULL")
				assert.Contains(t, got, "last_ordinal DESC, fp.last_call_index DESC, fp.file_path DESC")
				if d.name == "clickhouse" {
					assert.Contains(t, got, "toInt64(row_number() OVER (")
					assert.Contains(t, got, "toInt64(count() OVER (PARTITION BY s.project, tc.file_path)) AS edit_count")
					assert.Contains(t, got, "toString(fp.last_edited_at)")
					assert.Contains(t, got, "toString(r.timestamp)")
					assert.NotContains(t, got, "ESCAPE")
				} else {
					assert.Contains(t, got, "COUNT(*) OVER (PARTITION BY s.project, tc.file_path) AS edit_count")
					assert.Contains(t, got, "fp.edit_count, fp.last_edited_at,")
				}
			}
		})
	}
}
