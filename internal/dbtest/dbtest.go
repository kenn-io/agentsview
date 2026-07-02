// Package dbtest provides shared test helpers for database
// setup and session seeding across test packages.
package dbtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
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
	return OpenTestDBAt(t, filepath.Join(dir, "test.db"))
}

// OpenTestDBAt opens a temporary SQLite database at path for testing, creating
// it from the shared current-schema template when it does not exist. The
// database is automatically closed when the test completes.
func OpenTestDBAt(t *testing.T, path string) *db.DB {
	t.Helper()
	EnsureTestDBAt(t, path)
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// EnsureTestDBAt creates a current-schema SQLite test database at path when it
// does not already exist. Existing files are left intact so callers can reopen
// and add more fixture rows without losing earlier writes.
func EnsureTestDBAt(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checking test db %s: %v", path, err)
	}
	if err := copyTestDBTemplate(path); err != nil {
		t.Fatalf("copying test db template: %v", err)
	}
}

var (
	testDBTemplateOnce  sync.Once
	testDBTemplateFiles map[string][]byte
	testDBTemplateErr   error
)

func copyTestDBTemplate(dst string) error {
	testDBTemplateOnce.Do(func() {
		testDBTemplateFiles, testDBTemplateErr = buildTestDBTemplate()
	})
	if testDBTemplateErr != nil {
		return testDBTemplateErr
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

func buildTestDBTemplate() (map[string][]byte, error) {
	dir, err := os.MkdirTemp("", "agentsview-dbtest-template-*")
	if err != nil {
		return nil, fmt.Errorf("creating db template dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "test.db")
	template, err := db.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening db template: %w", err)
	}
	// The deadline only guards against a hung checkpoint: flushing
	// the schema-creation WAL can take well over a second on slow
	// Windows CI disks under parallel package load.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	checkpointErr := template.CheckpointWALTruncate(ctx)
	closeErr := template.Close()
	if checkpointErr != nil {
		return nil, fmt.Errorf("checkpointing db template: %w", checkpointErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("closing db template: %w", closeErr)
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
