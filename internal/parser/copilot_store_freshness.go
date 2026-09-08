package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

const copilotStoreVerifyInterval = 5 * time.Minute

var copilotTranscriptBytesRead atomic.Int64

// CopilotTranscriptBytesRead reports transcript payload work for sync scaling checks.
func CopilotTranscriptBytesRead() int64 { return copilotTranscriptBytesRead.Load() }

// Like the OpenCode factory index, this cache is shared by the short-lived
// providers of one engine. It caches producer inputs, never successful archive
// writes: only the engine's persisted fingerprint comparison permits a skip.
type copilotSourceCache struct {
	mu          sync.Mutex
	transcripts map[string]copilotTranscriptFingerprint
	stores      map[string]copilotStoreFingerprint
	// Work counters pin the amount of payload read by scaling regressions.
	transcriptBytes int64
	usageRows       int64
}

type copilotTranscriptFingerprint struct {
	Stat            uint64
	Size, Mtime     int64
	Hash, SessionID string
	UsesStore       bool
}

func (f copilotTranscriptFingerprint) encode() (string, error) {
	data, err := json.Marshal(f)
	return base64.RawURLEncoding.EncodeToString(data), err
}

func restoreCopilotTranscriptFingerprint(value string, stat uint64) (copilotTranscriptFingerprint, bool) {
	parts := strings.SplitN(value, ":", 4)
	if len(parts) != 4 || parts[0] != "copilot-session" || parts[1] != "v3" || stat == 0 {
		return copilotTranscriptFingerprint{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return copilotTranscriptFingerprint{}, false
	}
	var f copilotTranscriptFingerprint
	if json.Unmarshal(data, &f) != nil || f.Stat != stat {
		return copilotTranscriptFingerprint{}, false
	}
	return f, true
}

type copilotStoreMember struct {
	lastID int64
	hash   string
}

type copilotStoreFingerprint struct {
	state      SQLiteContainerState
	verifiedAt time.Time
	members    map[string]copilotStoreMember
}

func newCopilotSourceCache() *copilotSourceCache {
	return &copilotSourceCache{
		transcripts: make(map[string]copilotTranscriptFingerprint),
		stores:      make(map[string]copilotStoreFingerprint),
	}
}

func (c *copilotSourceCache) transcript(ctx context.Context, path string, info os.FileInfo, load StoredFingerprintLookup) (copilotTranscriptFingerprint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	workspace := copilotWorkspacePath(path)
	paths := []string{path}
	if workspace != "" {
		paths = append(paths, workspace)
	}
	stat := fileStatTupleDigest(0xC3, paths...)
	if prior, ok := c.transcripts[path]; ok && stat != 0 && prior.Stat == stat {
		return prior, nil
	}
	if load != nil && stat != 0 {
		if value, ok := load(path); ok {
			if prior, valid := restoreCopilotTranscriptFingerprint(value, stat); valid {
				c.transcripts[path] = prior
				return prior, nil
			}
		}
	}
	file, err := os.Open(path)
	if err != nil {
		delete(c.transcripts, path)
		return copilotTranscriptFingerprint{}, err
	}
	defer file.Close()
	c.transcriptBytes += info.Size()
	copilotTranscriptBytesRead.Add(info.Size())
	h := sha256.New()
	var started time.Time
	sessionID := ""
	lr := newLineReader(io.TeeReader(file, h), maxLineSize)
	defer releaseLineReader(lr)
	for {
		if err := ctx.Err(); err != nil {
			return copilotTranscriptFingerprint{}, err
		}
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}
		if started.IsZero() {
			started = parseTimestamp(gjson.Get(line, "timestamp").Str)
		}
		if gjson.Get(line, "type").Str == copilotEventSessionStart {
			if id := gjson.Get(line, "data.sessionId").Str; id != "" {
				sessionID = id
			}
		}
	}
	if err := lr.Err(); err != nil {
		return copilotTranscriptFingerprint{}, err
	}
	if sessionID == "" {
		sessionID = sessionIDFromPath(path)
	}
	if workspace != "" {
		data, err := os.ReadFile(workspace)
		if err != nil && !os.IsNotExist(err) {
			return copilotTranscriptFingerprint{}, err
		}
		fmt.Fprintf(h, "\x00workspace:%d\x00", len(data))
		h.Write(data)
	}
	size, mtime := CopilotCompositeFileStat(path, info)
	result := copilotTranscriptFingerprint{Stat: stat, Size: size, Mtime: mtime,
		Hash: fmt.Sprintf("%x", h.Sum(nil)), SessionID: sessionID,
		UsesStore: !started.Before(copilotUsageBasedPricingStartedAt)}
	c.transcripts[path] = result
	return result, nil
}

func (c *copilotSourceCache) usageHash(ctx context.Context, path, sessionID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, valid := StatSQLiteContainerState(path)
	if !valid {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			delete(c.stores, path)
			return "", nil
		}
	}
	prior, exists := c.stores[path]
	now := time.Now()
	full := !exists || now.Sub(prior.verifiedAt) >= copilotStoreVerifyInterval ||
		state.DBInode != prior.state.DBInode || state.DBDevice != prior.state.DBDevice
	if valid && exists && state == prior.state && !full {
		return prior.members[sessionID].hash, nil
	}
	store, err := sql.Open("sqlite3", "file:"+sqliteURIPath(path)+"?mode=ro&_busy_timeout=3000")
	if err != nil {
		return "", err
	}
	defer store.Close()
	tx, err := store.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	members := make(map[string]copilotStoreMember)
	if full || !valid {
		members, err = c.readUsageHashes(ctx, tx, "", nil)
	} else {
		// sessions is small metadata. The producer's (session_id, id) index makes
		// each MAX lookup bounded; do not aggregate every usage row on a WAL event.
		var rows *sql.Rows
		rows, err = tx.QueryContext(ctx, `SELECT id,
   COALESCE((SELECT MAX(id) FROM assistant_usage_events WHERE session_id = sessions.id), 0)
   FROM sessions`)
		if err == nil {
			for rows.Next() {
				var id string
				var lastID int64
				if err = rows.Scan(&id, &lastID); err != nil {
					break
				}
				if lastID != 0 {
					members[id] = copilotStoreMember{lastID: lastID}
				}
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
		}
		if err == nil {
			for id, member := range members {
				if old, ok := prior.members[id]; ok && old.lastID == member.lastID {
					members[id] = old
					continue
				}
				var changed map[string]copilotStoreMember
				changed, err = c.readUsageHashes(ctx, tx, "WHERE session_id = ?", []any{id})
				if err != nil {
					break
				}
				members[id] = changed[id]
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("fingerprinting copilot usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	verifiedAt := prior.verifiedAt
	if full || !valid {
		verifiedAt = now
	}
	// A failed or unreadable state is never trusted for an idle skip.
	if valid {
		c.stores[path] = copilotStoreFingerprint{state, verifiedAt, members}
	}
	return members[sessionID].hash, nil
}

func (c *copilotSourceCache) readUsageHashes(ctx context.Context, tx *sql.Tx, where string, args []any) (map[string]copilotStoreMember, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id, id, model,
  COALESCE(input_tokens,0), COALESCE(output_tokens,0), COALESCE(cache_read_tokens,0),
  COALESCE(cache_write_tokens,0), COALESCE(reasoning_tokens,0), created_at
  FROM assistant_usage_events `+where+` ORDER BY session_id, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]copilotStoreMember)
	var current string
	var h hash.Hash
	var lastID int64
	finish := func() {
		if h != nil {
			result[current] = copilotStoreMember{lastID, fmt.Sprintf("%x", h.Sum(nil))}
		}
	}
	for rows.Next() {
		var id int64
		var sessionID, model, timestamp string
		var input, output, read, write, reasoning int64
		if err := rows.Scan(&sessionID, &id, &model, &input, &output, &read, &write, &reasoning, &timestamp); err != nil {
			return nil, err
		}
		c.usageRows++
		if h == nil || sessionID != current {
			finish()
			current = sessionID
			h = sha256.New()
		}
		lastID = id
		fmt.Fprintf(h, "%d:%q:%d:%d:%d:%d:%d:%q\n", id, model, input, output, read, write, reasoning, timestamp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	finish()
	return result, nil
}

// Prune transcript hash entries during authoritative discovery, not on each
// store event. Deleted transcripts must not accumulate in the factory cache.
func (c *copilotSourceCache) pruneTranscripts(root string, seen map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for path := range c.transcripts {
		if copilotRootForEventsPath(path) == filepath.Clean(root) && !seen[path] {
			delete(c.transcripts, path)
		}
	}
}
