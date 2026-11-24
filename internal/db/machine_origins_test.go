package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testLocalInstallation  = "0123456789abcdef0123456789abcdef"
	testRemoteInstallation = "fedcba9876543210fedcba9876543210"
)

// A remote host's own archive reports its sessions under its installation
// identity. The collector must name the same origin, or a consumer that keeps
// one copy per originating machine counts the host's activity twice.
func TestRemoteMachineOriginNamesReportingSnapshots(t *testing.T) {
	tests := []struct {
		name     string
		reported string
		want     string
	}{
		{"recorded installation identity", testRemoteInstallation, testRemoteInstallation},
		{"older remote without identity", "", "devbox"},
		{"malformed identity", "devbox", "devbox"},
		{"this archive's own identity", testLocalInstallation, "devbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, testLocalInstallation))
			_, err := d.rawWriter().ExecContext(ctx, `
				INSERT INTO sessions(id,project,machine,agent,message_count,started_at,ended_at) VALUES
				('devbox~a','project','devbox','claude',2,'2026-07-28T10:00:00Z','2026-07-28T10:02:00Z');
				INSERT INTO messages(session_id,ordinal,role,content,timestamp,model) VALUES
				('devbox~a',0,'user','start','2026-07-28T10:00:00Z',''),
				('devbox~a',1,'assistant','end','2026-07-28T10:02:00Z','model-a')`)
			require.NoError(t, err)

			require.NoError(t, d.RecordRemoteMachineOrigin(ctx, "devbox", tt.reported))

			day, err := d.ExportReportingDay(ctx, ReportingExportOptions{
				Date:          time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
				Now:           time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
				SchemaVersion: 5,
				ByMachine:     true,
			})
			require.NoError(t, err)
			hour := day.Hours[10]
			assert.Equal(t, testLocalInstallation, hour.LocalMachineID)
			require.Contains(t, hour.Machines, tt.want)
			assert.InDelta(t, 2, hour.Machines[tt.want].Activity.Totals.AgentMinutes, 0.001)
		})
	}
}

// The change stream names each session's origin, so recording or changing a
// host's origin publishes that host's sessions again. An unchanged record
// publishes nothing.
func TestRemoteMachineOriginRepublishesHostSessions(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, testLocalInstallation))
	_, err := d.rawWriter().ExecContext(ctx, `
		INSERT INTO sessions(id,project,machine,agent,message_count,started_at) VALUES
		('devbox~a','project','devbox','claude',1,'2026-07-28T10:00:00Z'),
		('other~b','project','other','claude',1,'2026-07-28T10:00:00Z')`)
	require.NoError(t, err)
	revision := func(id string) int64 {
		var rev int64
		err := d.Reader().QueryRowContext(ctx,
			`SELECT COALESCE((SELECT revision FROM conversation_session_changes WHERE session_id=?),0)`, id).Scan(&rev)
		require.NoError(t, err)
		return rev
	}

	require.NoError(t, d.RecordRemoteMachineOrigin(ctx, "devbox", testRemoteInstallation))
	first := revision("devbox~a")
	assert.Positive(t, first)
	assert.Zero(t, revision("other~b"))

	require.NoError(t, d.RecordRemoteMachineOrigin(ctx, "devbox", testRemoteInstallation))
	assert.Equal(t, first, revision("devbox~a"))

	require.NoError(t, d.RecordRemoteMachineOrigin(ctx, "devbox", "00000000000000000000000000000001"))
	assert.Greater(t, revision("devbox~a"), first)
}
