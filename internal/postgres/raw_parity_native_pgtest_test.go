//go:build pgtest

package postgres

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawparity"
	"go.kenn.io/agentsview/internal/rawtest"
	"go.kenn.io/agentsview/internal/storage"
)

// Retained pushed rows predate explicit SourceSessionID evidence. Real parser
// output must still pair them with native raw captures without guessing from
// filenames, and duplicate captures on another device must share one group.
func TestRawParityNativeParserWithRetainedPush(t *testing.T) {
	const native = "11111111-1111-4111-8111-111111111111"
	for _, agent := range []parser.AgentType{parser.AgentCodex, parser.AgentEvener} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			var path, body string
			if agent == parser.AgentCodex {
				path = filepath.Join(root, "rollout-2026-01-01T00-00-00-"+native+".jsonl")
				body = `{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"` + native + `"}}` + "\n" +
					`{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
			} else {
				path = filepath.Join(root, "sessions", native+".transcript.jsonl")
				body = `{"kind":"header","format_version":2,"session_id":"` + native + `","created_at":"2026-01-01T00:00:00Z"}` + "\n" +
					`{"kind":"entry","seq":0,"turn":{"kind":"USER_INPUT","timestamp":"2026-01-01T00:00:01Z","message":{"content":[{"kind":"text","text":"hello"}]}}}` + "\n"
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			local, engine := rawtest.Oracle(t, agent, root, "", "")
			require.Equal(t, 1, rawtest.SyncOracle(t, engine))
			// This is the metadata retained by older parser output.
			require.NoError(t, local.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET source_session_id=''`)
				return err
			}))
			f := newProjectionFixture(t, func(pg *sql.DB) {
				var schema string
				require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
				pusher, err := New(testPGURL(t), schema, local, "capture-device", true, storage.PusherOptions{})
				require.NoError(t, err)
				defer func() { require.NoError(t, pusher.Close()) }()
				result, err := pusher.Push(t.Context(), true, nil)
				require.NoError(t, err)
				require.Zero(t, result.Errors)
			})
			provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}, Machine: "capture-device"})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0], ForceParse: true})
			require.NoError(t, err)
			for _, device := range []string{"device-a", "device-b"} {
				m, _ := f.accept(t, device, "native", "", agent)
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, rawderive.ParsedManifest{Outcome: parsed}))
			}
			got, err := rawparity.Run(t.Context(), RawParityReader{Tx: parityTx(t, f.runtime), Version: "parser-1"}, rawparity.Scope{})
			require.NoError(t, err)
			assert.True(t, got.Pass)
			assert.Equal(t, 1, got.Counts["equal"])
			assert.Zero(t, got.Counts["unmatchable"])
			var groups int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(DISTINCT group_id) FROM raw_session_branches WHERE active`).Scan(&groups))
			assert.Equal(t, 1, groups)
		})
	}
}
