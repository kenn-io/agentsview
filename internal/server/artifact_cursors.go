package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/artifact"
)

const (
	artifactCursorTTL  = 5 * time.Minute
	maxArtifactCursors = 64
)

type artifactCursorRegistry struct {
	mu     sync.Mutex
	tokens map[string]*artifactCursorLease
	active map[*artifactCursorLease]struct{}
	closed bool
}

type artifactCursorLease struct {
	registry        *artifactCursorRegistry
	cursor          *artifactSnapshotCursor
	scope           string
	token           string
	timer           *time.Timer
	constructCancel context.CancelFunc
	released        bool
}

type artifactSnapshotCursor struct {
	mu       sync.Mutex
	db       *sql.DB
	path     string
	scope    string
	lastKind int
	lastName string
	closing  bool
	inFlight int
	cleaned  bool
}

type artifactSnapshotItem struct {
	kind int
	name string
}

func newArtifactCursorRegistry() *artifactCursorRegistry {
	return &artifactCursorRegistry{
		tokens: make(map[string]*artifactCursorLease),
		active: make(map[*artifactCursorLease]struct{}),
	}
}

func (r *artifactCursorRegistry) buildSnapshot(
	ctx context.Context,
	root string,
	scope string,
	fill func(context.Context, func(int, string) error) error,
) (*artifactCursorLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	constructionCtx, cancel := context.WithCancel(ctx)
	lease, err := r.reserve(scope, cancel)
	if err != nil {
		cancel()
		return nil, err
	}
	cursor, err := newArtifactSnapshot(constructionCtx, root, scope,
		func(insert func(int, string) error) error {
			return fill(constructionCtx, insert)
		})
	if err != nil {
		lease.release()
		return nil, err
	}
	if err := r.attach(lease, cursor); err != nil {
		cursor.close()
		lease.release()
		return nil, err
	}
	return lease, nil
}

func (r *artifactCursorRegistry) reserve(
	scope string, cancel context.CancelFunc,
) (*artifactCursorLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fs.ErrClosed
	}
	if len(r.active) >= maxArtifactCursors {
		return nil, fmt.Errorf("%w: too many active artifact cursors", artifact.ErrArtifactConflict)
	}
	lease := &artifactCursorLease{
		registry:        r,
		scope:           scope,
		constructCancel: cancel,
	}
	r.active[lease] = struct{}{}
	return lease, nil
}

func (r *artifactCursorRegistry) attach(
	lease *artifactCursorLease, cursor *artifactSnapshotCursor,
) error {
	r.mu.Lock()
	if r.closed || lease == nil || lease.released {
		r.mu.Unlock()
		return fs.ErrClosed
	}
	if _, ok := r.active[lease]; !ok {
		r.mu.Unlock()
		return fs.ErrClosed
	}
	lease.cursor = cursor
	cancel := lease.constructCancel
	lease.constructCancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func newArtifactSnapshot(
	ctx context.Context,
	root string,
	scope string,
	fill func(func(int, string) error) error,
) (*artifactSnapshotCursor, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(root, ".peer-cursor-*.sqlite")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	database, err := sql.Open("sqlite3", path+"?_journal_mode=OFF&_synchronous=OFF&_temp_store=FILE")
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	cursor := &artifactSnapshotCursor{db: database, path: path, scope: scope, lastKind: -1}
	cleanup := true
	defer func() {
		if cleanup {
			cursor.close()
		}
	}()
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE items (kind INTEGER NOT NULL, name TEXT NOT NULL, PRIMARY KEY (kind, name)) WITHOUT ROWID`); err != nil {
		return nil, err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	statement, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO items (kind, name) VALUES (?, ?)`)
	if err != nil {
		return nil, err
	}
	err = fill(func(kind int, name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := statement.ExecContext(ctx, kind, name)
		return err
	})
	closeErr := statement.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	cleanup = false
	return cursor, nil
}

func (c *artifactSnapshotCursor) page(
	ctx context.Context, limit int,
) (items []artifactSnapshotItem, more bool, retErr error) {
	database, lastKind, lastName, err := c.beginPage()
	if err != nil {
		return nil, false, err
	}
	defer func() {
		closing, cleanupDB, cleanupPath := c.finishPage()
		cleanupArtifactSnapshot(cleanupDB, cleanupPath)
		if closing {
			items = nil
			more = false
			retErr = errors.Join(retErr, fs.ErrClosed)
		}
	}()

	rows, err := database.QueryContext(ctx, `
		SELECT kind, name
		FROM items
		WHERE kind > ? OR (kind = ? AND name > ?)
		ORDER BY kind, name
		LIMIT ?`, lastKind, lastKind, lastName, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items = make([]artifactSnapshotItem, 0, limit+1)
	for rows.Next() {
		var item artifactSnapshotItem
		if err := rows.Scan(&item.kind, &item.name); err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more = len(items) > limit
	if more {
		items = items[:limit]
	}
	if len(items) > 0 {
		last := items[len(items)-1]
		c.mu.Lock()
		if c.closing {
			c.mu.Unlock()
			return nil, false, fs.ErrClosed
		}
		c.lastKind = last.kind
		c.lastName = last.name
		c.mu.Unlock()
	}
	return items, more, nil
}

func (c *artifactSnapshotCursor) beginPage() (*sql.DB, int, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.db == nil {
		return nil, 0, "", fs.ErrClosed
	}
	if c.inFlight != 0 {
		return nil, 0, "", fmt.Errorf("%w: artifact cursor page already active", artifact.ErrArtifactConflict)
	}
	c.inFlight++
	return c.db, c.lastKind, c.lastName, nil
}

func (c *artifactSnapshotCursor) finishPage() (bool, *sql.DB, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
	if !c.closing || c.inFlight != 0 {
		return c.closing, nil, ""
	}
	database, path := c.takeCleanupLocked()
	return true, database, path
}

func (c *artifactSnapshotCursor) close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closing = true
	var database *sql.DB
	var path string
	if c.inFlight == 0 {
		database, path = c.takeCleanupLocked()
	}
	c.mu.Unlock()
	cleanupArtifactSnapshot(database, path)
}

func (c *artifactSnapshotCursor) takeCleanupLocked() (*sql.DB, string) {
	if c.cleaned {
		return nil, ""
	}
	c.cleaned = true
	database := c.db
	c.db = nil
	return database, c.path
}

func cleanupArtifactSnapshot(database *sql.DB, path string) {
	if database != nil {
		_ = database.Close()
	}
	if path == "" {
		return
	}
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

func artifactCursorToken() (string, error) {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

func (r *artifactCursorRegistry) claim(token, scope string) (*artifactCursorLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fs.ErrClosed
	}
	lease, ok := r.tokens[token]
	if !ok || lease.scope != scope || lease.released || lease.cursor == nil {
		return nil, fmt.Errorf("%w: invalid or expired artifact cursor", artifact.ErrArtifactInvalid)
	}
	delete(r.tokens, token)
	lease.token = ""
	if lease.timer != nil {
		lease.timer.Stop()
		lease.timer = nil
	}
	return lease, nil
}

func (r *artifactCursorRegistry) retain(lease *artifactCursorLease) (string, error) {
	token, err := artifactCursorToken()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", fs.ErrClosed
	}
	if lease == nil || lease.released || lease.cursor == nil || lease.token != "" {
		r.mu.Unlock()
		return "", fs.ErrClosed
	}
	if _, ok := r.active[lease]; !ok {
		r.mu.Unlock()
		return "", fs.ErrClosed
	}
	r.tokens[token] = lease
	lease.token = token
	lease.timer = time.AfterFunc(artifactCursorTTL, func() {
		r.release(token)
	})
	r.mu.Unlock()
	return token, nil
}

func (r *artifactCursorRegistry) release(token string) bool {
	r.mu.Lock()
	lease, ok := r.tokens[token]
	if !ok || lease.token != token || lease.released {
		r.mu.Unlock()
		return false
	}
	cursor, cancel := r.releaseLeaseLocked(lease)
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cursor != nil {
		cursor.close()
	}
	return true
}

func (l *artifactCursorLease) release() {
	if l == nil || l.registry == nil {
		return
	}
	l.registry.mu.Lock()
	if l.released {
		l.registry.mu.Unlock()
		return
	}
	cursor, cancel := l.registry.releaseLeaseLocked(l)
	l.registry.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cursor != nil {
		cursor.close()
	}
}

func (r *artifactCursorRegistry) releaseLeaseLocked(
	lease *artifactCursorLease,
) (*artifactSnapshotCursor, context.CancelFunc) {
	lease.released = true
	delete(r.active, lease)
	if lease.token != "" {
		delete(r.tokens, lease.token)
		lease.token = ""
	}
	if lease.timer != nil {
		lease.timer.Stop()
		lease.timer = nil
	}
	cursor := lease.cursor
	lease.cursor = nil
	cancel := lease.constructCancel
	lease.constructCancel = nil
	return cursor, cancel
}

func (l *artifactCursorLease) page(
	ctx context.Context, limit int,
) ([]artifactSnapshotItem, bool, error) {
	if l == nil || l.registry == nil {
		return nil, false, fs.ErrClosed
	}
	l.registry.mu.Lock()
	if l.released || l.cursor == nil {
		l.registry.mu.Unlock()
		return nil, false, fs.ErrClosed
	}
	cursor := l.cursor
	l.registry.mu.Unlock()
	return cursor.page(ctx, limit)
}

func (r *artifactCursorRegistry) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	cursors := make([]*artifactSnapshotCursor, 0, len(r.active))
	cancels := make([]context.CancelFunc, 0, len(r.active))
	for lease := range r.active {
		cursor, cancel := r.releaseLeaseLocked(lease)
		if cursor != nil {
			cursors = append(cursors, cursor)
		}
		if cancel != nil {
			cancels = append(cancels, cancel)
		}
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, cursor := range cursors {
		cursor.close()
	}
}

func finishArtifactSnapshotPage(
	ctx context.Context,
	registry *artifactCursorRegistry,
	lease *artifactCursorLease,
	limit int,
) ([]artifactSnapshotItem, string, error) {
	items, more, err := lease.page(ctx, limit)
	if err != nil {
		lease.release()
		return nil, "", err
	}
	if !more {
		lease.release()
		return items, "", nil
	}
	next, err := registry.retain(lease)
	if err != nil {
		lease.release()
		return nil, "", err
	}
	return items, next, nil
}
