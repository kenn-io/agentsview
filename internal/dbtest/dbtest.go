// Package dbtest provides shared test helpers for database
// setup and session seeding across test packages.
package dbtest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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
	if err := os.MkdirAll(
		filepath.Dir(path), 0o755,
	); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// MkdirTempWithCleanup creates a temporary directory and registers
// a cleanup that retries os.RemoveAll on Windows where SQLite WAL
// and mmap'd database files can remain briefly locked after the
// owning *sql.DB has been closed. A runtime.GC() runs first so
// any finalizer-driven stmt cleanup in mattn/go-sqlite3 releases
// its file handles before the directory removal is attempted.
func MkdirTempWithCleanup(t *testing.T, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	t.Cleanup(func() {
		runtime.GC()
		var removeErr error
		sleep := 25 * time.Millisecond
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			removeErr = os.RemoveAll(dir)
			if removeErr == nil {
				return
			}
			time.Sleep(sleep)
			if sleep < 500*time.Millisecond {
				sleep *= 2
			}
		}
		t.Errorf("removing temp dir %s: %v", dir, removeErr)
	})
	return dir
}

// OpenTestDB creates a temporary SQLite database for testing.
// The database is automatically closed when the test completes.
func OpenTestDB(t *testing.T) *db.DB {
	t.Helper()
	dir := MkdirTempWithCleanup(t, "agentsview-dbtest-*")
	path := filepath.Join(dir, "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// SeedMessages inserts messages into the database, failing the
// test on error.
func SeedMessages(t *testing.T, d *db.DB, msgs ...db.Message) {
	t.Helper()
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatalf("SeedMessages: %v", err)
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
	if err := d.UpsertSession(s); err != nil {
		t.Fatalf("SeedSession %s: %v", id, err)
	}
}

// WithMessageCount sets the session's total message count.
func WithMessageCount(n int) func(*db.Session) {
	return func(s *db.Session) { s.MessageCount = n }
}

// WithUserMessageCount sets the session's user message count.
func WithUserMessageCount(n int) func(*db.Session) {
	return func(s *db.Session) { s.UserMessageCount = n }
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
