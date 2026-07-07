package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

type analyticsProbeDriver struct{}

type analyticsProbeConn struct {
	state *analyticsProbeState
}

type analyticsProbeRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

type analyticsProbeState struct {
	mu      sync.Mutex
	queries []string
	args    [][]any
}

var (
	analyticsProbeRegisterOnce sync.Once
	analyticsProbeStatesMu     sync.Mutex
	analyticsProbeStates       = map[string]*analyticsProbeState{}
)

func newAnalyticsProbeDB(
	t *testing.T, state *analyticsProbeState,
) *sql.DB {
	t.Helper()
	analyticsProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_analytics_probe", analyticsProbeDriver{})
	})
	name := t.Name()
	analyticsProbeStatesMu.Lock()
	analyticsProbeStates[name] = state
	analyticsProbeStatesMu.Unlock()
	t.Cleanup(func() {
		analyticsProbeStatesMu.Lock()
		delete(analyticsProbeStates, name)
		analyticsProbeStatesMu.Unlock()
	})

	pg, err := sql.Open("agentsview_analytics_probe", name)
	require.NoError(t, err, "open analytics probe db")
	t.Cleanup(func() { pg.Close() })
	return pg
}

func (analyticsProbeDriver) Open(name string) (driver.Conn, error) {
	analyticsProbeStatesMu.Lock()
	state := analyticsProbeStates[name]
	analyticsProbeStatesMu.Unlock()
	return &analyticsProbeConn{state: state}, nil
}

func (c *analyticsProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (c *analyticsProbeConn) Close() error { return nil }

func (c *analyticsProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not implemented")
}

func (c *analyticsProbeConn) QueryContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	c.state.mu.Lock()
	c.state.queries = append(c.state.queries, query)
	c.state.args = append(c.state.args, values)
	c.state.mu.Unlock()

	normalized := strings.ToLower(query)
	switch {
	case strings.Contains(normalized, "select distinct model"):
		sessionModels := make([][]driver.Value, 0)
		for _, value := range values {
			sessionID, ok := value.(string)
			if !ok || !strings.HasPrefix(sessionID, "s") {
				continue
			}
			sessionModels = append(sessionModels,
				[]driver.Value{"model-" + sessionID},
			)
		}
		if len(sessionModels) == 0 {
			sessionModels = [][]driver.Value{
				{"model-s1"},
				{"model-s2"},
			}
		}
		return &analyticsProbeRows{
			columns: []string{"model"},
			values:  sessionModels,
		}, nil
	case strings.Contains(normalized, "from sessions"):
		if strings.Contains(normalized, "message_count, agent, project") {
			return &analyticsProbeRows{
				columns: []string{
					"id", "date", "message_count", "agent", "project",
					"total_output_tokens", "has_total_output_tokens",
				},
				values: [][]driver.Value{
					{
						"s1", time.Date(2024, 6, 3, 9, 0, 0, 0, time.UTC),
						int64(10), "claude", "alpha", int64(0), false,
					},
					{
						"s2", time.Date(2024, 6, 4, 9, 0, 0, 0, time.UTC),
						int64(20), "codex", "beta", int64(0), false,
					},
				},
			}, nil
		}
		if strings.Contains(normalized, "agent, project") {
			return &analyticsProbeRows{
				columns: []string{"id", "date", "agent", "project"},
				values: [][]driver.Value{
					{"s1", time.Date(2024, 6, 3, 9, 0, 0, 0, time.UTC), "claude", "alpha"},
					{"s2", time.Date(2024, 6, 4, 9, 0, 0, 0, time.UTC), "codex", "beta"},
				},
			}, nil
		}
		return &analyticsProbeRows{
			columns: []string{"id", "date", "agent"},
			values: [][]driver.Value{
				{"s1", time.Date(2024, 6, 3, 9, 0, 0, 0, time.UTC), "claude"},
				{"s2", time.Date(2024, 6, 4, 9, 0, 0, 0, time.UTC), "codex"},
			},
		}, nil
	case strings.Contains(normalized, "from tool_calls"):
		if strings.Contains(normalized, "trim(coalesce(tc.skill_name") {
			if !strings.Contains(normalized, "group by tc.session_id") ||
				!strings.Contains(normalized, "m.timestamp") {
				return nil, errors.New(
					"skill query must group by session, skill, and message timestamp")
			}
			if !strings.Contains(normalized, "left join messages") {
				return nil, errors.New("skill query must join messages")
			}
			if strings.Contains(normalized, "to_char(m.timestamp") {
				return nil, errors.New(
					"skill query must scan native message timestamps")
			}
			return &analyticsProbeRows{
				columns: []string{
					"session_id", "skill_name", "count", "last_used_at",
				},
				values: [][]driver.Value{
					{
						"s1", "review-code", int64(2),
						time.Date(2024, 6, 3, 12, 30, 0, 0, time.UTC),
					},
					{"s2", "review-code", int64(1), nil},
				},
			}, nil
		}
		if !strings.Contains(normalized,
			"trim(coalesce(tc.tool_name") {
			return nil, errors.New(
				"tool call query must project trimmed tool_name")
		}
		if !strings.Contains(normalized, "group by session_id, category") {
			if !strings.Contains(normalized,
				"group by tc.session_id, tc.category") {
				return nil, errors.New(
					"tool call query must group by session_id, category")
			}
		}
		if !strings.Contains(normalized,
			"trim(coalesce(tc.tool_name") {
			return nil, errors.New(
				"tool call query must group by tool_name")
		}
		if strings.Contains(normalized, "to_char(m.timestamp") {
			return &analyticsProbeRows{
				columns: []string{
					"session_id", "category", "tool_name", "count", "timestamp",
				},
				values: [][]driver.Value{
					{
						"s1", "Read", "Read", int64(2),
						"2024-06-03T09:00:00Z",
					},
					{
						"s1", "Bash", "Bash", int64(1),
						"2024-06-03T09:00:00Z",
					},
					{
						"s2", "Read", "Read", int64(1),
						"2024-06-04T09:00:00Z",
					},
				},
			}, nil
		}
		return &analyticsProbeRows{
			columns: []string{"session_id", "category", "tool_name", "count"},
			values: [][]driver.Value{
				{"s1", "Read", "Read", int64(2)},
				{"s1", "Bash", "Bash", int64(1)},
				{"s2", "Read", "Read", int64(1)},
			},
		}, nil
	case strings.Contains(normalized, "from messages"):
		if strings.Contains(normalized, "to_char") {
			return nil, errors.New("velocity query must scan native timestamps")
		}
		return &analyticsProbeRows{
			columns: []string{
				"session_id", "ordinal", "role",
				"timestamp", "content_length",
			},
			values: [][]driver.Value{
				{
					"s1", int64(0), "user",
					time.Date(2024, 6, 3, 9, 0, 0, 0, time.UTC),
					int64(2),
				},
				{
					"s1", int64(1), "assistant",
					time.Date(2024, 6, 3, 9, 0, 10, 0, time.UTC),
					int64(5),
				},
			},
		}, nil
	default:
		return nil, errors.New("unexpected analytics query")
	}
}

func (r *analyticsProbeRows) Columns() []string { return r.columns }

func (r *analyticsProbeRows) Close() error { return nil }

func (r *analyticsProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

func TestGetAnalyticsToolsAggregatesToolCallsInSQL(t *testing.T) {
	store := &Store{
		pg: newAnalyticsProbeDB(t, &analyticsProbeState{}),
	}

	resp, err := store.GetAnalyticsTools(
		context.Background(),
		db.AnalyticsFilter{
			From: "2024-06-01",
			To:   "2024-06-30",
		},
	)
	require.NoError(t, err, "GetAnalyticsTools")

	assert.Equal(t, 4, resp.TotalCalls)
	require.NotEmpty(t, resp.ByCategory)
	assert.Equal(t, "Read", resp.ByCategory[0].Category)
	assert.Equal(t, 3, resp.ByCategory[0].Count)
	require.NotEmpty(t, resp.ByTool)
	assert.Equal(t, "Read", resp.ByTool[0].ToolName)
	assert.Equal(t, 3, resp.ByTool[0].CallCount)
	assert.Equal(t, 2, resp.ByTool[0].SessionCount)
}

func TestGetAnalyticsSkillsAggregatesToolCallsInSQL(t *testing.T) {
	store := &Store{
		pg: newAnalyticsProbeDB(t, &analyticsProbeState{}),
	}

	resp, err := store.GetAnalyticsSkills(
		context.Background(),
		db.AnalyticsFilter{
			From: "2024-06-01",
			To:   "2024-06-30",
		},
	)
	require.NoError(t, err, "GetAnalyticsSkills")

	assert.Equal(t, 3, resp.TotalSkillCalls)
	assert.Equal(t, 1, resp.DistinctSkills)
	require.NotEmpty(t, resp.BySkill)
	assert.Equal(t, "review-code", resp.BySkill[0].SkillName)
	assert.Equal(t, 3, resp.BySkill[0].CallCount)
	assert.Equal(t, 2, resp.BySkill[0].SessionCount)
	assert.Equal(t, []db.SkillAgentBreakdown{
		{Agent: "claude", Count: 2},
		{Agent: "codex", Count: 1},
	}, resp.BySkill[0].AgentBreakdown)
	assert.Equal(t, []db.SkillProjectBreakdown{
		{Project: "alpha", Count: 2},
		{Project: "beta", Count: 1},
	}, resp.BySkill[0].ProjectBreakdown)
	assert.Equal(t, "2024-06-04T09:00:00Z", resp.BySkill[0].LastUsedAt,
		"LastUsedAt is the latest message timestamp, "+
			"with session fallback for null timestamps")
}

func TestGetAnalyticsToolsModelFilterJoinsMessages(t *testing.T) {
	state := &analyticsProbeState{}
	store := &Store{
		pg: newAnalyticsProbeDB(t, state),
	}

	_, err := store.GetAnalyticsTools(
		context.Background(),
		db.AnalyticsFilter{
			From:  "2024-06-01",
			To:    "2024-06-30",
			Model: "gpt-4o",
		},
	)
	require.NoError(t, err, "GetAnalyticsTools")

	state.mu.Lock()
	defer state.mu.Unlock()

	var toolQuery string
	for _, query := range state.queries {
		if strings.Contains(strings.ToLower(query), "from tool_calls tc") {
			toolQuery = query
			break
		}
	}
	require.NotEmpty(t, toolQuery, "tool query not captured")
	normalized := strings.Join(strings.Fields(strings.ToLower(toolQuery)), " ")
	assert.Contains(t, normalized,
		"join messages m on m.session_id = tc.session_id and m.ordinal = tc.message_ordinal")
	assert.Contains(t, normalized, "m.model = $")
}

func TestGetAnalyticsSkillsModelFilterUsesMatchingMessages(t *testing.T) {
	state := &analyticsProbeState{}
	store := &Store{
		pg: newAnalyticsProbeDB(t, state),
	}

	_, err := store.GetAnalyticsSkills(
		context.Background(),
		db.AnalyticsFilter{
			From:  "2024-06-01",
			To:    "2024-06-30",
			Model: "gpt-4o",
		},
	)
	require.NoError(t, err, "GetAnalyticsSkills")

	state.mu.Lock()
	defer state.mu.Unlock()

	var skillQuery string
	for _, query := range state.queries {
		q := strings.ToLower(query)
		if strings.Contains(q, "from tool_calls tc") &&
			strings.Contains(q, "trim(coalesce(tc.skill_name") {
			skillQuery = query
			break
		}
	}
	require.NotEmpty(t, skillQuery, "skill query not captured")
	normalized := strings.Join(strings.Fields(strings.ToLower(skillQuery)), " ")
	assert.Contains(t, normalized,
		"left join messages m on m.session_id = tc.session_id and m.ordinal = tc.message_ordinal")
	assert.Contains(t, normalized, "m.model = $")
}

func TestQueryVelocityMsgsScansNativeTimestamps(t *testing.T) {
	store := &Store{
		pg: newAnalyticsProbeDB(t, &analyticsProbeState{}),
	}
	sessionMsgs := map[string][]velocityMsg{}

	err := store.queryVelocityMsgs(
		context.Background(),
		[]string{"s1"},
		time.UTC,
		sessionMsgs,
	)
	require.NoError(t, err, "queryVelocityMsgs")

	require.Len(t, sessionMsgs["s1"], 2)
	assert.Equal(t, "assistant", sessionMsgs["s1"][1].role)
	assert.True(t, sessionMsgs["s1"][1].valid)
	assert.Equal(t, 10.0,
		sessionMsgs["s1"][1].ts.Sub(sessionMsgs["s1"][0].ts).Seconds())
}

func TestGetAnalyticsSummaryModelsFollowFilteredSessions(t *testing.T) {
	store := &Store{
		pg: newAnalyticsProbeDB(t, &analyticsProbeState{}),
	}

	resp, err := store.GetAnalyticsSummary(
		context.Background(),
		db.AnalyticsFilter{
			From:     "2024-06-03",
			To:       "2024-06-03",
			Timezone: "UTC",
		},
	)
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, []string{"model-s1"}, resp.Models)
}
