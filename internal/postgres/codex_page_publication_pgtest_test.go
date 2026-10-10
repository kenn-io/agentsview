//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestCodexPagePublicationDuringThreadPurge(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			for _, first := range []string{"purge", "push"} {
				t.Run(fmt.Sprintf("hosted=%v/empty=%v/first=%s", hosted, empty, first), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					var workers sync.WaitGroup
					defer workers.Wait()
					defer cancel()
					local, syncer, store := newCodexTrashMirror(t)
					observer, writer := store.pg, store.pg
					var schema, dsn, tenant string
					dsn = testPGURL(t)
					require.NoError(t, observer.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema))
					if hosted {
						f := newHostedFixture(t, "tenant-page-publication")
						observer, writer, schema, dsn, tenant = f.admin, f.runtime, f.schema, f.dsn, f.tenant
						syncer.pg, syncer.schema = writer, schema
						_, err := syncer.Push(ctx, true, nil)
						require.NoError(t, err)
					}
					// Retirement retains this fileless version-130 thread as the
					// scope anchor. The returning page does not yet exist remotely.
					_, err := writer.ExecContext(ctx, `UPDATE sessions SET data_version=130 WHERE id=$1`, codexTrashThread)
					require.NoError(t, err)
					_, err = local.RestoreSession(ctx, codexTrashThread)
					require.NoError(t, err)
					require.NoError(t, local.UpsertSession(ctx, db.Session{ID: codexTrashReturning, Agent: "codex", Project: "returning", Machine: "machine", ParentSessionID: new(codexTrashThread), RelationshipType: "continuation"}))
					require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: codexTrashReturning, Role: "user", Content: "Returning page"}}))
					page, err := (&Store{pg: writer}).GetSessionFull(ctx, codexTrashReturning)
					require.NoError(t, err)
					require.Nil(t, page, "the page must be absent before the competing push")
					// A project-filtered push need not republish the thread row.
					syncer.projects = []string{"returning"}
					var endpoint interface {
						DeleteSessionIfTrashed(context.Context, string) (int64, error)
						EmptyTrash(context.Context) (int, error)
					}
					for _, op := range []string{"purge", "push"} {
						opDSN, err := appendConnParams(dsn, map[string]string{"application_name": "codex_publication_" + op})
						require.NoError(t, err)
						var pg *sql.DB
						if hosted {
							pg, err = OpenHosted(opDSN, schema, tenant, false)
						} else {
							pg, err = Open(opDSN, schema, true)
						}
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, pg.Close()) })
						if op == "push" {
							syncer.pg = pg
						} else if hosted {
							endpoint, err = newHostedAdapter(pg, tenant)
							require.NoError(t, err)
						} else {
							endpoint = &Store{pg: pg}
						}
					}
					gate, err := writer.BeginTx(ctx, nil)
					require.NoError(t, err)
					defer func() { _ = gate.Rollback() }()
					gateTable, gateQuery, second := "excluded_sessions", "%INSERT INTO excluded_sessions%", "push"
					if first == "push" {
						gateTable, gateQuery, second = "messages", "%DELETE FROM messages%", "purge"
					}
					_, err = gate.ExecContext(ctx, `LOCK TABLE `+gateTable+` IN SHARE MODE`)
					require.NoError(t, err)
					purged := make(chan error, 1)
					type pushResult struct {
						result storage.PushResult
						err    error
					}
					pushed := make(chan pushResult, 1)
					operations := map[string]func(){
						"push": func() { result, err := syncer.Push(ctx, false, nil); pushed <- pushResult{result, err} },
						"purge": func() {
							var err error
							if empty {
								_, err = endpoint.EmptyTrash(ctx)
							} else {
								_, err = endpoint.DeleteSessionIfTrashed(ctx, codexTrashThread)
							}
							purged <- err
						},
					}
					workers.Go(operations[first])
					// Hold purge after its page inventory, or push after its session
					// insert and before its message writes, then start the other API.
					require.Eventually(t, func() bool {
						var blocked bool
						err := observer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE $2)`, "codex_publication_"+first, gateQuery).Scan(&blocked)
						return err == nil && blocked
					}, 10*time.Second, 20*time.Millisecond)
					workers.Go(operations[second])
					// The original writer could finish while purge remained open.
					require.Eventually(t, func() bool {
						if len(pushed) > 0 || len(purged) > 0 {
							return true
						}
						var blocked bool
						err := observer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0)`, "codex_publication_"+second).Scan(&blocked)
						return err == nil && blocked
					}, 10*time.Second, 20*time.Millisecond)
					require.NoError(t, gate.Commit())
					require.NoError(t, <-purged)
					push := <-pushed
					require.NoError(t, push.err)
					require.Zero(t, push.result.Errors)
					if first == "push" {
						assert.Equal(t, 1, push.result.SessionsPushed, "purge must see the page committed by the earlier push")
					} else {
						assert.Zero(t, push.result.SessionsPushed, "push must observe the earlier purge's exclusion")
					}
					page, err = (&Store{pg: writer}).GetSessionFull(ctx, codexTrashReturning)
					require.NoError(t, err)
					assert.Nil(t, page, "a page published during a committed whole-thread purge must not survive")
					var messages int
					require.NoError(t, writer.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE session_id=$1`, codexTrashReturning).Scan(&messages))
					assert.Zero(t, messages)
				})
			}
		}
	}
}
