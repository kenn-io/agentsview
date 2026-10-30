package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMachineMembershipFiltersDoNotDuplicateSessions(t *testing.T) {
	d := testDB(t)
	for _, session := range []Session{
		{ID: "shared", Machine: "hosted", Agent: "claude", MessageCount: 1, UserMessageCount: 2},
		{ID: "legacy", Machine: "a", Agent: "claude", MessageCount: 1, UserMessageCount: 2},
		{ID: "other", Machine: "hosted", Agent: "claude", MessageCount: 1, UserMessageCount: 2},
	} {
		require.NoError(t, d.UpsertSession(t.Context(), session))
	}
	_, err := d.getWriter().Exec(t.Context(), `CREATE TABLE machine_test_sources(session_id TEXT, machine TEXT);
		INSERT INTO machine_test_sources VALUES ('shared','a'),('shared','b'),('other','c')`)
	require.NoError(t, err)

	dialect := SQLiteQueryDialect().WithMachinePredicate(func(id, machine string, placeholders []string) string {
		if id == "id" {
			id = "s.id"
		}
		if machine == "machine" {
			machine = "s.machine"
		}
		values := "(" + strings.Join(placeholders, ",") + ")"
		return "EXISTS (SELECT 1 FROM (SELECT " + machine + " AS source_machine UNION ALL SELECT src.machine FROM machine_test_sources src WHERE src.session_id=" + id + ") member WHERE member.source_machine IN " + values + ")"
	})
	builders := []struct {
		name  string
		build func(string) (string, []any)
	}{
		{"sessions", func(machine string) (string, []any) {
			return BuildSessionBaseFilterSQL(SessionFilter{Machine: machine}, dialect)
		}},
		{"analytics", func(machine string) (string, []any) {
			b := NewQueryBuilder(dialect, 0)
			where := BuildAnalyticsWhere(AnalyticsFilter{Machine: machine}, b, "s.", "s.id", nil)
			return where, b.Args()
		}},
		{"usage", func(machine string) (string, []any) {
			b := NewQueryBuilder(dialect, 0)
			where := strings.Join(BuildUsageSessionFilter(UsageFilter{Machine: machine}, b, ""), " AND ")
			return where, b.Args()
		}},
	}
	for _, builder := range builders {
		t.Run(builder.name, func(t *testing.T) {
			for _, tc := range []struct {
				machines string
				want     int
			}{{"a", 2}, {"b", 1}, {"a,b", 2}, {"c", 1}, {"missing", 0}} {
				t.Run(tc.machines, func(t *testing.T) {
					where, args := builder.build(tc.machines)
					var count int
					err := d.getReader().QueryRow(t.Context(), "SELECT COUNT(*) FROM sessions s WHERE "+where, args...).Scan(&count)
					require.NoError(t, err)
					assert.Equal(t, tc.want, count)
				})
			}
		})
	}
}

func TestMachinePredicatePreservesParameterOffset(t *testing.T) {
	dialect := PostgresQueryDialect().WithMachinePredicate(func(id, machine string, placeholders []string) string {
		assert.Equal(t, "s.id", id)
		assert.Equal(t, "s.machine", machine)
		assert.Equal(t, []string{"$4", "$5"}, placeholders)
		return "EXISTS (SELECT 1 WHERE " + machine + " IN (" + strings.Join(placeholders, ",") + "))"
	})
	b := NewQueryBuilder(dialect, 3)
	assert.NotEmpty(t, b.MachinePredicate("s.id", "s.machine", []string{"a", "b"}))
	assert.Equal(t, []any{"a", "b"}, b.Args())

	empty := NewQueryBuilder(dialect, 0)
	assert.Equal(t, "1 = 0", empty.MachinePredicate("s.id", "s.machine", nil))
	assert.Empty(t, empty.Args())
}
