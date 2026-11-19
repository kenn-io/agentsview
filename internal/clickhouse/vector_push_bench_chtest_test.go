//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// Mirror shape the vector push benchmark seeds, taken from a long-lived
// mirror: 31,656 sessions with 11 documents each (about 348,000 documents)
// and one or two chunks per document (about 675,000 chunks). Re-pushing
// every session twice the way a push flush did leaves lightweight-delete
// patch parts on both tables, as on that mirror.
const (
	benchVectorSessions       = 31656
	benchVectorDocsPerSession = 11
	benchVectorDimension      = 768
	benchVectorInsertBatches  = 64
	benchVectorRepushRounds   = 2
)

// benchSessionStride spreads the sessions a case changes across the doc_key
// order: session s gets id number s*benchSessionStride mod
// benchVectorSessions, a bijection because the stride is coprime to the
// session count. It is near the session count over the golden ratio, so the
// first N sessions land evenly across the id range for every N, the way the
// random UUIDs of the sessions one production push changes do.
const benchSessionStride = 19565

// benchSessionID is the SQL expression for session s's UUID-shaped id.
var benchSessionID = fmt.Sprintf(
	`concat('00000000-0000-4000-8000-', leftPad(toString(s * %d %% %d), 12, '0'))`,
	benchSessionStride, benchVectorSessions)

// BenchmarkVectorPush measures generation-wide vector pushes on a mirror
// seeded as above, where every session not named by a case is unchanged:
//
//   - evict=N: the local index dropped N sessions the mirror still holds
//     vectors for, so the push evicts them.
//   - repush=N: N sessions gained one document, so the push rewrites them.
//     N=10 is the size of a typical watch push.
//
// Each iteration re-seeds its sessions untimed. Besides time, it reports the
// DELETE statements the push ran, the server CPU and rows read its statements
// used, the rows the DELETEs wrote to patch parts, and the work of the
// mutations that ran for the DELETEs, read from system.query_log and
// system.part_log, so the server needs both logs enabled. Under the default
// lightweight_delete_mode a DELETE runs as a mutation on background threads,
// which query_log does not charge to the statement, so server-cpu-s/op leaves
// it out. part_log charges a mutation for the parts it rewrites
// (mutation-cpu-s/op) but not for checking which parts hold matching rows.
// server-wide-cpu-s/op covers everything the server did during the push,
// including any merge that started, so a push starts only once the merges its
// seeding triggered have finished.
//
// To measure lightweight_update, add ?lightweight_delete_mode=lightweight_update
// to TEST_CLICKHOUSE_URL on a server whose merge_tree settings enable
// enable_block_number_column and enable_block_offset_column. Without those
// columns the server silently runs ALTER UPDATE instead, and the log lines the
// seed writes report no patch rows.
func BenchmarkVectorPush(b *testing.B) {
	ctx := b.Context()
	dsn, database := chtest.FreshDatabase(b)
	target := Target{URL: dsn, Database: database}
	local, err := db.Open(ctx, filepath.Join(b.TempDir(), "local.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, local.Close()) })
	source := &fakeVectorSource{
		gen: storage.VectorGenerationInfo{
			Fingerprint: vectorFixtureFP, Model: "bench-model", Dimension: benchVectorDimension,
		},
		hasGen: true,
	}
	s, err := New(ctx, target, local, fixtureMachine, storage.PusherOptions{VectorSource: source})
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	require.NoError(b, s.EnsureSchema(ctx))
	// The push's own connection keeps the settings the DSN names, so the
	// seeding deletes run under the same lightweight_delete_mode as the push.
	conn := s.conn
	seedBenchVectorMirror(b, conn, s.archiveID)

	unchangedFrom := func(n int) map[string]string {
		hashes := make(map[string]string, benchVectorSessions)
		for i := n; i < benchVectorSessions; i++ {
			hashes[benchSessionIDOf(i)] = benchStateHash(i)
		}
		return hashes
	}
	run := func(b *testing.B, sessions int, check func(storage.VectorPushResult)) {
		var elapsed time.Duration
		var total benchStats
		var serverWideCPU float64
		for b.Loop() {
			b.StopTimer()
			seedBenchVectorRows(b, conn, s.archiveID, 0, sessions)
			comment := fmt.Sprintf("bench-vector-push-%d", newPushVersion())
			waitForBenchMerges(b, conn)
			var pushStart string
			require.NoError(b, conn.QueryRowContext(b.Context(),
				`SELECT toString(now64(6))`).Scan(&pushStart))
			cpuBefore := benchServerWideCPUSeconds(b, conn)
			pushCtx := chdriver.Context(b.Context(),
				chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
			b.StartTimer()
			start := time.Now()
			res, err := s.pushVectors(pushCtx, 0, nil, nil, nil, nil)
			elapsed += time.Since(start)
			b.StopTimer()
			require.NoError(b, err)
			check(res)
			serverWideCPU += benchServerWideCPUSeconds(b, conn) - cpuBefore
			total.add(benchServerStats(b, conn, comment, pushStart))
			b.StartTimer()
		}
		b.ReportMetric(float64(elapsed.Milliseconds())/float64(b.N*sessions), "ms/session")
		n := float64(b.N)
		b.ReportMetric(float64(total.deletes)/n, "deletes/op")
		b.ReportMetric(total.cpuSeconds/n, "server-cpu-s/op")
		b.ReportMetric(serverWideCPU/n, "server-wide-cpu-s/op")
		b.ReportMetric(float64(total.readRows)/n, "read-rows/op")
		b.ReportMetric(float64(total.patchRows)/n, "patch-rows/op")
		b.ReportMetric(float64(total.mutatedParts)/n, "mutated-parts/op")
		b.ReportMetric(float64(total.mutationReadRows)/n, "mutation-read-rows/op")
		b.ReportMetric(total.mutationCPUSeconds/n, "mutation-cpu-s/op")
	}

	for _, evict := range []int{10, 50, 100, 500, 2000} {
		b.Run(fmt.Sprintf("evict=%d", evict), func(b *testing.B) {
			source.hashes, source.docs = unchangedFrom(evict), nil
			run(b, evict, func(res storage.VectorPushResult) {
				require.Equal(b, evict, res.SessionsEvicted)
				require.Equal(b, benchVectorSessions-evict, res.SessionsUnchanged)
			})
			// Restore what the last push evicted for the cases that follow.
			seedBenchVectorRows(b, conn, s.archiveID, 0, evict)
		})
	}
	for _, repush := range []int{10, 50, 100, 500} {
		b.Run(fmt.Sprintf("repush=%d", repush), func(b *testing.B) {
			source.hashes = unchangedFrom(repush)
			source.docs = make(map[string][]storage.VectorPushDoc, repush)
			for i := range repush {
				id := benchSessionIDOf(i)
				source.hashes[id] = benchStateHash(i) + "-grown"
				source.docs[id] = benchSessionDocs(i)
			}
			run(b, repush, func(res storage.VectorPushResult) {
				require.Equal(b, repush, res.SessionsPushed)
				require.Equal(b, benchVectorSessions-repush, res.SessionsUnchanged)
			})
		})
	}
}

func benchSessionIDOf(i int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", i*benchSessionStride%benchVectorSessions)
}

func benchStateHash(i int) string { return fmt.Sprintf("agg-%d", i) }

// benchSessionDocs builds session i's seeded documents, with the keys and
// chunk counts seedBenchVectorRows writes, plus one new document.
func benchSessionDocs(i int) []storage.VectorPushDoc {
	id := benchSessionIDOf(i)
	out := make([]storage.VectorPushDoc, benchVectorDocsPerSession+1)
	for d := range out {
		g := i*benchVectorDocsPerSession + d
		key := fmt.Sprintf("u:%s:00000000-0000-4000-9000-%012d", id, g)
		if d == benchVectorDocsPerSession {
			key = fmt.Sprintf("u:%s:new", id)
		}
		chunks := 2
		if g%16 == 0 {
			chunks = 1
		}
		embeddings := make([][]float32, chunks)
		for c := range embeddings {
			embeddings[c] = make([]float32, benchVectorDimension)
		}
		out[d] = vdoc(id, key, d, fmt.Sprintf("document %d", g), fmt.Sprint(g), embeddings...)
	}
	return out
}

// waitForBenchMerges returns once no merge is running in the bench database.
func waitForBenchMerges(b *testing.B, conn *sql.DB) {
	b.Helper()
	for {
		var merges uint64
		require.NoError(b, conn.QueryRowContext(b.Context(),
			`SELECT count() FROM system.merges WHERE database = currentDatabase()`).Scan(&merges))
		if merges == 0 {
			return
		}
		time.Sleep(time.Second)
	}
}

// benchServerWideCPUSeconds returns the CPU the server has used since it
// started, across all of its work.
func benchServerWideCPUSeconds(b *testing.B, conn *sql.DB) float64 {
	b.Helper()
	var seconds float64
	require.NoError(b, conn.QueryRowContext(b.Context(), `
		SELECT sum(value) / 1e6 FROM system.events
		WHERE event IN ('UserTimeMicroseconds', 'SystemTimeMicroseconds')`).Scan(&seconds))
	return seconds
}

// benchStats is what the server did for one push.
type benchStats struct {
	deletes, readRows, patchRows   uint64
	cpuSeconds                     float64
	mutatedParts, mutationReadRows uint64
	mutationCPUSeconds             float64
}

func (t *benchStats) add(s benchStats) {
	t.deletes += s.deletes
	t.readRows += s.readRows
	t.patchRows += s.patchRows
	t.cpuSeconds += s.cpuSeconds
	t.mutatedParts += s.mutatedParts
	t.mutationReadRows += s.mutationReadRows
	t.mutationCPUSeconds += s.mutationCPUSeconds
}

// benchServerStats returns, for the statements that ran under comment, how
// many were DELETEs, the server CPU seconds and rows read they all used, and
// the rows the DELETEs wrote to patch parts. It adds the vector-table
// mutations that finished since pushStart. A DELETE waits for its mutation
// and the seed runs no mutation during a push, so those are the push's own.
func benchServerStats(b *testing.B, conn *sql.DB, comment, pushStart string) benchStats {
	b.Helper()
	ctx := b.Context()
	_, err := conn.ExecContext(ctx, `SYSTEM FLUSH LOGS`)
	require.NoError(b, err)
	var s benchStats
	require.NoError(b, conn.QueryRowContext(ctx, `
		SELECT countIf(query_kind = 'Delete'),
			sum(ProfileEvents['UserTimeMicroseconds'] + ProfileEvents['SystemTimeMicroseconds']) / 1e6,
			sum(read_rows)
		FROM system.query_log
		WHERE type = 'QueryFinish' AND log_comment = ?`,
		comment).Scan(&s.deletes, &s.cpuSeconds, &s.readRows))
	require.NoError(b, conn.QueryRowContext(ctx, `
		SELECT sum(rows) FROM system.part_log
		WHERE event_type = 'NewPart' AND startsWith(part_name, 'patch-') AND query_id IN (
			SELECT query_id FROM system.query_log
			WHERE type = 'QueryFinish' AND log_comment = ? AND query_kind = 'Delete')`,
		comment).Scan(&s.patchRows))
	require.NoError(b, conn.QueryRowContext(ctx, `
		SELECT countIf(read_rows > 0), sum(read_rows),
			sum(ProfileEvents['UserTimeMicroseconds'] + ProfileEvents['SystemTimeMicroseconds']) / 1e6
		FROM system.part_log
		WHERE event_type = 'MutatePart' AND database = currentDatabase()
		  AND table IN ('vector_documents', 'vector_chunks')
		  AND event_time_microseconds >= toDateTime64(?, 6)`,
		pushStart).Scan(&s.mutatedParts, &s.mutationReadRows, &s.mutationCPUSeconds))
	return s
}

// seedBenchVectorMirror writes the resident session rows, then every
// session's documents, chunks, and push state in batches, then re-pushes
// every session the way a push flush used to: insert the new version, then
// delete every older one. Each delete writes a patch part.
func seedBenchVectorMirror(b *testing.B, conn *sql.DB, archiveID string) {
	b.Helper()
	ctx := b.Context()
	_, err := conn.ExecContext(ctx, `
		INSERT INTO sessions (id, project, machine, agent, source_archive_id, push_version)
		SELECT `+benchSessionID+`, 'bench', 'bench', 'claude', ?, 1
		FROM (SELECT number AS s FROM numbers(?))`, archiveID, benchVectorSessions)
	require.NoError(b, err, "seed sessions")
	step := (benchVectorSessions + benchVectorInsertBatches - 1) / benchVectorInsertBatches
	for round := 0; round <= benchVectorRepushRounds; round++ {
		for lo := 0; lo < benchVectorSessions; lo += step {
			hi := min(lo+step, benchVectorSessions)
			version := seedBenchVectorRows(b, conn, archiveID, lo, hi)
			if round == 0 {
				continue
			}
			for _, table := range []string{"vector_documents", "vector_chunks"} {
				_, err := conn.ExecContext(ctx, `
					DELETE FROM `+table+`
					WHERE session_id IN (SELECT `+benchSessionID+` FROM (SELECT ? + number AS s FROM numbers(?)))
					  AND push_version < ?`, lo, hi-lo, version)
				require.NoError(b, err, "re-push delete")
			}
		}
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT table, countIf(name NOT LIKE 'patch%'), sumIf(rows, name NOT LIKE 'patch%'),
			countIf(name LIKE 'patch%'), sumIf(rows, name LIKE 'patch%')
		FROM system.parts
		WHERE database = currentDatabase() AND active AND table IN ('vector_documents', 'vector_chunks')
		GROUP BY table ORDER BY table`)
	require.NoError(b, err)
	defer rows.Close()
	for rows.Next() {
		var table string
		var dataParts, dataRows, patchParts, patchRows uint64
		require.NoError(b, rows.Scan(&table, &dataParts, &dataRows, &patchParts, &patchRows))
		b.Logf("%s: %d data rows in %d parts, %d patch rows in %d patch parts",
			table, dataRows, dataParts, patchRows, patchParts)
	}
	require.NoError(b, rows.Err())
}

// seedBenchVectorRows writes documents, chunks, and push state for sessions
// [lo, hi) under a fresh push version and returns that version. It inserts
// at most benchSeedSessions sessions per statement, which keeps each insert
// within a small test server's memory limit.
func seedBenchVectorRows(b *testing.B, conn *sql.DB, archiveID string, lo, hi int) uint64 {
	b.Helper()
	version := newPushVersion()
	for from := lo; from < hi; from += benchSeedSessions {
		seedBenchVectorRange(b, conn, archiveID, from, min(from+benchSeedSessions, hi), version)
	}
	return version
}

// benchSeedSessions bounds the sessions one seeding insert covers.
const benchSeedSessions = 500

func seedBenchVectorRange(b *testing.B, conn *sql.DB, archiveID string, lo, hi int, version uint64) {
	b.Helper()
	ctx := context.WithoutCancel(b.Context())
	docs := `
		SELECT s, d, s * ? + d AS g, ` + benchSessionID + ` AS sid,
			concat('u:', sid, ':00000000-0000-4000-9000-', leftPad(toString(g), 12, '0')) AS doc_key
		FROM (SELECT ? + intDiv(number, ?) AS s, number % ? AS d FROM numbers(?))`
	docArgs := []any{
		benchVectorDocsPerSession, lo, benchVectorDocsPerSession, benchVectorDocsPerSession,
		(hi - lo) * benchVectorDocsPerSession,
	}
	_, err := conn.ExecContext(ctx, `
		INSERT INTO vector_documents (
			doc_key, session_id, source_uuid, ordinal, ordinal_end, subordinate,
			offsets, content, content_hash, push_version
		)
		SELECT doc_key, sid, toString(g), d, d, false, '[]',
			concat(repeat('lorem ipsum ', 640), toString(g)), toString(g), ?
		FROM (`+docs+`)`, append([]any{version}, docArgs...)...)
	require.NoError(b, err, "seed vector documents")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO vector_chunks (
			generation_fingerprint, doc_key, chunk_index, session_id, embedding, push_version
		)
		SELECT ?, doc_key, c, sid, arrayWithConstant(?, toFloat32(g % 7) / 7), ?
		FROM (`+docs+`) ARRAY JOIN range(if(g % 16 = 0, 1, 2)) AS c`,
		append([]any{vectorFixtureFP, benchVectorDimension, version}, docArgs...)...)
	require.NoError(b, err, "seed vector chunks")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO vector_push_state (
			source_archive_id, generation_fingerprint, session_id, doc_agg_hash, push_version
		)
		SELECT ?, ?, `+benchSessionID+`, concat('agg-', toString(s)), ?
		FROM (SELECT ? + number AS s FROM numbers(?))`,
		archiveID, vectorFixtureFP, version, lo, hi-lo)
	require.NoError(b, err, "seed vector push state")
}
