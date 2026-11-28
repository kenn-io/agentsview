//go:build pgtest

package postgres

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/docbank/document/ocr"
)

// roundTripCountDelay is the latency the counting proxies add. Without it the
// server answers the first statements of a pipelined batch while the client
// is still writing the rest, and each switch would count as a round trip.
const roundTripCountDelay = 5 * time.Millisecond

func TestPGPushNewSessionsTakeFewRoundTrips(t *testing.T) {
	t.Parallel()
	const sessions, messages = 100, 20
	ctx := t.Context()
	pgURL := testPGURL(t)
	for _, sequential := range []bool{false, true} {
		local := testDB(t)
		schema := fmt.Sprintf("round_trips_sequential_%t", sequential)
		syncer, proxy := newNewSessionBatchSync(t, pgURL, schema, local, sequential)
		syncer.pushBatchSize = 0
		// The first push to a target also publishes model pricing and
		// backfill markers; take that once so the count covers sessions.
		seedLatencyFixture(t, local, "warm", 1, 2)
		_, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)

		seedLatencyFixture(t, local, "new", sessions, messages)
		before := proxy.roundTrips.Load()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		trips := proxy.roundTrips.Load() - before
		require.Zero(t, result.Errors)
		require.Equal(t, sessions, result.SessionsPushed)
		t.Logf("sequential=%t: %d round trips for %d new sessions", sequential, trips, sessions)
		if sequential {
			// One statement at a time costs about 20 round trips per session.
			assert.Greater(t, trips, int64(10*sessions))
		} else {
			// The batch takes a few round trips per 50-session chunk; parallel
			// load can split a pipelined write and add a few more.
			assert.Less(t, trips, int64(sessions))
		}
	}
}

// seedNewSessionBatchFixture writes one session for each kind of row a push
// publishes, plus sessions the batch must hand to the sequential path.
func seedNewSessionBatchFixture(t *testing.T, local *db.DB) {
	t.Helper()
	ctx := t.Context()
	seedLatencyFixture(t, local, "plain", 6, 6)
	seedLatencyFixture(t, local, "excluded", 1, 2)
	seedLatencyFixture(t, local, "foreign", 1, 2)

	require.NoError(t, local.ReplaceSessionSecretFindings(ctx, "plain-1", []db.SecretFinding{{
		SessionID: "plain-1", RuleName: "aws-access-key", Confidence: "high", LocationKind: "message",
		MessageOrdinal: 0, MatchStart: 1, MatchEnd: 5, RedactedMatch: "AKIA****", RulesVersion: "v1",
	}}, 1, "v1"))
	findings := testFrictionRows("plain-2")
	require.NoError(t, local.UpdateSessionSignals(ctx, "plain-2", db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{Findings: findings, RulesVersion: friction.RulesVersion, Hash: db.FrictionHash(findings, friction.RulesVersion)},
	}))
	require.NoError(t, local.ReplaceSessionPlanUsage(ctx, "plain-3", fullPlanUsage()))
	require.NoError(t, local.ReplaceSessionUsageTotal(ctx, "plain-4", &db.SessionUsageTotal{
		Source: "agent", Models: []db.SessionUsageTotalModel{{Model: "model-a", InputTokens: 10, OutputTokens: 5}},
	}))

	dbtest.SeedCommitLinks(t, local)

	image := `[{"type":"input_image","image_url":"data:image/png;base64,` +
		base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage")) + `"}]`
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: "image", Project: "proj", Machine: "latency", Agent: "claude",
		MessageCount: 2, UserMessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{
		{SessionID: "image", Ordinal: 0, Role: "user", Content: "look at this", ContentLength: 12},
		{SessionID: "image", Ordinal: 1, Role: "assistant", Content: "reading it", ContentLength: 10,
			ToolCalls: []db.ToolCall{{ToolName: "Read", ToolUseID: "tu-image", ResultContent: image}}},
	}))
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, db.RetainedToolResultImages(image)[0].SHA256,
		ocr.Identity{Fingerprint: "proc-a", Provider: "test", Model: "model", Revision: "r1"}, "invoice 4827", false))

	filePath := "logs/session_abc/meta.json"
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: "vibe:abc", Project: "proj", Machine: "latency", Agent: "vibe", FilePath: &filePath,
		MessageCount: 1, UserMessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: "vibe:abc", Ordinal: 0, Role: "user", Content: "hello"}}))
}

// seedNewSessionBatchPGState gives the target rows that a new session's push
// must respect: a retained commit link for a session PostgreSQL has no row
// for, an exclusion, and a session another machine owns.
func seedNewSessionBatchPGState(t *testing.T, syncer *Sync) {
	t.Helper()
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, `INSERT INTO commit_links
		(session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_text,project,retained)
		VALUES ('commit-live','fff1234567890123456789012345678901234567','main','Old change','repo',1,0,'old-call','2026-01-01T00:00:00Z','','commit-project',TRUE)`)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `INSERT INTO excluded_sessions (id) VALUES ('excluded-0')`)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `INSERT INTO sessions
		(id, machine, owner_marker, project, agent, first_message, message_count, user_message_count)
		VALUES ('foreign-0', 'other-machine', 'other-marker', 'proj', 'claude', 'theirs', 1, 1)`)
	require.NoError(t, err)
}

// twinNewSessionBatchArchives seeds one archive and copies its file, so both
// pushes start from identical rows, including seed-time timestamps.
func twinNewSessionBatchArchives(t *testing.T) (*db.DB, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	first := filepath.Join(dir, "first.db")
	seeded, err := db.Open(t.Context(), first)
	require.NoError(t, err)
	seedNewSessionBatchFixture(t, seeded)
	require.NoError(t, seeded.Close())
	data, err := os.ReadFile(first)
	require.NoError(t, err)
	second := filepath.Join(dir, "second.db")
	require.NoError(t, os.WriteFile(second, data, 0o600))
	open := func(path string) *db.DB {
		archive, err := db.Open(t.Context(), path)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, archive.Close()) })
		return archive
	}
	return open(first), open(second)
}

func newNewSessionBatchSync(t *testing.T, pgURL, schema string, local *db.DB, sequential bool) (*Sync, *latencyProxy) {
	t.Helper()
	proxy := startLatencyProxy(t, pgURL, roundTripCountDelay)
	syncer, err := New(t.Context(), proxy.url(t, pgURL), schema, local, "latency", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	require.NoError(t, syncer.EnsureSchema(t.Context()))
	syncer.pushBatchSize = 5
	syncer.sequentialNewSessions = sequential
	return syncer, proxy
}

// newSessionBatchTables lists the tables a session push writes and the
// columns that differ between two correct pushes: generated row IDs, push
// times, and journal revisions.
var newSessionBatchTables = []struct {
	name     string
	volatile []string
}{
	// The owner marker belongs to each push target, and sessions created
	// after the archives were copied carry their own seed times.
	{"sessions", []string{"updated_at", "owner_marker", "created_at"}},
	{"messages", []string{"id"}},
	{"tool_calls", []string{"id"}},
	{"tool_result_events", []string{"id"}},
	{"usage_events", []string{"id"}},
	{"secret_findings", []string{"id", "created_at"}},
	{"commit_links", nil},
	{"image_text", []string{"id"}},
	{"friction_findings", []string{"id", "created_at"}},
	{"session_plan_usage", nil},
	{"session_usage_totals", nil},
	{"session_aliases", []string{"created_at"}},
	{"excluded_sessions", []string{"created_at"}},
	{"pinned_messages", []string{"id", "created_at"}},
	{"usage_session_changes", []string{"revision"}},
}

func assertNewSessionBatchTablesEqual(t *testing.T, want, got *Sync) {
	t.Helper()
	wantDump, gotDump := dumpNewSessionBatchTables(t, want), dumpNewSessionBatchTables(t, got)
	for _, table := range newSessionBatchTables {
		assert.Equal(t, wantDump[table.name], gotDump[table.name], table.name)
	}
}

func dumpNewSessionBatchTables(t *testing.T, syncer *Sync) map[string][]string {
	t.Helper()
	out := make(map[string][]string, len(newSessionBatchTables))
	for _, table := range newSessionBatchTables {
		volatile := table.volatile
		if volatile == nil {
			volatile = []string{}
		}
		var exists bool
		require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, table.name).Scan(&exists))
		if !exists {
			// CockroachDB targets have no usage change journal.
			continue
		}
		rows, err := syncer.pg.QueryContext(t.Context(),
			`SELECT (to_jsonb(t) - $1::text[])::text FROM `+table.name+` t ORDER BY 1`, volatile)
		require.NoError(t, err, table.name)
		dump := []string{}
		for rows.Next() {
			var row string
			require.NoError(t, rows.Scan(&row))
			dump = append(dump, row)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		out[table.name] = dump
	}
	return out
}

func TestPGPushNewSessionBatchMatchesSequentialWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgURL := testPGURL(t)
	batchedLocal, sequentialLocal := twinNewSessionBatchArchives(t)
	batched, batchedProxy := newNewSessionBatchSync(t, pgURL, "new_batched", batchedLocal, false)
	sequential, sequentialProxy := newNewSessionBatchSync(t, pgURL, "new_sequential", sequentialLocal, true)
	seedNewSessionBatchPGState(t, batched)
	seedNewSessionBatchPGState(t, sequential)

	push := func(syncer *Sync, proxy *latencyProxy) (storage.PushResult, int64) {
		t.Helper()
		before := proxy.roundTrips.Load()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		return result, proxy.roundTrips.Load() - before
	}
	batchedResult, batchedTrips := push(batched, batchedProxy)
	sequentialResult, sequentialTrips := push(sequential, sequentialProxy)
	require.NotZero(t, sequentialResult.SessionsPushed)
	assert.Equal(t, 1, sequentialResult.SkippedConflicts, "the foreign session stays with its owner")
	assert.Equal(t, sequentialResult.SessionsPushed, batchedResult.SessionsPushed)
	assert.Equal(t, sequentialResult.MessagesPushed, batchedResult.MessagesPushed)
	assert.Equal(t, sequentialResult.SkippedConflicts, batchedResult.SkippedConflicts)
	assert.Equal(t, sequentialResult.Errors, batchedResult.Errors)
	t.Logf("first push: %d round trips batched, %d one statement at a time", batchedTrips, sequentialTrips)
	assert.Less(t, batchedTrips, sequentialTrips, "the batched push should save round trips")
	assertNewSessionBatchTablesEqual(t, sequential, batched)

	// A later push mixes a changed session PostgreSQL already has with a new
	// one in the same chunk.
	for _, local := range []*db.DB{batchedLocal, sequentialLocal} {
		require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: "plain-0", Ordinal: 6, Role: "user", Content: "one more thing"}}))
		session, err := local.GetSession(ctx, "plain-0")
		require.NoError(t, err)
		require.NotNil(t, session)
		session.MessageCount = 7
		require.NoError(t, local.UpsertSession(ctx, *session))
		seedLatencyFixture(t, local, "late", 1, 4)
	}
	batchedResult, _ = push(batched, batchedProxy)
	sequentialResult, _ = push(sequential, sequentialProxy)
	assert.Equal(t, sequentialResult.SessionsPushed, batchedResult.SessionsPushed)
	assert.Equal(t, sequentialResult.MessagesPushed, batchedResult.MessagesPushed)
	assert.Equal(t, sequentialResult.Errors, batchedResult.Errors)
	assertNewSessionBatchTablesEqual(t, sequential, batched)
}

func TestPGPushNewSessionBatchYieldsToConcurrentOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgURL := testPGSchemaURL(t, "agentsview")
	local := testDB(t)
	seedLatencyFixture(t, local, "race", 4, 4)
	syncer, _ := newNewSessionBatchSync(t, pgURL, "agentsview", local, false)

	other, err := sql.Open("pgx", pgURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	var once sync.Once
	syncer.beforeNewSessionBatch = func() {
		once.Do(func() {
			_, err := other.ExecContext(ctx, `INSERT INTO agentsview.sessions
				(id, machine, owner_marker, project, agent, first_message, message_count, user_message_count)
				VALUES ('race-1', 'other-machine', 'other-marker', 'proj', 'claude', 'theirs', 1, 1)`)
			require.NoError(t, err)
		})
	}

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, result.SessionsPushed)
	assert.Equal(t, 12, result.MessagesPushed)
	assert.Equal(t, 1, result.SkippedConflicts)
	assert.Zero(t, result.Errors)

	var machine, firstMessage string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT machine, first_message FROM sessions WHERE id = 'race-1'`).Scan(&machine, &firstMessage))
	assert.Equal(t, "other-machine", machine)
	assert.Equal(t, "theirs", firstMessage)
	var raceMessages, otherMessages int
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE session_id = 'race-1'),
		count(*) FILTER (WHERE session_id <> 'race-1') FROM messages`).Scan(&raceMessages, &otherMessages))
	assert.Zero(t, raceMessages)
	assert.Equal(t, 12, otherMessages)
}

// skipOnCockroach skips tests whose setup or concurrency assumptions hold
// only on PostgreSQL.
func skipOnCockroach(t *testing.T, pgURL, reason string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err)
	defer pg.Close()
	var version string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT version()`).Scan(&version))
	if strings.Contains(version, "CockroachDB") {
		t.Skip(reason)
	}
}

func TestPGPushNewSessionBatchFailureRetriesSessionsAlone(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgURL := testPGURL(t)
	skipOnCockroach(t, pgURL, "CockroachDB triggers cannot read NEW")
	results := map[bool]storage.PushResult{}
	for _, sequential := range []bool{false, true} {
		local := testDB(t)
		seedLatencyFixture(t, local, "retry", 4, 4)
		schema := fmt.Sprintf("retry_sequential_%t", sequential)
		syncer, _ := newNewSessionBatchSync(t, pgURL, schema, local, sequential)
		_, err := syncer.pg.ExecContext(ctx, `
			CREATE FUNCTION reject_retry_2() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.session_id = 'retry-2' THEN RAISE EXCEPTION 'rejected for test'; END IF;
				RETURN NEW;
			END $$;
			CREATE TRIGGER reject_retry_2 BEFORE INSERT ON messages
				FOR EACH ROW EXECUTE FUNCTION reject_retry_2();`)
		require.NoError(t, err)
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		results[sequential] = result

		var pushed []string
		rows, err := syncer.pg.QueryContext(ctx, `SELECT DISTINCT session_id FROM messages ORDER BY 1`)
		require.NoError(t, err)
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			pushed = append(pushed, id)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		assert.Equal(t, []string{"retry-0", "retry-1", "retry-3"}, pushed, "sequential=%t", sequential)
	}
	assert.Equal(t, 1, results[false].Errors)
	assert.Equal(t, 3, results[false].SessionsPushed)
	assert.Equal(t, results[true].Errors, results[false].Errors)
	assert.Equal(t, results[true].SessionsPushed, results[false].SessionsPushed)
	assert.Equal(t, results[true].MessagesPushed, results[false].MessagesPushed)
}

func TestPGPushNewSessionBatchKeepsPinsOfConcurrentSameOwnerRow(t *testing.T) {
	t.Parallel()
	for _, sequential := range []bool{false, true} {
		t.Run(fmt.Sprintf("sequential=%t", sequential), func(t *testing.T) {
			ctx := t.Context()
			pgURL := testPGURL(t)
			local := testDB(t)
			seedLatencyFixture(t, local, "pinned", 3, 0)
			messages := []db.Message{}
			for ordinal, content := range []string{"open the plan", "explain the join", "rewrite the query", "run it again"} {
				messages = append(messages, db.Message{SessionID: "pinned-1", Ordinal: ordinal, Role: "user", Content: content, ContentLength: len(content)})
			}
			require.NoError(t, local.InsertMessages(ctx, messages))
			schema := fmt.Sprintf("pins_sequential_%t", sequential)
			syncer, _ := newNewSessionBatchSync(t, pgURL, schema, local, sequential)
			markerID, err := syncer.pushMarkerID(ctx)
			require.NoError(t, err)

			other, err := sql.Open("pgx", pgURL)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			var once sync.Once
			// Another push from this archive publishes an older transcript of
			// a session after the batch chose it, and a reader pins a message
			// that the current transcript holds at another ordinal.
			hook := func() {
				once.Do(func() {
					_, err := other.ExecContext(ctx, `INSERT INTO `+schema+`.sessions
						(id, machine, owner_marker, project, agent, first_message, message_count, user_message_count)
						VALUES ('pinned-1', 'latency', $1, 'proj', 'claude', 'earlier', 2, 2)`, markerID)
					require.NoError(t, err)
					_, err = other.ExecContext(ctx, `INSERT INTO `+schema+`.messages (session_id, ordinal, role, content, content_length)
						VALUES ('pinned-1', 0, 'user', 'open the plan', 13), ('pinned-1', 3, 'user', 'explain the join', 16)`)
					require.NoError(t, err)
					_, err = other.ExecContext(ctx, `INSERT INTO `+schema+`.pinned_messages (session_id, message_id, ordinal, note)
						VALUES ('pinned-1', 3, 3, 'keep this')`)
					require.NoError(t, err)
				})
			}
			if sequential {
				hook()
			} else {
				syncer.beforeNewSessionBatch = hook
			}

			result, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			assert.Zero(t, result.Errors)
			assert.Equal(t, 3, result.SessionsPushed)
			var note string
			var ordinal int
			require.NoError(t, syncer.pg.QueryRowContext(ctx,
				`SELECT note, ordinal FROM pinned_messages WHERE session_id = 'pinned-1'`).Scan(&note, &ordinal))
			assert.Equal(t, "keep this", note)
			assert.Equal(t, 1, ordinal, "the pin follows its message")
		})
	}
}

func TestPGPushNewSessionBatchHonorsLateAliasExclusion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgURL := testPGSchemaURL(t, "agentsview")
	// A serializable transaction cannot see an exclusion committed after
	// it began, on either path.
	skipOnCockroach(t, pgURL, "CockroachDB transactions read from one snapshot")
	local := testDB(t)
	seedLatencyFixture(t, local, "plain", 2, 2)
	filePath := "logs/session_abc/meta.json"
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: "vibe:abc", Project: "proj", Machine: "latency", Agent: "vibe", FilePath: &filePath,
		MessageCount: 1, UserMessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: "vibe:abc", Ordinal: 0, Role: "user", Content: "hello"}}))
	syncer, _ := newNewSessionBatchSync(t, pgURL, "agentsview", local, false)

	other, err := sql.Open("pgx", pgURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	var once sync.Once
	// The session is purged under its fallback ID after the batch chose it.
	syncer.beforeNewSessionBatch = func() {
		once.Do(func() {
			_, err := other.ExecContext(ctx, `INSERT INTO agentsview.excluded_sessions (id) VALUES ('vibe:session_abc')`)
			require.NoError(t, err)
		})
	}

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Errors)
	var published int
	require.NoError(t, syncer.pg.QueryRowContext(ctx,
		`SELECT count(*) FROM sessions WHERE id = 'vibe:abc'`).Scan(&published))
	assert.Zero(t, published, "the purged session stays unpublished")
	var plain int
	require.NoError(t, syncer.pg.QueryRowContext(ctx,
		`SELECT count(*) FROM sessions WHERE id LIKE 'plain-%'`).Scan(&plain))
	assert.Equal(t, 2, plain)
}

func TestPGPushNewSessionBatchReplacesConcurrentSameOwnerRow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgURL := testPGSchemaURL(t, "agentsview")
	local := testDB(t)
	seedLatencyFixture(t, local, "stale", 3, 4)
	syncer, _ := newNewSessionBatchSync(t, pgURL, "agentsview", local, false)
	markerID, err := syncer.pushMarkerID(ctx)
	require.NoError(t, err)

	other, err := sql.Open("pgx", pgURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	var once sync.Once
	// Another push from this archive publishes an older copy of a session,
	// with a commit link the archive no longer has, after the batch chose it.
	syncer.beforeNewSessionBatch = func() {
		once.Do(func() {
			_, err := other.ExecContext(ctx, `INSERT INTO agentsview.sessions
				(id, machine, owner_marker, project, agent, first_message, message_count, user_message_count)
				VALUES ('stale-1', 'latency', $1, 'proj', 'claude', 'earlier', 1, 1)`, markerID)
			require.NoError(t, err)
			_, err = other.ExecContext(ctx, `INSERT INTO agentsview.commit_links
				(session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_text,project,retained)
				VALUES ('stale-1','eee1234567890123456789012345678901234567','main','Gone','repo',1,0,'gone-call','2026-01-01T00:00:00Z','','proj',FALSE)`)
			require.NoError(t, err)
		})
	}

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Errors)
	assert.Equal(t, 3, result.SessionsPushed)
	var messageCount, links int
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT message_count FROM sessions WHERE id = 'stale-1'`).Scan(&messageCount))
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT count(*) FROM commit_links WHERE session_id = 'stale-1'`).Scan(&links))
	assert.Equal(t, 4, messageCount, "the archive's copy replaces the older one")
	assert.Zero(t, links, "the archive no longer has the commit link")
}

func TestPGPushNewSessionBatchRollsBackEarlySends(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgURL := testPGSchemaURL(t, "agentsview")
	skipOnCockroach(t, pgURL, "CockroachDB triggers cannot read NEW")
	local := testDB(t)
	// Two transcripts larger than newSessionBatchFlushBytes make the batch
	// send early whatever the session order, so the rejected session fails
	// after earlier statements already reached the server.
	content := strings.Repeat("x", 1<<20)
	started := "2026-10-01T09:00:00Z"
	for _, id := range []string{"flush-0", "flush-1", "flush-2"} {
		messages := 9
		if id == "flush-1" {
			messages = 1
		}
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Machine: "latency", Project: "proj", Agent: "claude", StartedAt: &started, CreatedAt: started, MessageCount: messages, UserMessageCount: messages}))
		batch := make([]db.Message, messages)
		for ordinal := range messages {
			batch[ordinal] = db.Message{SessionID: id, Ordinal: ordinal, Role: "user", Timestamp: started, Content: content}
		}
		require.NoError(t, local.InsertMessages(ctx, batch))
	}
	syncer, _ := newNewSessionBatchSync(t, pgURL, "agentsview", local, false)
	_, err := syncer.pg.ExecContext(ctx, `
		CREATE FUNCTION reject_flush_1() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.session_id = 'flush-1' THEN RAISE EXCEPTION 'rejected for test'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_flush_1 BEFORE INSERT ON messages
			FOR EACH ROW EXECUTE FUNCTION reject_flush_1();`)
	require.NoError(t, err)

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Errors)
	assert.Equal(t, 2, result.SessionsPushed)
	counts := map[string]int{}
	rows, err := syncer.pg.QueryContext(ctx, `SELECT s.id, count(m.ordinal) FROM sessions s LEFT JOIN messages m ON m.session_id = s.id GROUP BY s.id`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		var count int
		require.NoError(t, rows.Scan(&id, &count))
		counts[id] = count
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.Equal(t, map[string]int{"flush-0": 9, "flush-2": 9}, counts, "the rejected session leaves no rows")
}

func TestPGPushNewSessionBatchReplacesLinksWrittenAfterPreload(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgURL := testPGSchemaURL(t, "agentsview")
	local := testDB(t)
	seedLatencyFixture(t, local, "orphan", 2, 2)
	syncer, _ := newNewSessionBatchSync(t, pgURL, "agentsview", local, false)

	other, err := sql.Open("pgx", pgURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	var once sync.Once
	// A link for a session PostgreSQL has no row for appears after the
	// batch read the published links; the archive has no links for it.
	syncer.beforeNewSessionBatch = func() {
		once.Do(func() {
			_, err := other.ExecContext(ctx, `INSERT INTO agentsview.commit_links
				(session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_text,project,retained)
				VALUES ('orphan-1','ddd1234567890123456789012345678901234567','main','Late','repo',1,0,'late-call','2026-01-01T00:00:00Z','','proj',FALSE)`)
			require.NoError(t, err)
		})
	}

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Errors)
	assert.Equal(t, 2, result.SessionsPushed)
	var links int
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT count(*) FROM commit_links WHERE session_id = 'orphan-1'`).Scan(&links))
	assert.Zero(t, links, "the archive has no links for the session")
}
