package clickhouse

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// localSyncTimestampLayout is the layout ListSessionsForMirrorWindow
// compares sync_marker against.
const localSyncTimestampLayout = "2006-01-02T15:04:05.000Z"

// Sync pushes the SQLite archive into one ClickHouse mirror. Push is
// one-way: it never reads content back into SQLite.
type Sync struct {
	target          Target
	local           *db.DB
	machine         string
	projects        []string
	excludeProjects []string
	// vectorSource, when non-nil, enables the vector push phase.
	vectorSource storage.VectorPushSource

	connMu sync.Mutex
	conn   *sql.DB
	// schemaDone memoizes EnsureSchema for this Sync.
	schemaDone bool

	// archiveID caches the local archive identity stamped on every pushed
	// session row and used to scope the mirror's push cursor.
	archiveID string

	// pricer prices the current push's session batches with the catalog
	// snapshot syncUsagePrices loaded.
	pricer *usagePricer

	// hooks is nil in production; tests inject failures at push boundaries.
	hooks *pushHooks

	closeOnce sync.Once
	closeErr  error
}

// pushHooks exposes the push boundaries the consistency design depends on
// so tests can prove the recovery path without a real crash.
type pushHooks struct {
	// beforeSessionRows runs after a batch's dependent rows are written and
	// its stale rows deleted, before the session rows (and fingerprints)
	// land.
	beforeSessionRows func(batch []db.Session) error
}

// SyncStatus summarizes a mirror from its own metadata.
type SyncStatus struct {
	Machine         string `json:"machine"`
	LastPushAt      string `json:"last_push_at"`
	LastPushMachine string `json:"last_push_machine"`
	Scope           string `json:"scope"`
	SchemaVersion   int    `json:"schema_version"`
	DataVersion     int    `json:"data_version"`
	Sessions        int    `json:"clickhouse_sessions"`
	Messages        int    `json:"clickhouse_messages"`
	// SchemaMissing reports that the mirror database or tables do not exist
	// yet; every count is zero.
	SchemaMissing bool `json:"schema_missing,omitempty"`
}

// New prepares a push into target. It does not connect: EnsureSchema (or
// the first push) creates the database and tables, then connects.
func New(
	ctx context.Context, target Target, local *db.DB, machine string,
	opts storage.PusherOptions,
) (*Sync, error) {
	if local == nil {
		return nil, errors.New("clickhouse sync requires a local archive")
	}
	if _, err := target.DatabaseName(); err != nil {
		return nil, err
	}
	archiveID, err := local.GetArchiveID(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading local archive id: %w", err)
	}
	if archiveID == "" {
		return nil, errors.New("local archive has no archive id")
	}
	return &Sync{
		target:          target,
		local:           local,
		machine:         machine,
		projects:        append([]string(nil), opts.Projects...),
		excludeProjects: append([]string(nil), opts.ExcludeProjects...),
		vectorSource:    opts.VectorSource,
		archiveID:       archiveID,
	}, nil
}

// EnsureSchema creates the mirror database and tables and opens the
// connection every later call uses.
func (s *Sync) EnsureSchema(ctx context.Context) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.schemaDone && s.conn != nil {
		return nil
	}
	if err := EnsureSchema(ctx, s.target); err != nil {
		return err
	}
	if s.conn == nil {
		conn, err := Open(ctx, s.target)
		if err != nil {
			return err
		}
		s.conn = conn
	}
	s.schemaDone = true
	return nil
}

// Close releases the mirror connection.
func (s *Sync) Close() error {
	s.closeOnce.Do(func() {
		s.connMu.Lock()
		defer s.connMu.Unlock()
		if s.conn != nil {
			s.closeErr = s.conn.Close()
			s.conn = nil
		}
	})
	return s.closeErr
}

// Push runs one push. full forces every in-scope session to be rewritten.
func (s *Sync) Push(
	ctx context.Context, full bool, onProgress func(storage.PushProgress),
) (storage.PushResult, error) {
	return s.PushWithOptions(ctx, storage.PushOptions{Full: full}, onProgress)
}

func (s *Sync) isFiltered() bool {
	return len(s.projects) > 0 || len(s.excludeProjects) > 0
}

func (s *Sync) scopeString() string {
	return canonicalPushScope(s.projects, s.excludeProjects)
}

// canonicalPushScope renders the project filters so a scope change is
// detectable across pushes. Unfiltered is the empty string.
func canonicalPushScope(projects, excludeProjects []string) string {
	if len(projects) == 0 && len(excludeProjects) == 0 {
		return ""
	}
	scope := struct {
		Projects []string `json:"projects,omitempty"`
		Exclude  []string `json:"exclude,omitempty"`
	}{Projects: sortedCopy(projects), Exclude: sortedCopy(excludeProjects)}
	data, err := json.Marshal(scope)
	if err != nil {
		return ""
	}
	return string(data)
}

func sortedCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// Status reads this archive's push state and the mirror's row counts.
func (s *Sync) Status(ctx context.Context) (SyncStatus, error) {
	return ReadStatus(ctx, s.target, s.machine, s.archiveID, s.projects, s.excludeProjects)
}

// ReadStatus reads a mirror's status without needing the local archive
// beyond its archive id. A missing database or schema reports
// SchemaMissing instead of failing.
func ReadStatus(
	ctx context.Context, target Target, machine, archiveID string,
	projects, exclude []string,
) (SyncStatus, error) {
	status := SyncStatus{Machine: machine}
	conn, err := Open(ctx, target)
	if err != nil {
		if isMissingTableError(err) {
			status.SchemaMissing = true
			return status, nil
		}
		return status, err
	}
	defer conn.Close()
	meta, err := readMetadata(ctx, conn,
		schemaVersionKey, sourceDataVersionKey,
		archiveMetadataKey(lastPushAtKeyBase, archiveID),
		archiveMetadataKey(lastPushMachineKeyBase, archiveID),
		archiveMetadataKey(pushScopeKeyBase, archiveID),
	)
	if err != nil {
		if isMissingTableError(err) {
			status.SchemaMissing = true
			return status, nil
		}
		return status, err
	}
	status.SchemaVersion, _ = strconv.Atoi(meta[schemaVersionKey])
	status.DataVersion, _ = strconv.Atoi(meta[sourceDataVersionKey])
	status.LastPushAt = meta[archiveMetadataKey(lastPushAtKeyBase, archiveID)]
	status.LastPushMachine = meta[archiveMetadataKey(lastPushMachineKeyBase, archiveID)]
	status.Scope = meta[archiveMetadataKey(pushScopeKeyBase, archiveID)]
	sessionSQL := `SELECT COUNT(*) FROM sessions`
	messageSQL := `SELECT COUNT(*) FROM messages`
	var sessionArgs, messageArgs []any
	if pred, args := statusProjectPredicate(projects, exclude); pred != "" {
		sessionSQL += " WHERE " + pred
		messageSQL += " WHERE session_id IN (SELECT id FROM sessions WHERE " + pred + ")"
		sessionArgs = args
		messageArgs = args
	}
	if err := conn.QueryRowContext(ctx, sessionSQL, sessionArgs...).Scan(&status.Sessions); err != nil {
		return status, fmt.Errorf("counting clickhouse sessions: %w", err)
	}
	if err := conn.QueryRowContext(ctx, messageSQL, messageArgs...).Scan(&status.Messages); err != nil {
		return status, fmt.Errorf("counting clickhouse messages: %w", err)
	}
	return status, nil
}

func statusProjectPredicate(projects, exclude []string) (string, []any) {
	switch {
	case len(projects) > 0:
		ph, args := statusInPlaceholders(projects)
		return "project IN (" + ph + ")", args
	case len(exclude) > 0:
		ph, args := statusInPlaceholders(exclude)
		return "project NOT IN (" + ph + ")", args
	default:
		return "", nil
	}
}

func statusInPlaceholders(values []string) (string, []any) {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(values)), ","), args
}
