package db

import (
	"context"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFullContentCommitsSignalStateOnce(t *testing.T) {
	for _, mode := range []string{"replace", "bulk", "staged"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			d := testDB(t)
			insertSession(t, d, "s1", "project-a")
			msgs := []Message{{SessionID: "s1", Ordinal: 0, Role: "assistant", Content: "first"}}
			update := SessionSignalUpdate{FullState: &SessionSignalState{State: []byte("first-state"), SignalVersion: CurrentQualitySignalVersion}}
			write := func() error {
				switch mode {
				case "bulk":
					result, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: Session{ID: "s1", Project: "project-a", Agent: "codex", MessageCount: 1}, Messages: msgs, Signals: update, ReplaceMessages: true}})
					if err == nil {
						require.Empty(result.Errors)
						require.Equal(1, result.WrittenSessions)
					}
					return err
				case "staged":
					scratch := newScratchStagedResults(t)
					return d.ReplaceSessionContentStaged(t.Context(), "s1", msgs, scratch, nil, func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) { return update, nil, nil })
				default:
					return d.ReplaceSessionContent("s1", msgs, update, nil)
				}
			}
			commits := 0
			conn, err := d.getWriter().Conn(t.Context())
			require.NoError(err)
			require.NoError(conn.Raw(func(raw any) error {
				raw.(*sqlite3.SQLiteConn).RegisterCommitHook(func() int { commits++; return 0 })
				return nil
			}))
			require.NoError(conn.Close())
			t.Cleanup(func() {
				conn, err := d.getWriter().Conn(context.WithoutCancel(t.Context()))
				require.NoError(err)
				require.NoError(conn.Raw(func(raw any) error { raw.(*sqlite3.SQLiteConn).RegisterCommitHook(nil); return nil }))
				require.NoError(conn.Close())
			})
			for _, content := range []string{"first", "changed", "changed"} {
				msgs[0].Content = content
				update.FullState.State = []byte(content + "-state")
				before := commits
				require.NoError(write())
				assert.Equal(1, commits-before, "content and seed must share one commit")
				state, ok, err := d.GetSessionSignalState("s1")
				require.NoError(err)
				require.True(ok)
				revision, err := d.TranscriptRevision("s1")
				require.NoError(err)
				assert.Equal(revision, state.TranscriptRevision)
				assert.Equal([]byte(content+"-state"), state.State)
			}
			// A failed seed must roll back the content too.
			_, err = d.getWriter().Exec(`CREATE TRIGGER reject_signal_seed BEFORE INSERT ON session_signal_state BEGIN SELECT RAISE(ABORT, 'reject seed'); END`)
			require.NoError(err)
			msgs[0].Content = "must roll back"
			if mode == "bulk" {
				result, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: Session{ID: "s1", Project: "project-a", Agent: "codex"}, Messages: msgs, Signals: update, ReplaceMessages: true}})
				require.NoError(err)
				require.Equal(1, result.FailedSessions)
			} else {
				require.Error(write())
			}
			stored, err := d.GetAllMessages(t.Context(), "s1")
			require.NoError(err)
			require.Len(stored, 1)
			assert.Equal("changed", stored[0].Content)
		})
	}
}
