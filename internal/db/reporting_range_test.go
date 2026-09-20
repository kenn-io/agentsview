package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReportingRangeCoversDatedReportingSources(t *testing.T) {
	for _, tt := range []struct {
		name string
		seed string
		want string
	}{
		{name: "empty"},
		{
			name: "subagent activity",
			seed: `INSERT INTO sessions(id, project, machine, agent, message_count, relationship_type, started_at)
				VALUES('child', 'project-a', 'local', 'agent-a', 1, 'subagent', '2024-01-02T10:00:00Z')`,
			want: "2024-01-02",
		},
		{
			name: "old activity message",
			seed: `INSERT INTO sessions(id, project, machine, agent, message_count, started_at)
				VALUES('session-a', 'project-a', 'local', 'agent-a', 1, '2026-07-28T10:00:00Z');
				INSERT INTO messages(session_id, ordinal, role, content, timestamp)
				VALUES('session-a', 0, 'user', 'example', '2024-02-03T10:00:00Z')`,
			want: "2024-02-03",
		},
		{
			name: "old usage message without activity",
			seed: `INSERT INTO sessions(id, project, machine, agent, started_at)
				VALUES('session-a', 'project-a', 'local', 'agent-a', '2026-07-28T10:00:00Z');
				INSERT INTO messages(session_id, ordinal, role, content, timestamp, model, token_usage)
				VALUES('session-a', 0, 'assistant', '', '2024-03-04T10:00:00Z', 'model-a', '{"output_tokens":1}')`,
			want: "2024-03-04",
		},
		{
			name: "usage event without activity",
			seed: `INSERT INTO sessions(id, project, machine, agent, started_at)
				VALUES('session-a', 'project-a', 'local', 'agent-a', '2026-07-28T10:00:00Z');
				INSERT INTO usage_events(session_id, source, model, occurred_at, output_tokens)
				VALUES('session-a', 'fixture', 'model-a', '2024-04-05T10:00:00Z', 1)`,
			want: "2024-04-05",
		},
		{
			name: "usage fallback to session start",
			seed: `INSERT INTO sessions(id, project, machine, agent, started_at)
				VALUES('session-a', 'project-a', 'local', 'agent-a', '2024-05-06T10:00:00Z');
				INSERT INTO usage_events(session_id, source, model, output_tokens)
				VALUES('session-a', 'fixture', 'model-a', 1)`,
			want: "2024-05-06",
		},
		{
			name: "standalone usage ordered by UTC instant",
			seed: `INSERT INTO cursor_usage_events(occurred_at, model, output_tokens) VALUES
				('2024-06-07T23:30:00-02:00', 'model-a', 1),
				('2024-06-08T00:30:00+02:00', 'model-a', 1)`,
			want: "2024-06-07",
		},
		{
			name: "terminal tool execution",
			seed: `INSERT INTO sessions(id, project, machine, agent, message_count, started_at)
				VALUES('session-a', 'project-a', 'local', 'agent-a', 1, '2026-07-28T10:00:00Z');
				INSERT INTO tool_result_events(session_id, tool_call_message_ordinal, source, status, content, timestamp)
				VALUES('session-a', 0, 'tool_execution', 'completed', '', '2024-07-08T10:00:00Z')`,
			want: "2024-07-08",
		},
		{
			name: "untimed session creation fallback",
			seed: `INSERT INTO sessions(id, project, machine, agent, message_count, started_at, created_at)
				VALUES('session-a', 'project-a', 'local', 'agent-a', 1, '', '2024-08-09T10:00:00Z')`,
			want: "2024-08-09",
		},
		{
			name: "unreportable rows are empty",
			seed: `INSERT INTO sessions(id, project, machine, agent, message_count, started_at, deleted_at) VALUES
				('deleted', 'project-a', 'local', 'agent-a', 1, '2024-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
				('usage-only', 'project-a', 'local', 'agent-a', 0, '2024-01-01T00:00:00Z', NULL);
				INSERT INTO messages(session_id, ordinal, role, content, timestamp, model, token_usage) VALUES
				('deleted', 0, 'assistant', '', '2024-01-01T00:00:00Z', 'model-a', '{"output_tokens":1}'),
				('usage-only', 0, 'assistant', '', '2024-01-01T00:00:00Z', '<synthetic>', '{"output_tokens":1}');
				INSERT INTO usage_events(session_id, source, model, occurred_at) VALUES
				('deleted', 'fixture', 'model-a', '2024-01-01T00:00:00Z'),
				('usage-only', 'fixture', '', '2024-01-01T00:00:00Z');
				INSERT INTO cursor_usage_events(occurred_at, model) VALUES
				('2024-01-01T00:00:00Z', ''), ('malformed', 'model-a'),
				('2026-07-29T14:00:00Z', 'model-a')`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			if tt.seed != "" {
				_, err := d.getWriter().ExecContext(t.Context(), tt.seed)
				require.NoError(t, err)
			}
			got, err := d.ExportReportingRange(t.Context(), time.Date(2026, 7, 29, 9, 37, 0, 0, time.FixedZone("offset", -5*60*60)))
			require.NoError(t, err)
			assert.Equal(t, "2026-07-29T14:00:00Z", got.ClosedThrough)
			if tt.want == "" {
				assert.Nil(t, got.EarliestDate)
			} else {
				require.NotNil(t, got.EarliestDate)
				assert.Equal(t, tt.want, *got.EarliestDate)
			}
		})
	}
}

func TestReportingRangeUntimedEvidence(t *testing.T) {
	for _, source := range []struct {
		name         string
		messageCount int
		insert       string
	}{
		{
			name: "usage message",
			insert: `INSERT INTO messages(session_id, ordinal, role, content, timestamp, model, token_usage)
				VALUES('session-a', 0, 'assistant', '', ?, 'model-a', '{"output_tokens":1}')`,
		},
		{
			name: "usage event",
			insert: `INSERT INTO usage_events(session_id, source, model, occurred_at, output_tokens)
				VALUES('session-a', 'fixture', 'model-a', ?, 1)`,
		},
		{
			name:         "terminal tool execution",
			messageCount: 1,
			insert: `INSERT INTO tool_result_events(session_id, tool_call_message_ordinal, source, status, content, timestamp)
				VALUES('session-a', 0, 'tool_execution', 'completed', '', ?)`,
		},
	} {
		for _, timestamps := range []struct {
			name    string
			event   any
			started any
			want    string
		}{
			{name: "null timestamps", want: "2024-08-09"},
			{name: "empty timestamps", event: "", started: "", want: "2024-08-09"},
			{name: "session start takes precedence", event: "", started: "2024-09-10T10:00:00Z", want: "2024-09-10"},
		} {
			t.Run(source.name+"/"+timestamps.name, func(t *testing.T) {
				d := testDB(t)
				_, err := d.getWriter().ExecContext(t.Context(), `
					INSERT INTO sessions(id, project, machine, agent, message_count, started_at, created_at)
					VALUES('session-a', 'project-a', 'local', 'agent-a', ?, ?, '2024-08-09T10:00:00Z')`,
					source.messageCount, timestamps.started)
				require.NoError(t, err)
				_, err = d.getWriter().ExecContext(t.Context(), source.insert, timestamps.event)
				require.NoError(t, err)
				got, err := d.ExportReportingRange(t.Context(), time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC))
				require.NoError(t, err)
				require.NotNil(t, got.EarliestDate)
				assert.Equal(t, timestamps.want, *got.EarliestDate)
			})
		}
	}
}

func TestReportingRangeWithoutCursorUsageTable(t *testing.T) {
	d := testDB(t)
	_, err := d.getWriter().ExecContext(t.Context(), `
		DROP TABLE cursor_usage_events;
		INSERT INTO sessions(id, project, machine, agent, message_count, started_at)
		VALUES('session-a', 'project-a', 'local', 'agent-a', 1, '2024-01-02T10:00:00Z')`)
	require.NoError(t, err)
	path := d.Path()
	require.NoError(t, d.Close())
	readOnly, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readOnly.Close()) })
	got, err := readOnly.ExportReportingRange(t.Context(), time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC))
	require.NoError(t, err)
	require.NotNil(t, got.EarliestDate)
	assert.Equal(t, "2024-01-02", *got.EarliestDate)
	assert.Equal(t, "2026-07-29T14:00:00Z", got.ClosedThrough)
}

func BenchmarkReportingRange(b *testing.B) {
	for _, count := range []int{1000, 100000} {
		b.Run(fmt.Sprintf("messages_%d", count), func(b *testing.B) {
			d := testDB(b)
			_, err := d.getWriter().Exec(b.Context(), `INSERT INTO sessions(id, project, machine, agent, message_count, started_at)
				VALUES('session-a', 'project-a', 'local', 'agent-a', ?, '2024-01-02T00:00:00Z')`, count)
			require.NoError(b, err)
			_, err = d.getWriter().Exec(b.Context(), `WITH RECURSIVE n(i) AS (
				VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i + 1 < ?
			) INSERT INTO messages(session_id, ordinal, role, content, timestamp)
				SELECT 'session-a', i, 'assistant', '', '2024-01-02T10:00:00Z' FROM n`, count)
			require.NoError(b, err)
			now := time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got, err := d.ExportReportingRange(b.Context(), now)
				require.NoError(b, err)
				require.NotNil(b, got.EarliestDate)
				require.Equal(b, "2024-01-02", *got.EarliestDate)
			}
		})
	}
}
