//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawparity"
	"go.kenn.io/agentsview/internal/storage"
)

// Each raw-capable provider crosses the real push/adoption/publication boundary.
// Parsed fixtures isolate parity from capture format parsing (covered separately
// by raw_derive_pipeline tests); legacy content is built independently.
func TestRawParityAdoptedPushProviders(t *testing.T) {
	for _, factory := range parser.ProviderFactories() {
		if factory.Capabilities().RawCapture.Support != parser.CapabilitySupported {
			continue
		}
		agent := factory.Definition().Type
		t.Run(string(agent), func(t *testing.T) {
			id := string(agent) + ":portable"
			local := testDB(t)
			require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: id, Agent: string(agent), Project: "synthetic", Machine: "archive", StartedAt: new("2026-01-01T00:00:00Z"), MessageCount: 1, UserMessageCount: 1}))
			require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5, Timestamp: "2026-01-01T00:00:00Z"}}))
			f := newProjectionFixture(t, func(pg *sql.DB) {
				var schema string
				require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
				syncer, err := New(testPGURL(t), schema, local, "archive", true, storage.PusherOptions{})
				require.NoError(t, err)
				defer func() { require.NoError(t, syncer.Close()) }()
				result, err := syncer.Push(t.Context(), true, nil)
				require.NoError(t, err)
				require.Zero(t, result.Errors)
			})
			out := projectionOutcome("hello")
			r := &out.Outcome.Results[0].Result
			r.Session.ID, r.Session.Agent = id, agent
			r.Messages, r.UsageEvents = r.Messages[:1], nil
			r.Messages[0].Timestamp, r.Messages[0].ContentLength = r.Session.StartedAt, len(r.Messages[0].Content)
			m, _ := f.accept(t, "device-a", "capture", "", agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
			require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
			got, err := rawparity.Run(t.Context(), RawParityReader{Tx: parityTx(t, f.runtime), Version: "parser-1"}, rawparity.Scope{})
			require.NoError(t, err)
			class := "equal"
			if agent == parser.AgentPiebald {
				class = "unmatchable"
			}
			assert.Equal(t, 1, got.Counts[class])
			assert.Equal(t, agent != parser.AgentPiebald, got.Pass)
			// Curation stays masked, and the complete report remains snapshot-bound.
			_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET deleted_at=clock_timestamp() WHERE provenance_kind='legacy'`)
			require.NoError(t, err)
			got, err = rawparity.Run(t.Context(), RawParityReader{Tx: parityTx(t, f.runtime), Version: "parser-1"}, rawparity.Scope{Device: "device-a", Provider: string(agent)})
			require.NoError(t, err)
			assert.Equal(t, 1, got.Counts[class])
			_, err = f.runtime.ExecContext(t.Context(), `UPDATE messages SET content='changed' WHERE session_id=$1`, id)
			require.NoError(t, err)
			got, err = rawparity.Run(t.Context(), RawParityReader{Tx: parityTx(t, f.runtime), Version: "parser-1"}, rawparity.Scope{})
			require.NoError(t, err)
			if agent != parser.AgentPiebald {
				require.Len(t, got.Sessions, 1)
				assert.Equal(t, []string{"messages"}, got.Sessions[0].Differing)
				assert.False(t, got.Pass)
			}
		})
	}
}

func TestRawParityReaderRejectsUnsafeTransaction(t *testing.T) {
	_, err := rawparity.Run(t.Context(), RawParityReader{Version: "parser-1"}, rawparity.Scope{})
	assert.ErrorContains(t, err, "REPEATABLE READ READ ONLY")
}

// Exercise the database/sql transport, including empty inventories on legacy
// rows. Identity strings are opaque: JSON punctuation must survive unchanged.
func TestRawParityCandidateArrayTransport(t *testing.T) {
	f := newProjectionFixture(t)
	alias := "codex:quotes\"slashes\\,braces{}-日本語"
	out := projectionOutcome("transport")
	out.Outcome.Results[0].Result.Session.ID = alias
	m, _ := f.accept(t, "device-b", "transport", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
	second, _ := f.accept(t, "device-a", "same-cohort", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, second), second, out))
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,agent,project,machine) VALUES ('legacy-empty','codex','synthetic','archive')`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	rows, err := (RawParityReader{Tx: parityTx(t, f.runtime), Version: "parser-1"}).Candidates(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		if row.ID == "legacy-empty" {
			assert.Empty(t, row.Aliases)
			assert.Empty(t, row.Devices)
		} else {
			assert.Equal(t, []string{alias}, row.Aliases)
			assert.ElementsMatch(t, []string{"device-a", "device-b"}, row.Devices)
		}
	}
}
