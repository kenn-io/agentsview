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

// benchSessionID is the SQL expression for session s's UUID-shaped id.
const benchSessionID = `concat('00000000-0000-4000-8000-', leftPad(toString(s), 12, '0'))`

// BenchmarkVectorPush measures generation-wide vector pushes on a mirror
// seeded as above, where every session not named by a case is unchanged:
//
//   - evict=N: the local index dropped N sessions the mirror still holds
//     vectors for, so the push evicts them.
//   - repush=N: N sessions gained one document, so the push rewrites them.
//
// Each iteration re-seeds its sessions untimed. Besides time, it reports the
// DELETE statements the push ran, the server CPU its statements used, and
// the rows the DELETEs wrote to patch parts, read from system.query_log and
// system.part_log, so the server needs both logs enabled.
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
	conn := chtest.Open(b, dsn, database)
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
		var deletes, patchRows uint64
		var cpuSeconds float64
		for b.Loop() {
			b.StopTimer()
			seedBenchVectorRows(b, conn, s.archiveID, 0, sessions)
			comment := fmt.Sprintf("bench-vector-push-%d", newPushVersion())
			pushCtx := chdriver.Context(b.Context(),
				chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
			b.StartTimer()
			start := time.Now()
			res, err := s.pushVectors(pushCtx, 0, nil, nil, nil, nil)
			elapsed += time.Since(start)
			b.StopTimer()
			require.NoError(b, err)
			check(res)
			n, cpu, rows := benchServerStats(b, conn, comment)
			deletes += n
			cpuSeconds += cpu
			patchRows += rows
			b.StartTimer()
		}
		b.ReportMetric(float64(elapsed.Milliseconds())/float64(b.N*sessions), "ms/session")
		b.ReportMetric(float64(deletes)/float64(b.N), "deletes/op")
		b.ReportMetric(cpuSeconds/float64(b.N), "server-cpu-s/op")
		b.ReportMetric(float64(patchRows)/float64(b.N), "patch-rows/op")
	}

	for _, evict := range []int{100, 2000} {
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
	const repush = 500
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

func benchSessionIDOf(i int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
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

// benchServerStats returns, for the statements that ran under comment, how
// many were DELETEs, the server CPU seconds they all used, and the rows the
// DELETEs wrote to patch parts.
func benchServerStats(b *testing.B, conn *sql.DB, comment string) (deletes uint64, cpuSeconds float64, patchRows uint64) {
	b.Helper()
	ctx := b.Context()
	_, err := conn.ExecContext(ctx, `SYSTEM FLUSH LOGS`)
	require.NoError(b, err)
	require.NoError(b, conn.QueryRowContext(ctx, `
		SELECT countIf(query_kind = 'Delete'),
			sum(ProfileEvents['UserTimeMicroseconds'] + ProfileEvents['SystemTimeMicroseconds']) / 1e6
		FROM system.query_log
		WHERE type = 'QueryFinish' AND log_comment = ?`,
		comment).Scan(&deletes, &cpuSeconds))
	require.NoError(b, conn.QueryRowContext(ctx, `
		SELECT sum(rows) FROM system.part_log
		WHERE event_type = 'NewPart' AND startsWith(part_name, 'patch-') AND query_id IN (
			SELECT query_id FROM system.query_log
			WHERE type = 'QueryFinish' AND log_comment = ? AND query_kind = 'Delete')`,
		comment).Scan(&patchRows))
	return deletes, cpuSeconds, patchRows
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
// [lo, hi) under a fresh push version and returns that version.
func seedBenchVectorRows(b *testing.B, conn *sql.DB, archiveID string, lo, hi int) uint64 {
	b.Helper()
	ctx := context.WithoutCancel(b.Context())
	version := newPushVersion()
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
	return version
}
