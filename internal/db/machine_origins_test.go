package db

import (
	"context"
	"fmt"
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

// Readers page the change stream by revision. Republishing more sessions
// than one page holds must still deliver each of them exactly once.
func TestRemoteMachineOriginRepublishesAcrossChangeStreamPages(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, testLocalInstallation))
	_, err := d.EnsureConversationExportInitialized(ctx)
	require.NoError(t, err)
	const hostSessions = MaxSessionLimit + 7
	for i := range hostSessions {
		require.NoError(t, d.UpsertSession(ctx, Session{
			ID: fmt.Sprintf("devbox~%04d", i), Project: "project", Machine: "devbox", Agent: "claude",
		}))
	}
	var cache ChangeStreamUsageCache
	cursor := ""
	for {
		batch, err := d.ReadChangeStream(ctx, cursor, &cache)
		require.NoError(t, err)
		cursor = batch.To
		if !batch.More {
			break
		}
	}

	require.NoError(t, d.RecordRemoteMachineOrigin(ctx, "devbox", testRemoteInstallation))

	delivered := map[string]int{}
	for {
		batch, err := d.ReadChangeStream(ctx, cursor, &cache)
		require.NoError(t, err)
		for _, session := range batch.Sessions {
			delivered[session.SessionID]++
			require.NotNil(t, session.MachineID)
			assert.Equal(t, testRemoteInstallation, *session.MachineID)
		}
		cursor = batch.To
		if !batch.More {
			break
		}
	}
	assert.Len(t, delivered, hostSessions)
	for id, count := range delivered {
		assert.Equal(t, 1, count, id)
	}
}

// A full resync or archive rebuild replaces the database. The recorded
// origins must survive it, or the host's activity reverts to its host name
// until the host can be synced again.
func TestRemoteMachineOriginSurvivesDatabaseReplacement(t *testing.T) {
	tests := []struct {
		name string
		copy func(context.Context, *DB, string) error
	}{
		{"full resync", func(ctx context.Context, d *DB, source string) error { return d.CopySyncStateFrom(ctx, source) }},
		{"archive rebuild", func(ctx context.Context, d *DB, source string) error {
			return d.CopyAttachedArchiveStateFrom(ctx, source)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			source := testDB(t)
			require.NoError(t, source.ConfigureArtifactLocalMachine(ctx, testLocalInstallation))
			require.NoError(t, source.RecordRemoteMachineOrigin(ctx, "devbox", testRemoteInstallation))

			replacement := testDB(t)
			require.NoError(t, tt.copy(ctx, replacement, source.Path()))
			_, err := replacement.rawWriter().ExecContext(ctx, `
				INSERT INTO sessions(id,project,machine,agent,message_count,started_at,ended_at) VALUES
				('devbox~a','project','devbox','claude',2,'2026-07-28T10:00:00Z','2026-07-28T10:02:00Z');
				INSERT INTO messages(session_id,ordinal,role,content,timestamp,model) VALUES
				('devbox~a',0,'user','start','2026-07-28T10:00:00Z',''),
				('devbox~a',1,'assistant','end','2026-07-28T10:02:00Z','model-a')`)
			require.NoError(t, err)

			day, err := replacement.ExportReportingDay(ctx, ReportingExportOptions{
				Date:          time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
				Now:           time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
				SchemaVersion: 5,
				ByMachine:     true,
			})
			require.NoError(t, err)
			assert.Contains(t, day.Hours[10].Machines, testRemoteInstallation)
			assert.NotContains(t, day.Hours[10].Machines, "devbox")
		})
	}
}

// Two computers that sync the same Codex cloud login hold the same cloud
// chats. Schema 7 reports them under the login's account key on both, so a
// consumer that keeps one copy per origin counts each chat once, while each
// computer's own sessions stay under its installation.
func TestCodexCloudSessionsReportUnderAccount(t *testing.T) {
	const origin = "codex-cloud-account:0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	const keyed = "/data/sources/codex-cloud/0f1e2d3c4b5a69788796a5b4c3d2e1f0/thread-1.jsonl.zstd"
	tests := []struct {
		name   string
		schema int
		path   string
		// wantAccount reports whether the cloud chat leaves the machine part.
		wantAccount bool
	}{
		{"schema 7 names the account", 7, keyed, true},
		{"path without an account key", 7, "/data/sources/codex-cloud/thread-1.jsonl.zstd", false},
		{"released schema 5 keeps the machine", 5, keyed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var accountDigests []string
			for _, host := range []string{testLocalInstallation, testRemoteInstallation} {
				d := testDB(t)
				ctx := t.Context()
				require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, host))
				_, err := d.rawWriter().ExecContext(ctx, `
					INSERT INTO sessions(id,project,machine,agent,message_count,started_at,ended_at,file_path) VALUES
					('own','project','local','claude',2,'2026-07-28T10:00:00Z','2026-07-28T10:02:00Z',NULL),
					('codex-cloud:thread-1','project','local','codex-cloud',2,'2026-07-28T10:00:00Z','2026-07-28T10:03:00Z',?)`,
					tt.path)
				require.NoError(t, err)
				insertMessages(t, d,
					Message{SessionID: "own", Ordinal: 0, Role: "user", Content: "start", Timestamp: "2026-07-28T10:00:00Z"},
					Message{SessionID: "own", Ordinal: 1, Role: "assistant", Content: "end", Timestamp: "2026-07-28T10:02:00Z", Model: "model-a", TokenUsage: []byte(`{"output_tokens":5}`)},
					Message{SessionID: "codex-cloud:thread-1", Ordinal: 0, Role: "user", Content: "start", Timestamp: "2026-07-28T10:00:00Z"},
					Message{SessionID: "codex-cloud:thread-1", Ordinal: 1, Role: "assistant", Content: "end", Timestamp: "2026-07-28T10:03:00Z", Model: "model-a", TokenUsage: []byte(`{"output_tokens":7}`)},
				)

				day, err := d.ExportReportingDay(ctx, ReportingExportOptions{
					Date:          time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
					Now:           time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
					SchemaVersion: tt.schema,
					ByMachine:     true,
				})
				require.NoError(t, err)
				hour := day.Hours[10]
				assert.InDelta(t, 5, hour.Activity.Totals.AgentMinutes, 0.001)
				if !tt.wantAccount {
					assert.NotContains(t, hour.Machines, origin)
					assert.InDelta(t, 5, hour.Machines[host].Activity.Totals.AgentMinutes, 0.001)
					assert.EqualValues(t, 12, hour.Machines[host].Usage.Totals.OutputTokens)
					continue
				}
				assert.InDelta(t, 2, hour.Machines[host].Activity.Totals.AgentMinutes, 0.001)
				assert.EqualValues(t, 5, hour.Machines[host].Usage.Totals.OutputTokens)
				require.Contains(t, hour.Machines, origin)
				assert.InDelta(t, 3, hour.Machines[origin].Activity.Totals.AgentMinutes, 0.001)
				assert.EqualValues(t, 7, hour.Machines[origin].Usage.Totals.OutputTokens)
				accountDigests = append(accountDigests, hour.Machines[origin].Digest)
			}
			if tt.wantAccount {
				require.Len(t, accountDigests, 2)
				assert.Equal(t, accountDigests[0], accountDigests[1])
			}
		})
	}
}

// Schema 7 reserves the account prefix, so a machine whose own ID starts
// with it cannot report under a Codex cloud account's key.
func TestCodexCloudAccountPrefixReservedForAccounts(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, testLocalInstallation))
	_, err := d.rawWriter().ExecContext(ctx, `
		INSERT INTO sessions(id,project,machine,agent,message_count,started_at,ended_at) VALUES
		('spoof','project','codex-cloud-account:0f1e2d3c4b5a69788796a5b4c3d2e1f0','claude',2,'2026-07-28T10:00:00Z','2026-07-28T10:02:00Z');
		INSERT INTO messages(session_id,ordinal,role,content,timestamp,model) VALUES
		('spoof',0,'user','start','2026-07-28T10:00:00Z',''),
		('spoof',1,'assistant','end','2026-07-28T10:02:00Z','model-a')`)
	require.NoError(t, err)
	day, err := d.ExportReportingDay(ctx, ReportingExportOptions{
		Date:          time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:           time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
		SchemaVersion: 7,
		ByMachine:     true,
	})
	require.NoError(t, err)
	hour := day.Hours[10]
	assert.NotContains(t, hour.Machines, "codex-cloud-account:0f1e2d3c4b5a69788796a5b4c3d2e1f0")
	const hashed = "machine:34e89babd9c2c4024b131bf70f880ed0500f62e7cca9f5e53e332b4d090a9f10"
	require.Contains(t, hour.Machines, hashed)
	assert.InDelta(t, 2, hour.Machines[hashed].Activity.Totals.AgentMinutes, 0.001)
}

func TestCodexCloudCopiesMatchBetweenDayAndDigest(t *testing.T) {
	const imported = testRemoteInstallation + "~codex-cloud:thread-1"
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, testLocalInstallation))
	// The other computer watched the chat's first morning and captured its
	// usage. This computer's copy also holds the next day's turn, with a
	// smaller captured total. A one-day export of the second day loads only
	// this computer's copy, so it must still choose the copies that a digest
	// of both days chooses.
	_, err := d.rawWriter().ExecContext(ctx, `
		INSERT INTO sessions(id,project,machine,agent,message_count,started_at,ended_at,file_path) VALUES
		('codex-cloud:thread-1','project','local','codex-cloud',3,'2026-07-28T05:00:00Z','2026-07-29T00:10:00Z',
			'/data/sources/codex-cloud/0f1e2d3c4b5a69788796a5b4c3d2e1f0/thread-1.jsonl.zstd'),
		(?,'project',?,'codex-cloud',2,'2026-07-28T05:00:00Z','2026-07-28T05:05:00Z',NULL)`,
		imported, testRemoteInstallation)
	require.NoError(t, err)
	_, err = d.rawWriter().ExecContext(ctx,
		`INSERT INTO codex_cloud_accounts(session_id, account_key) VALUES (?, '0f1e2d3c4b5a69788796a5b4c3d2e1f0')`, imported)
	require.NoError(t, err)
	insertMessages(t, d,
		Message{SessionID: "codex-cloud:thread-1", Ordinal: 0, Role: "user", Content: "start", Timestamp: "2026-07-28T05:00:00Z"},
		Message{SessionID: "codex-cloud:thread-1", Ordinal: 1, Role: "assistant", Content: "first", Timestamp: "2026-07-28T05:05:00Z", Model: "gpt-5-codex"},
		Message{SessionID: "codex-cloud:thread-1", Ordinal: 2, Role: "assistant", Content: "next", Timestamp: "2026-07-29T00:10:00Z", Model: "gpt-5-codex"},
		Message{SessionID: imported, Ordinal: 0, Role: "user", Content: "start", Timestamp: "2026-07-28T05:00:00Z"},
		Message{SessionID: imported, Ordinal: 1, Role: "assistant", Content: "first", Timestamp: "2026-07-28T05:05:00Z", Model: "gpt-5-codex"},
	)
	usage := func(session, key, at string, input, output int) UsageEvent {
		return UsageEvent{
			SessionID: session, Source: "codex-cloud", Model: "gpt-5-codex",
			InputTokens: input, OutputTokens: output, OccurredAt: at, DedupKey: key,
		}
	}
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, imported, []UsageEvent{
		usage(imported, "a", "2026-07-28T05:01:00Z", 100, 20),
		usage(imported, "b", "2026-07-28T05:04:00Z", 100, 20),
	}))
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "codex-cloud:thread-1", []UsageEvent{
		usage("codex-cloud:thread-1", "c", "2026-07-29T00:10:00Z", 150, 30),
	}))

	from := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	digests, err := d.ExportReportingDigest(ctx, ReportingDigestExportOptions{
		From: from, To: from.Add(24 * time.Hour), Now: now, SchemaVersion: 7, ByMachine: true,
	})
	require.NoError(t, err)
	require.Len(t, digests, 2)
	for i, digest := range digests {
		day, err := d.ExportReportingDay(ctx, ReportingExportOptions{
			Date: from.Add(time.Duration(i) * 24 * time.Hour), Now: now, SchemaVersion: 7, ByMachine: true,
		})
		require.NoError(t, err)
		assert.Equal(t, reportingDigestDayFromDay(day), digest)
	}
}
