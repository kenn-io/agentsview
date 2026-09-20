package clickhouse

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// Compile-time check: *Store satisfies db.Store.
var _ db.Store = (*Store)(nil)

// errNotImplemented marks db.Store methods the ClickHouse reader does not
// serve yet. The HTTP layer surfaces it as a server error; later tasks
// replace each stub with a real query.
var errNotImplemented = errors.New("not implemented for the ClickHouse backend")

// Store serves the read-only HTTP API from a ClickHouse mirror. Every
// connection carries final=1 so reads see one row per ReplacingMergeTree key
// without FINAL in the query text.
type Store struct {
	conn          *sql.DB
	cursorMu      sync.RWMutex
	cursorSecret  []byte
	customPricing map[string]config.CustomModelRate

	// vectorMu guards the searcher and reason installed by the serve gate.
	vectorMu                  sync.RWMutex
	vectorSearcher            db.VectorSearcher
	semanticUnavailableReason string
	closeOnce                 sync.Once
	closeErr                  error
	probeCache                activityProbeCache
}

// NewStore connects to the mirror named by t and refuses schemas or data
// versions this binary cannot serve.
func NewStore(ctx context.Context, t Target) (*Store, error) {
	conn, err := Open(ctx, t)
	if err != nil {
		return nil, err
	}
	if err := CheckSchemaCompat(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}
	if err := CheckDataVersionCompat(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}
	return NewStoreFromDB(conn), nil
}

// NewStoreFromDB wraps an already open connection. The caller owns the
// connection's compatibility checks.
func NewStoreFromDB(conn *sql.DB) *Store {
	return &Store{conn: conn}
}

// DB exposes the underlying connection for tests and status commands.
func (s *Store) DB() *sql.DB { return s.conn }

func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.conn.Close() })
	return s.closeErr
}

func (s *Store) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.conn.QueryContext(ctx, query, args...)
}

func (s *Store) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.conn.QueryRowContext(ctx, query, args...)
}

func (s *Store) ReadOnly() bool { return true }

func (s *Store) HasFTS(_ context.Context) bool { return true }

func (s *Store) SetCustomPricing(p map[string]config.CustomModelRate) {
	s.customPricing = p
}

func (s *Store) SetCursorSecret(secret []byte) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	s.cursorSecret = append([]byte(nil), secret...)
}

func (s *Store) EncodeCursor(c db.SessionCursor) string {
	data, _ := json.Marshal(c)
	s.cursorMu.RLock()
	secret := append([]byte(nil), s.cursorSecret...)
	s.cursorMu.RUnlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(data) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

func (s *Store) DecodeCursor(raw string) (db.SessionCursor, error) {
	parts := strings.Split(raw, ".")
	if len(parts) == 1 {
		data, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			return db.SessionCursor{}, fmt.Errorf("%w: %w", db.ErrInvalidCursor, err)
		}
		var c db.SessionCursor
		if err := json.Unmarshal(data, &c); err != nil {
			return db.SessionCursor{}, fmt.Errorf("%w: %w", db.ErrInvalidCursor, err)
		}
		c.Total = 0
		return c, nil
	}
	if len(parts) != 2 {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid format", db.ErrInvalidCursor)
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid payload: %w", db.ErrInvalidCursor, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid signature: %w", db.ErrInvalidCursor, err)
	}
	s.cursorMu.RLock()
	secret := append([]byte(nil), s.cursorSecret...)
	s.cursorMu.RUnlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return db.SessionCursor{}, fmt.Errorf("%w: signature mismatch", db.ErrInvalidCursor)
	}
	var c db.SessionCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid json: %w", db.ErrInvalidCursor, err)
	}
	return c, nil
}

// formatDBTime renders a scanned ClickHouse value as the RFC 3339 text the
// API returns. DateTime64 columns scan as time.Time; NULL scans as nil.
func formatDBTime(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		if t == nil {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

// Recall entries live only in the SQLite archive.

func (s *Store) ListRecallEntries(_ context.Context, _ db.RecallQuery) ([]db.RecallEntry, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) GetRecallEntry(_ context.Context, _ string) (*db.RecallEntry, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) QueryRecallEntries(_ context.Context, _ db.RecallQuery) (db.RecallPage, error) {
	return db.RecallPage{}, db.ErrReadOnly
}

func (s *Store) RecordRecallQueryEvent(_ context.Context, _ db.RecallQueryEvent) (string, error) {
	return "", db.ErrReadOnly
}

func (s *Store) InsertRecallEntry(_ context.Context, _ db.RecallEntry) (string, error) {
	return "", db.ErrReadOnly
}

func (s *Store) ImportAcceptedRecallEntriesJSONL(_ context.Context, _ io.Reader) (db.RecallImportResult, error) {
	return db.RecallImportResult{}, db.ErrReadOnly
}

func (s *Store) ImportAcceptedRecallEntriesJSONLWithOptions(
	_ context.Context, _ io.Reader, _ db.RecallImportOptions,
) (db.RecallImportResult, error) {
	return db.RecallImportResult{}, db.ErrReadOnly
}

func (s *Store) IngestEvalTrajectory(
	_ context.Context, _ db.EvalTrajectoryIngest,
) (db.EvalTrajectoryIngestResult, error) {
	return db.EvalTrajectoryIngestResult{}, db.ErrReadOnly
}
