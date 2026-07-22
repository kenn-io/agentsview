package parser

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
)

// discoveryDiskMap is a short-lived, disk-backed lookup used when discovery
// metadata scales with archive cardinality. It avoids retaining project or
// membership maps in the daemon heap while preserving O(1) lookups.
type discoveryDiskMap struct {
	path       string
	db         *sql.DB
	remove     func(string) error
	queryError error
}

type discoveryDiskMapFaults struct {
	queryError   error
	cleanupError error
}

type discoveryDiskMapFaultsContextKey struct{}

const discoveryDiskMapForEachQuery = `
	SELECT key, value
	FROM entries
	ORDER BY key, ordinal
`

// WithDiscoveryDiskMapQueryError injects a disk-index query failure for
// end-to-end reconciliation tests. Production callers never attach this value.
func WithDiscoveryDiskMapQueryError(ctx context.Context, err error) context.Context {
	faults, _ := ctx.Value(discoveryDiskMapFaultsContextKey{}).(discoveryDiskMapFaults)
	faults.queryError = err
	return context.WithValue(ctx, discoveryDiskMapFaultsContextKey{}, faults)
}

// WithDiscoveryDiskMapCleanupError injects temporary-index removal failure so
// callers can prove cleanup errors keep reconciliation incomplete.
func WithDiscoveryDiskMapCleanupError(ctx context.Context, err error) context.Context {
	faults, _ := ctx.Value(discoveryDiskMapFaultsContextKey{}).(discoveryDiskMapFaults)
	faults.cleanupError = err
	return context.WithValue(ctx, discoveryDiskMapFaultsContextKey{}, faults)
}

func newDiscoveryDiskMapForContext(ctx context.Context) (*discoveryDiskMap, error) {
	index, err := newDiscoveryDiskMap()
	if err != nil {
		return nil, err
	}
	faults, _ := ctx.Value(discoveryDiskMapFaultsContextKey{}).(discoveryDiskMapFaults)
	index.queryError = faults.queryError
	if faults.cleanupError != nil {
		remove := index.remove
		index.remove = func(path string) error {
			if path == index.path {
				return faults.cleanupError
			}
			return remove(path)
		}
	}
	return index, nil
}

func newDiscoveryDiskMap() (*discoveryDiskMap, error) {
	file, err := os.CreateTemp("", "agentsview-discovery-map-*.db")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	database, err := sql.Open("sqlite3", "file:"+sqliteURIPath(path)+"?_journal_mode=OFF&_synchronous=OFF")
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec(`
		CREATE TABLE entries (
			key TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			value TEXT NOT NULL,
			PRIMARY KEY (key, ordinal)
		) WITHOUT ROWID;
		CREATE TRIGGER entries_replace_tail
		AFTER INSERT ON entries
		WHEN NEW.ordinal = 0
		BEGIN
			DELETE FROM entries
			WHERE key = NEW.key AND ordinal <> 0;
		END;
	`); err != nil {
		_ = database.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &discoveryDiskMap{path: path, db: database, remove: os.Remove}, nil
}

func (m *discoveryDiskMap) loadJSONL(
	ctx context.Context, path, keyField, valueField string,
) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO entries (key, ordinal, value)
		VALUES (?, 0, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		key := gjson.GetBytes(line, keyField).String()
		value := gjson.GetBytes(line, valueField).String()
		if key == "" || value == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, key, value); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan discovery metadata: %w", err)
	}
	return tx.Commit()
}

func (m *discoveryDiskMap) get(
	ctx context.Context, key string,
) (string, bool, error) {
	if m.queryError != nil {
		return "", false, m.queryError
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT value
		FROM entries
		WHERE key = ?
		ORDER BY ordinal
	`, key)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		return "", false, fmt.Errorf("read discovery index key: %w", err)
	}
	defer rows.Close()
	var value strings.Builder
	found := false
	for rows.Next() {
		var part string
		if err := rows.Scan(&part); err != nil {
			return "", false, err
		}
		if found {
			value.WriteByte('\n')
		}
		value.WriteString(part)
		found = true
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return value.String(), found, nil
}

func (m *discoveryDiskMap) put(ctx context.Context, key, value string, replace bool) error {
	if replace {
		_, err := m.db.ExecContext(
			ctx,
			`INSERT OR REPLACE INTO entries (key, ordinal, value)
			 VALUES (?, 0, ?)`,
			key,
			value,
		)
		return err
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO entries (key, ordinal, value)
		VALUES (?, 0, ?)
	`, key, value)
	return err
}

func (m *discoveryDiskMap) putIfAbsent(
	ctx context.Context, key, value string,
) (bool, error) {
	result, err := m.db.ExecContext(
		ctx, `
			INSERT OR IGNORE INTO entries (key, ordinal, value)
			VALUES (?, 0, ?)
		`, key, value,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (m *discoveryDiskMap) append(ctx context.Context, key, value string) error {
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO entries (key, ordinal, value)
		VALUES (
			?,
			COALESCE((
				SELECT ordinal + 1
				FROM entries
				WHERE key = ?
				ORDER BY ordinal DESC
				LIMIT 1
			), 0),
			?
		)
	`, key, key, value)
	return err
}

func (m *discoveryDiskMap) forEach(
	ctx context.Context, yield func(key, value string) error,
) error {
	if m.queryError != nil {
		return m.queryError
	}
	rows, err := m.db.QueryContext(ctx, discoveryDiskMapForEachQuery)
	if err != nil {
		return err
	}
	defer rows.Close()
	var currentKey string
	var value strings.Builder
	haveKey := false
	havePart := false
	yieldCurrent := func() error {
		observeStreamingDiscoveryBuffer(ctx, 1)
		return yield(currentKey, value.String())
	}
	for rows.Next() {
		var key, part string
		if err := rows.Scan(&key, &part); err != nil {
			return err
		}
		if haveKey && key != currentKey {
			if err := yieldCurrent(); err != nil {
				return err
			}
			value.Reset()
			havePart = false
		}
		if !haveKey || key != currentKey {
			currentKey = key
			haveKey = true
		}
		if havePart {
			value.WriteByte('\n')
		}
		value.WriteString(part)
		havePart = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !haveKey {
		return nil
	}
	return yieldCurrent()
}

func (m *discoveryDiskMap) addGeminiPath(
	ctx context.Context, absolutePath, name string, replace bool,
) error {
	project := ExtractProjectFromCwd(absolutePath)
	if project == "" {
		project = "unknown"
	}
	if err := m.put(ctx, geminiPathHash(absolutePath), project, replace); err != nil {
		return err
	}
	if name != "" {
		return m.put(ctx, name, project, replace)
	}
	return nil
}

func (m *discoveryDiskMap) loadGeminiConfig(ctx context.Context, root string) error {
	// Trusted folders establish fallbacks; named projects overwrite them,
	// matching BuildGeminiProjectMap's projects-first precedence.
	if err := m.decodeGeminiTrustedFolders(ctx, filepath.Join(root, "trustedFolders.json")); err != nil {
		return err
	}
	return m.decodeGeminiProjects(ctx, filepath.Join(root, "projects.json"))
}

func (m *discoveryDiskMap) decodeGeminiProjects(ctx context.Context, path string) error {
	return decodeJSONObjectField(path, "projects", func(dec *json.Decoder) error {
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			var name string
			if err := dec.Decode(&name); err != nil {
				return err
			}
			if err := m.addGeminiPath(ctx, key.(string), name, true); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	})
}

func (m *discoveryDiskMap) decodeGeminiTrustedFolders(ctx context.Context, path string) error {
	return decodeJSONArrayField(path, "trustedFolders", func(dec *json.Decoder) error {
		for dec.More() {
			var folder string
			if err := dec.Decode(&folder); err != nil {
				return err
			}
			if err := m.addGeminiPath(ctx, folder, "", false); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	})
}

func decodeJSONObjectField(
	path, field string, consume func(*json.Decoder) error,
) error {
	return decodeJSONField(path, field, json.Delim('{'), consume)
}

func decodeJSONArrayField(
	path, field string, consume func(*json.Decoder) error,
) error {
	return decodeJSONField(path, field, json.Delim('['), consume)
}

func decodeJSONField(
	path, field string, want json.Delim, consume func(*json.Decoder) error,
) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	if _, err := dec.Token(); err != nil {
		return err
	}
	for dec.More() {
		name, err := dec.Token()
		if err != nil {
			return err
		}
		if name != field {
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return err
			}
			continue
		}
		delim, err := dec.Token()
		if err != nil {
			return err
		}
		if delim != want {
			return fmt.Errorf("%s: unexpected %s shape", path, field)
		}
		return consume(dec)
	}
	return nil
}

func (m *discoveryDiskMap) close() error {
	if m == nil {
		return nil
	}
	var cleanupErr error
	if err := m.db.Close(); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close discovery index: %w", err))
	}
	remove := m.remove
	if remove == nil {
		remove = os.Remove
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := remove(m.path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr,
				fmt.Errorf("remove discovery index%s: %w", suffix, err))
		}
	}
	return cleanupErr
}
