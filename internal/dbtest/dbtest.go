// Package dbtest provides shared test helpers for database
// setup and session seeding across test packages.
package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return new(v) }

// WriteTestFile creates a file at path with the given content,
// creating parent directories as needed. Fails the test on
// any error.
func WriteTestFile(
	t *testing.T, path string, content []byte,
) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "MkdirAll %s", filepath.Dir(path))
	require.NoError(t, os.WriteFile(path, content, 0o644), "WriteFile %s", path)
}

// MkdirTempWithCleanup creates a temporary directory and registers
// a cleanup that retries os.RemoveAll on Windows where SQLite WAL
// and mmap'd database files can remain briefly locked after the
// owning *sql.DB has been closed. A runtime.GC() runs first so
// any finalizer-driven stmt cleanup in mattn/go-sqlite3 releases
// its file handles before the directory removal is attempted.
func MkdirTempWithCleanup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		runtime.GC()
		assert.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.NoError(collect, os.RemoveAll(dir))
		}, 10*time.Second, 25*time.Millisecond, "removing temp dir %s", dir)
	})
	return dir
}

// OpenTestDB creates a temporary SQLite database for testing.
// The database is automatically closed when the test completes.
func OpenTestDB(t *testing.T) *db.DB {
	t.Helper()
	dir := MkdirTempWithCleanup(t)
	return OpenTestDBAt(t, filepath.Join(dir, "test.db"))
}

// OpenTestDBAt opens a temporary SQLite database at path for testing, creating
// it from the shared current-schema template when it does not exist. The
// database is automatically closed when the test completes.
func OpenTestDBAt(t *testing.T, path string) *db.DB {
	t.Helper()
	EnsureTestDBAt(t, path)
	d, err := db.Open(t.Context(), path)
	require.NoError(t, err, "opening test db")
	t.Cleanup(func() { d.Close() })
	return d
}

// EnsureTestDBAt creates a current-schema SQLite test database at path when it
// does not already exist. Existing files are left intact so callers can reopen
// and add more fixture rows without losing earlier writes.
func EnsureTestDBAt(t *testing.T, path string) {
	t.Helper()
	ensureTestDBAtWith(t, path, func(path string) error {
		return copyTestDBTemplate(t.Context(), path)
	})
}

func ensureTestDBAtWith(
	t *testing.T, path string, copyTemplate func(string) error,
) {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err, "checking test db %s", path)
	}
	if err := copyTemplate(path); err != nil {
		// The shared template is only a setup-cost optimization.
		// Never let a template failure poison every test in the
		// binary; build this database from scratch instead.
		t.Logf("test db template unavailable, creating %s from scratch: %v",
			path, err)
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(path + suffix)
		}
		d, openErr := db.Open(t.Context(), path)
		require.NoError(t, openErr, "creating test db from scratch")
		if closeErr := d.Close(); closeErr != nil {
			require.NoError(t, closeErr, "closing scratch test db")
		}
	}
}

var (
	testDBTemplateOnce  sync.Once
	testDBTemplateFiles map[string][]byte
	errTestDBTemplate   error
)

func copyTestDBTemplate(ctx context.Context, dst string) error {
	testDBTemplateOnce.Do(func() {
		testDBTemplateFiles, errTestDBTemplate = buildTestDBTemplate(ctx)
	})
	if errTestDBTemplate != nil {
		return errTestDBTemplate
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating test db dir: %w", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, ok := testDBTemplateFiles[suffix]
		if !ok {
			continue
		}
		if err := os.WriteFile(dst+suffix, data, 0o600); err != nil {
			return fmt.Errorf("writing test db copy %s: %w", dst+suffix, err)
		}
	}
	return nil
}

func buildTestDBTemplate(ctx context.Context) (map[string][]byte, error) {
	dir, err := os.MkdirTemp("", "agentsview-dbtest-template-*")
	if err != nil {
		return nil, fmt.Errorf("creating db template dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "test.db")
	template, err := db.Open(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("opening db template: %w", err)
	}
	// Checkpointing keeps the copied template compact, but it is best-effort:
	// the copy below carries the -wal/-shm files along, so a checkpoint that
	// cannot finish in time must not fail the build. The generous deadline only
	// guards against a hung checkpoint on slow Windows CI disks.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := template.CheckpointWALTruncate(ctx); err != nil {
		fmt.Fprintf(os.Stderr,
			"dbtest: template wal checkpoint failed, copying wal as-is: %v\n",
			err)
	}
	if err := template.Close(); err != nil {
		return nil, fmt.Errorf("closing db template: %w", err)
	}

	files := make(map[string][]byte, 3)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			if suffix != "" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("reading db template %s: %w", path+suffix, err)
		}
		files[suffix] = data
	}
	return files, nil
}

// SeedMessages inserts messages into the database, failing the
// test on error.
func SeedMessages(t *testing.T, d *db.DB, msgs ...db.Message) {
	t.Helper()
	if err := d.InsertMessages(t.Context(), msgs); err != nil {
		require.NoError(t, err, "SeedMessages")
	}
}

// UserMsg creates a user message for the given session.
func UserMsg(
	sid string, ordinal int, content string,
) db.Message {
	return db.Message{
		SessionID:     sid,
		Ordinal:       ordinal,
		Role:          "user",
		Content:       content,
		ContentLength: len(content),
	}
}

// UserMessagesf builds count user messages for the session with
// ordinals 0..count-1 and content formatted as fmt.Sprintf(format, i).
func UserMessagesf(sid string, count int, format string) []db.Message {
	msgs := make([]db.Message, 0, count)
	for i := range count {
		msgs = append(msgs, UserMsg(sid, i, fmt.Sprintf(format, i)))
	}
	return msgs
}

// AsstMsg creates an assistant message for the given session.
func AsstMsg(
	sid string, ordinal int, content string,
) db.Message {
	return db.Message{
		SessionID:     sid,
		Ordinal:       ordinal,
		Role:          "assistant",
		Content:       content,
		ContentLength: len(content),
	}
}

// SeedSession creates and upserts a session with sensible
// defaults. Override any field via the opts functions.
func SeedSession(
	t *testing.T, d *db.DB, id, project string,
	opts ...func(*db.Session),
) {
	t.Helper()
	s := db.Session{
		ID:           id,
		Project:      project,
		Machine:      "local",
		Agent:        "claude",
		MessageCount: 1,
	}
	for _, opt := range opts {
		opt(&s)
	}
	if err := d.UpsertSession(t.Context(), s); err != nil {
		require.NoError(t, err, "SeedSession %s", id)
	}
}

// WithMessageCount sets the session's total message count.
func WithMessageCount(n int) func(*db.Session) {
	return func(s *db.Session) { s.MessageCount = n }
}

// WithMessageCounts sets the session's total and user message counts.
func WithMessageCounts(total, user int) func(*db.Session) {
	return func(s *db.Session) {
		s.MessageCount = total
		s.UserMessageCount = user
	}
}

// SeedSessionWithMessages seeds a session and its messages in one call,
// applying any session option functions before insert.
func SeedSessionWithMessages(
	t *testing.T, d *db.DB, id, project string,
	msgs []db.Message, opts ...func(*db.Session),
) {
	t.Helper()
	SeedSession(t, d, id, project, opts...)
	SeedMessages(t, d, msgs...)
}

// SeedUsageGroups records renamed jobs, matching names and ungrouped runs.
func SeedUsageGroups(t *testing.T, conn *sql.DB) {
	t.Helper()
	_, err := conn.ExecContext(t.Context(), `INSERT INTO sessions(id, project, machine, agent, group_key, session_name, started_at, deleted_at, message_count, user_message_count) VALUES
 ('group-a-old', 'hermes-cron', 'host-a.example', 'hermes', 'job-a', 'Old digest · Oct 07 12:00', '2026-10-07T12:00:00Z', NULL, 1, 1),
 ('group-a-new', 'hermes-cron', 'host-a.example', 'hermes', 'job-a', 'Research digest · Oct 08 12:00', '2026-10-08T12:00:00Z', NULL, 1, 1),
 ('group-a-untitled', 'hermes-cron', 'host-a.example', 'hermes', 'job-a', '', '2026-10-09T12:00:00Z', NULL, 1, 1),
 ('group-a-deleted', 'hermes-cron', 'host-a.example', 'hermes', 'job-a', 'Deleted digest · Oct 10 12:00', '2026-10-10T12:00:00Z', '2026-10-10T13:00:00Z', 1, 1),
 ('group-second-machine', 'hermes-cron', 'host-b.example', 'hermes', 'job-a', 'Research digest · Oct 08 12:00', '2026-10-08T12:00:00Z', NULL, 1, 1),
 ('group-b', 'hermes-cron', 'host-a.example', 'hermes', 'job-b', 'Research digest · Oct 08 12:00', '2026-10-08T12:00:00Z', NULL, 1, 1),
 ('group-other', 'hermes-cron', 'host-a.example', 'hermes', '', '', '2026-10-08T12:00:00Z', NULL, 1, 1),
 ('group-another-project', 'another-project', 'host-a.example', 'hermes', 'job-a', 'Separate project · Oct 08 12:00', '2026-10-08T12:00:00Z', NULL, 1, 1);
 INSERT INTO messages(session_id, ordinal, role, content, timestamp)
 SELECT id, 0, 'assistant', 'run message', started_at FROM sessions WHERE id LIKE 'group-%';
 INSERT INTO usage_events(session_id, source, model, input_tokens, output_tokens, cost_microdollars, cost_status, cost_source, occurred_at, dedup_key)
 SELECT id, 'session', 'gpt-5.4', 10, 2, CASE WHEN id = 'group-b' THEN 4000000 ELSE 1000000 END, 'exact', 'provider-reported', started_at, id FROM sessions WHERE id LIKE 'group-%';`)
	require.NoError(t, err)
}

// AssertUsageGroups protects the same filtered grouping contract on each backend.
func AssertUsageGroups(t *testing.T, store db.Store) {
	t.Helper()
	filter := db.UsageFilter{From: "2026-10-07", To: "2026-10-10", Agent: "hermes", ProjectLabels: []string{"hermes-cron"}, TopSessionsByGroup: true, TopSessionsSort: "cost"}
	rows, err := store.GetTopSessionsByCost(t.Context(), filter, 100)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	assert.Equal(t, "job-b", rows[0].GroupKey)
	assert.Equal(t, int64(4_000_000), rows[0].Cost.Microdollars)
	assert.Equal(t, "job-a", rows[1].GroupKey)
	assert.Equal(t, int64(3_000_000), rows[1].Cost.Microdollars)
	assert.Equal(t, 30, rows[1].InputTokens)
	assert.Equal(t, 6, rows[1].OutputTokens)
	byKey := make(map[string]db.TopSessionEntry)
	for _, row := range rows {
		byKey[row.Machine+"/"+row.GroupKey] = row
		assert.Equal(t, "hermes-cron", row.Project)
	}
	assert.Equal(t, "group-other", byKey["host-a.example/"].SessionID)
	assert.Equal(t, 10, byKey["host-a.example/"].InputTokens)
	assert.Empty(t, byKey["host-a.example/job-a"].SessionID)
	filter.From, filter.To = "2026-10-07", "2026-10-07"
	rows, err = store.GetTopSessionsByCost(t.Context(), filter, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(1_000_000), rows[0].Cost.Microdollars)
	filter.From, filter.To = "2026-10-07", "2026-10-10"
	filter.Machine = "host-b.example"
	rows, err = store.GetTopSessionsByCost(t.Context(), filter, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "host-b.example", rows[0].Machine)
	filter.Machine = ""
	filter.ProjectLabels = nil
	rows, err = store.GetTopSessionsByCost(t.Context(), filter, 100)
	require.NoError(t, err)
	require.Len(t, rows, 5)
	filter.Model = "absent-model"
	rows, err = store.GetTopSessionsByCost(t.Context(), filter, 100)
	require.NoError(t, err)
	assert.Empty(t, rows)
}
