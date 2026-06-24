package git

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

// Cache is a small TTL-backed key-value store for git aggregation results.
//
// Reads and writes go through the provided *sql.DB. The caller is expected
// to have created the `git_cache` table (see internal/db/schema.sql) before
// using the cache; Cache itself never issues DDL.
type Cache struct {
	db    *sql.DB
	write bool
}

// NewCache wraps db as a git TTL cache. db must be a handle on a SQLite
// database that already contains the `git_cache` table.
func NewCache(db *sql.DB) *Cache {
	return &Cache{db: db, write: true}
}

// NewReadOnlyCache wraps db as a TTL cache that reads existing rows but does
// not attempt to persist freshly computed values. It is used by commands that
// intentionally open SQLite read-only.
func NewReadOnlyCache(db *sql.DB) *Cache {
	return &Cache{db: db}
}

// CacheKey returns a hex-encoded sha256 digest of the (kind, repo, author,
// since, until) tuple. Fields are encoded as JSON before hashing so any
// field can contain arbitrary bytes (including '|', tabs, or newlines)
// without colliding with another tuple. Any change to any field produces a
// different key.
func CacheKey(kind, repo, author, since, until string) string {
	encoded, err := json.Marshal([]string{
		kind, repo, author, since, until,
	})
	if err != nil {
		// json.Marshal cannot fail for []string; fall back to a
		// length-prefixed concatenation for robustness if it ever does.
		encoded = fmt.Appendf(nil,
			"%d:%s|%d:%s|%d:%s|%d:%s|%d:%s",
			len(kind), kind,
			len(repo), repo,
			len(author), author,
			len(since), since,
			len(until), until,
		)
	}
	h := sha256.Sum256(encoded)
	return hex.EncodeToString(h[:])
}

// GetOrCompute returns the cached payload for key when present and within
// ttl. On miss or stale entry, compute is invoked and its result is stored
// with INSERT OR REPLACE. compute is called at most once per invocation,
// and only on miss. Errors from compute propagate unchanged and do NOT
// write a cache row.
//
// kind is persisted alongside the payload as a debugging aid; it is not
// used for lookup (the key already encodes kind).
func (c *Cache) GetOrCompute(
	ctx context.Context,
	key, kind string,
	ttl time.Duration,
	compute func() ([]byte, error),
) ([]byte, error) {
	payload, ok, err := c.lookup(ctx, key, ttl)
	if err != nil {
		if !c.write && isMissingCacheTable(err) {
			return compute()
		}
		return nil, err
	}
	if ok {
		return payload, nil
	}

	fresh, err := compute()
	if err != nil {
		return nil, err
	}
	if !c.write {
		return fresh, nil
	}
	if err := c.store(ctx, key, kind, fresh); err != nil {
		return nil, fmt.Errorf("git_cache store: %w", err)
	}
	return fresh, nil
}

func isMissingCacheTable(err error) bool {
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code != sqlite3.ErrError {
		return false
	}
	return strings.Contains(err.Error(), "no such table: git_cache")
}

// lookup returns (payload, true, nil) when a fresh row exists, (nil, false,
// nil) on miss or stale entry, and (nil, false, err) on a query error.
// Rows whose computed_at cannot be parsed are treated as stale.
func (c *Cache) lookup(
	ctx context.Context, key string, ttl time.Duration,
) ([]byte, bool, error) {
	var payload, computedAt string
	err := c.db.QueryRowContext(
		ctx,
		`SELECT payload, computed_at FROM git_cache WHERE cache_key = ?`,
		key,
	).Scan(&payload, &computedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("git_cache lookup: %w", err)
	}
	t, parseErr := time.Parse(time.RFC3339Nano, computedAt)
	if parseErr != nil {
		// Malformed timestamp: treat as stale so compute runs and overwrites.
		return nil, false, nil
	}
	if time.Since(t) > ttl {
		return nil, false, nil
	}
	return []byte(payload), true, nil
}

// tokenIdentity returns a stable hex digest of ghToken suitable for
// inclusion in a cache key. The raw token is never written into the key —
// only this digest, which CacheKey hashes again. The result depends only
// on the token bytes, so the same account/token always produces the same
// identity slot.
func tokenIdentity(ghToken string) string {
	h := sha256.Sum256([]byte(ghToken))
	return hex.EncodeToString(h[:])
}

// store upserts a cache row with the current time as computed_at.
func (c *Cache) store(
	ctx context.Context, key, kind string, payload []byte,
) error {
	_, err := c.db.ExecContext(
		ctx,
		`INSERT OR REPLACE INTO git_cache
		 (cache_key, kind, payload, computed_at)
		 VALUES (?, ?, ?, ?)`,
		key, kind, string(payload),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

// AggregateLogCached wraps AggregateLog with a TTL-bounded read-through
// cache. The cache key is derived from ("log", repo, author, since, until).
// On a cache miss, AggregateLog is invoked; its LogResult is JSON-encoded
// before being stored. Errors from AggregateLog propagate unchanged.
func AggregateLogCached(
	ctx context.Context,
	cache *Cache,
	repo, author, since, until string,
	ttl time.Duration,
) (LogResult, error) {
	key := CacheKey("log", repo, author, since, until)
	payload, err := cache.GetOrCompute(
		ctx, key, "log", ttl,
		func() ([]byte, error) {
			res, err := AggregateLog(ctx, repo, author, since, until)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	)
	if err != nil {
		return LogResult{}, err
	}
	var out LogResult
	if err := json.Unmarshal(payload, &out); err != nil {
		return LogResult{}, fmt.Errorf("git_cache decode log payload: %w", err)
	}
	return out, nil
}

// AggregatePRsCached wraps AggregatePRs with a TTL-bounded cache.
//
// `--author=@me` resolves against the GitHub identity behind ghToken, so
// the token effectively partitions the result set. To prevent one
// account's PR counts from leaking into another's cache (after `gh auth
// switch` or a token swap), the cache key includes a SHA-256 digest of
// the token. The token itself never lands on disk — only its digest
// appears in the key, which is itself hashed again by CacheKey.
//
// When ghToken == "", AggregatePRs returns (nil, nil) and this wrapper
// mirrors that behavior without touching the cache — a nil PRResult is
// ambiguous ("gh unavailable") and we don't want to memoize it.
func AggregatePRsCached(
	ctx context.Context,
	cache *Cache,
	repo, since, until, ghToken string,
	ttl time.Duration,
) (*PRResult, error) {
	if ghToken == "" {
		return AggregatePRs(ctx, repo, since, until, ghToken)
	}
	key := CacheKey("pr", repo, tokenIdentity(ghToken), since, until)
	payload, err := cache.GetOrCompute(
		ctx, key, "pr", ttl,
		func() ([]byte, error) {
			res, err := AggregatePRs(ctx, repo, since, until, ghToken)
			if err != nil {
				return nil, err
			}
			if res == nil {
				// Defensive: AggregatePRs only returns nil when ghToken
				// is empty, which we short-circuit above. Guard anyway
				// so a future refactor can't accidentally cache nil.
				return nil, errors.New("git_cache: AggregatePRs returned nil with non-empty token")
			}
			return json.Marshal(res)
		},
	)
	if err != nil {
		return nil, err
	}
	var out PRResult
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("git_cache decode pr payload: %w", err)
	}
	return &out, nil
}
