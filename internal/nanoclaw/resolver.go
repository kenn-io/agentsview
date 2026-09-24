package nanoclaw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Resolver maps archived session paths to NanoClaw persona and channel. It
// caches the v2.db agent map and reloads it when the database or its WAL
// changes (spec §11.1). It is safe for concurrent use.
type Resolver struct {
	dataDirs []string // configured data dir, plus its symlink-resolved form
	dbPath   string
	filter   Filter

	mu     sync.Mutex
	loaded bool
	stamp  dbStamp
	agents AgentMap
	warned map[string]bool
}

// NewResolver builds a resolver for one cell. dbPath "" means
// <dataDir>/v2.db (nanoclaw.rs:82).
func NewResolver(dataDir, dbPath string, f Filter) *Resolver {
	clean := filepath.Clean(dataDir)
	if dbPath == "" {
		dbPath = filepath.Join(clean, "v2.db")
	}
	dirs := []string{clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && filepath.Clean(resolved) != clean {
		dirs = append(dirs, filepath.Clean(resolved))
	}
	return &Resolver{dataDirs: dirs, dbPath: dbPath, filter: f}
}

// Resolve returns the persona and channel for a session's source path.
// ok is false when the path is not a cell transcript; callers then leave
// the NanoClaw dimensions unset. excluded is true when the trust filter
// rejects the agent, or when a filter is configured and v2.db does not map
// the agent (fail closed, nanoclaw.rs:193-203); excluded sessions carry no
// persona or channel.
func (r *Resolver) Resolve(ctx context.Context, filePath string) (persona, channel string, excluded, ok bool) {
	id, found := r.agentID(filePath)
	if !found {
		return "", "", false, false
	}
	agents := r.currentMap(ctx)
	a, mapped := agents[id]
	if !mapped {
		if r.filter.Configured() {
			r.warnOnce(id)
			return "", "", true, true
		}
		a = Agent{ID: id, Persona: id, Folder: id}
	}
	if !r.filter.Allowed(a) {
		return "", "", true, true
	}
	return a.Persona, a.Channel, false, true
}

// SessionRoots are the <dataDir>/v2-sessions directories whose sessions
// this resolver answers for.
func (r *Resolver) SessionRoots() []string {
	roots := make([]string, 0, len(r.dataDirs))
	for _, d := range r.dataDirs {
		roots = append(roots, filepath.Join(d, "v2-sessions"))
	}
	return roots
}

// Fingerprint identifies the resolver's configuration and currently loaded
// agent map. Stored dims can be stale exactly when it changes.
func (r *Resolver) Fingerprint(ctx context.Context) string {
	agents := r.currentMap(ctx)
	h := sha256.New()
	fmt.Fprintf(h, "nanoclaw-dims-v1\x00%s\x00%s\x00%s\x00%s\x00",
		strings.Join(r.dataDirs, "\x01"), r.dbPath,
		strings.Join(r.filter.Include, "\x01"), strings.Join(r.filter.Exclude, "\x01"))
	for _, id := range slices.Sorted(maps.Keys(agents)) {
		a := agents[id]
		fmt.Fprintf(h, "%s\x01%s\x01%s\x01%s\x00", a.ID, a.Persona, a.Folder, a.Channel)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// agentID matches filePath against every spelling of the data dir. When
// that fails for a path that names a v2-sessions directory, it retries with
// the path's symlink-resolved form, so /var/… and /private/var/… (macOS) or
// a symlinked cell directory still match. Other paths cost no syscall.
func (r *Resolver) agentID(filePath string) (string, bool) {
	if id, ok := r.matchDataDirs(filePath); ok {
		return id, true
	}
	if !strings.Contains(filepath.ToSlash(filePath), "/v2-sessions/") {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(filePath)
	if err != nil || resolved == filePath {
		return "", false
	}
	return r.matchDataDirs(resolved)
}

func (r *Resolver) matchDataDirs(filePath string) (string, bool) {
	for _, dir := range r.dataDirs {
		if id, ok := AgentIDFromPath(dir, filePath); ok {
			return id, true
		}
	}
	return "", false
}

// dbStamp identifies one version of v2.db. NanoClaw writes through WAL, so
// the -wal file is part of the version.
type dbStamp struct {
	db, wal fileStamp
}

type fileStamp struct {
	exists  bool
	modTime time.Time
	size    int64
}

func statFile(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, modTime: info.ModTime(), size: info.Size()}
}

func (a fileStamp) equal(b fileStamp) bool {
	return a.exists == b.exists && a.size == b.size && a.modTime.Equal(b.modTime)
}

func (a dbStamp) equal(b dbStamp) bool { return a.db.equal(b.db) && a.wal.equal(b.wal) }

// currentMap returns the cached map, reloading it when v2.db changed. A
// load failure degrades to an empty map with a warning (nanoclaw.rs:103-115)
// and is cached for that version of the file, except when the failure came
// from the context: caching it would fail every filtered session closed
// until the file changed.
func (r *Resolver) currentMap(ctx context.Context) AgentMap {
	stamp := dbStamp{db: statFile(r.dbPath), wal: statFile(r.dbPath + "-wal")}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded && stamp.equal(r.stamp) {
		return r.agents
	}
	agents, err := LoadAgentMap(ctx, r.dbPath)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return AgentMap{}
		}
		log.Printf("nanoclaw: cannot read agent map from %s: %v; personas fall back to agent dir names", r.dbPath, err)
		agents = AgentMap{}
	}
	r.agents, r.stamp, r.loaded = agents, stamp, true
	r.warned = map[string]bool{}
	return agents
}

func (r *Resolver) warnOnce(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warned == nil {
		r.warned = map[string]bool{}
	}
	if r.warned[agentID] {
		return
	}
	r.warned[agentID] = true
	log.Printf("nanoclaw: agent %q has no v2.db mapping and a trust filter is configured; its sessions are excluded from review (fail closed)", agentID)
}
