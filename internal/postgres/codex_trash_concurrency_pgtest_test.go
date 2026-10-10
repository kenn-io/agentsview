//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestCodexTrashConcurrentRestoreAndPurge(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			for _, first := range []string{"purge", "restore"} {
				t.Run(fmt.Sprintf("hosted=%v/empty=%v/first=%s", hosted, empty, first), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
					var workers sync.WaitGroup
					defer workers.Wait()
					defer cancel()
					var observer, writer *sql.DB
					var schema, dsn, tenant string
					wantTotal := int64(3)
					if hosted {
						f := newHostedFixture(t, "tenant-concurrent-trash")
						observer, writer, schema, dsn, tenant = f.admin, f.runtime, f.schema, f.dsn, f.tenant
						// Keep the real hosted revision trigger active, including its identity
						// fences on DELETE. These rows are the legacy upgrade's resulting state.
						_, err := writer.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,deleted_at,trash_includes_codex_pages,source_trash_includes_codex_pages) VALUES($1,'sample','machine','codex',NOW(),true,true),($2,'sample','machine','codex',NOW(),false,false)`, codexTrashThread, codexTrashPage)
						require.NoError(t, err)
						wantTotal = 2
						var shared int
						require.NoError(t, observer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT DISTINCT hashtextextended(a,0)&255 AS b FROM unnest($1::text[]) a INTERSECT SELECT DISTINCT hashtextextended(a,0)&255 AS b FROM unnest($2::text[]) a) overlap`, []string{codexTrashThread, legacyPublicVariant(codexTrashThread)}, []string{codexTrashPage, legacyPublicVariant(codexTrashPage)}).Scan(&shared))
						require.Zero(t, shared, "fixture aliases must not serialize the requests before row locks")
					} else {
						_, _, store := newCodexTrashMirror(t)
						observer, writer = store.pg, store.pg
						require.NoError(t, observer.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema))
						dsn = testPGURL(t)
					}
					type result struct {
						n   int64
						err error
					}
					operations := make(map[string]func() (int64, error))
					for _, op := range []string{"restore", "purge"} {
						opDSN, err := appendConnParams(dsn, map[string]string{"application_name": "codex_curation_" + op})
						require.NoError(t, err)
						var pg *sql.DB
						if hosted {
							pg, err = OpenHosted(opDSN, schema, tenant, false)
						} else {
							pg, err = Open(opDSN, schema, true)
						}
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, pg.Close()) })
						var endpoint interface {
							RestoreSession(context.Context, string) (int64, error)
							DeleteSessionIfTrashed(context.Context, string) (int64, error)
							EmptyTrash(context.Context) (int, error)
						}
						if hosted {
							endpoint, err = newHostedAdapter(pg, tenant)
							require.NoError(t, err)
						} else {
							endpoint = &Store{pg: pg}
						}
						if op == "restore" {
							operations[op] = func() (int64, error) { return endpoint.RestoreSession(ctx, codexTrashPage) }
						} else if empty {
							operations[op] = func() (int64, error) { n, err := endpoint.EmptyTrash(ctx); return int64(n), err }
						} else {
							operations[op] = func() (int64, error) { return endpoint.DeleteSessionIfTrashed(ctx, codexTrashThread) }
						}
					}
					// Queue both production APIs at the thread row. Committing the fixture
					// gate leaves only the API transactions contending with one another.
					gate, err := writer.BeginTx(ctx, nil)
					require.NoError(t, err)
					defer func() { _ = gate.Rollback() }()
					var id string
					require.NoError(t, gate.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id=$1 FOR UPDATE`, codexTrashThread).Scan(&id))
					results := make(map[string]chan result)
					second := "restore"
					if first == "restore" {
						second = "purge"
					}
					for _, op := range []string{first, second} {
						done := make(chan result, 1)
						results[op] = done
						workers.Go(func() { n, err := operations[op](); done <- result{n, err} })
						require.Eventually(t, func() bool {
							var blocked bool
							err := observer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0)`, "codex_curation_"+op).Scan(&blocked)
							return err == nil && blocked
						}, 10*time.Second, 20*time.Millisecond, "%s must reach a production lock before releasing the gate", op)
					}
					require.NoError(t, gate.Commit())
					restored, purged := <-results["restore"], <-results["purge"]
					if hosted && restored.err != nil {
						gone, ok := errors.AsType[*db.SessionIdentityError](restored.err)
						require.True(t, ok, "restore may only lose to a committed purge: %v", restored.err)
						assert.Equal(t, "gone", gone.State)
					} else {
						require.NoError(t, restored.err)
					}
					require.NoError(t, purged.err)
					page, err := (&Store{pg: writer}).GetSessionFull(ctx, codexTrashPage)
					require.NoError(t, err)
					if restored.n == 1 {
						require.NotNil(t, page, "a successful restore must survive the competing purge")
						assert.Nil(t, page.DeletedAt)
						if empty {
							assert.Equal(t, wantTotal-1, purged.n)
						} else {
							assert.EqualValues(t, 1, purged.n)
						}
					} else {
						assert.Zero(t, restored.n)
						assert.Nil(t, page, "a purge that won the race must remove its page")
						assert.Equal(t, wantTotal, purged.n)
					}
					head, err := (&Store{pg: writer}).GetSessionFull(ctx, codexTrashThread)
					require.NoError(t, err)
					assert.Nil(t, head)
				})
			}
		}
	}
}

func TestHostedCodexPurgeRetriesBusyPageIdentity(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%v", empty), func(t *testing.T) {
			f := newHostedFixture(t, "tenant-busy-trash-page")
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			var workers sync.WaitGroup
			defer workers.Wait()
			defer cancel()
			_, err := f.runtime.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,deleted_at,trash_includes_codex_pages) VALUES($1,'sample','machine','codex',NOW(),true),($2,'sample','machine','codex',NOW(),false)`, codexTrashThread, codexTrashPage)
			require.NoError(t, err)
			dsn, err := appendConnParams(f.dsn, map[string]string{"application_name": "codex_identity_purge"})
			require.NoError(t, err)
			pg, err := OpenHosted(dsn, f.schema, f.tenant, false)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, pg.Close()) })
			h, err := newHostedAdapter(pg, f.tenant)
			require.NoError(t, err)
			gate, err := f.runtime.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = gate.Rollback() }()
			require.NoError(t, lockHostedAliases(ctx, gate, []string{codexTrashPage, legacyPublicVariant(codexTrashPage)}))
			type result struct {
				n   int64
				err error
			}
			done := make(chan result, 1)
			workers.Go(func() {
				if empty {
					n, err := h.EmptyTrash(ctx)
					done <- result{int64(n), err}
				} else {
					n, err := h.DeleteSessionIfTrashed(ctx, codexTrashThread)
					done <- result{n, err}
				}
			})
			// DELETE's real identity trigger returns 40001 while the page's
			// alias is busy. Wait for rollback, then let the next attempt run.
			require.Eventually(t, func() bool {
				var rolledBack bool
				err := f.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name='codex_identity_purge' AND state='idle' AND upper(query)='ROLLBACK')`).Scan(&rolledBack)
				return err == nil && rolledBack
			}, 10*time.Second, time.Millisecond)
			require.NoError(t, gate.Commit())
			got := <-done
			require.NoError(t, got.err)
			assert.EqualValues(t, 2, got.n, "the aborted purge must not count or retain partial work")
			for _, id := range []string{codexTrashThread, codexTrashPage} {
				row, err := (&Store{pg: f.runtime}).GetSessionFull(ctx, id)
				require.NoError(t, err)
				assert.Nil(t, row)
			}
		})
	}
}
