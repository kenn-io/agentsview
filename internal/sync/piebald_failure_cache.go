package sync

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/mattn/go-sqlite3"

	"go.kenn.io/agentsview/internal/parser"
)

type piebaldFailureIdentity struct {
	dbPath     string
	dbSize     int64
	dbMtimeNS  int64
	walSize    int64
	walMtimeNS int64
}

type piebaldFailureMemoEntry struct {
	identity    piebaldFailureIdentity
	err         error
	retryNeeded bool
}

type piebaldFailureLookup struct {
	key        string
	identity   piebaldFailureIdentity
	identityOK bool
	err        error
	retry      bool
}

func (e *Engine) preparePiebaldFailure(
	ctx context.Context, source parser.SourceRef, force bool,
) (piebaldFailureLookup, bool) {
	if e.forceParse {
		return piebaldFailureLookup{}, false // Report-only parse-diff leaves caches untouched.
	}
	key, dbPath, ok := piebaldFailureSourcePaths(source)
	if !ok {
		return piebaldFailureLookup{}, false
	}
	failureKey := providerAgentSkipCacheKey(key, parser.AgentPiebald)
	_, durable := e.failures.Lookup(failureKey)
	e.skipMu.RLock()
	_, remembered := e.piebaldFailureMemo[key]
	e.skipMu.RUnlock()
	retry := durable
	if force || (durable || remembered) && e.pathNeedsCachedSkipBypass(ctx, parser.AgentPiebald, key) {
		retry = durable || remembered
		e.clearPiebaldFailure(source)
		e.failures.Clear(failureKey)
	}
	if retry {
		// A persisted failure must reach the fingerprint check before archive
		// freshness shortcuts, including after a restart. Keep retry intent if
		// stat, fingerprinting, or a transient parse fails along the way.
		e.skipMu.Lock()
		if e.piebaldFailureMemo == nil {
			e.piebaldFailureMemo = make(map[string]piebaldFailureMemoEntry)
		}
		entry := e.piebaldFailureMemo[key]
		entry.err = nil
		entry.retryNeeded = true
		e.piebaldFailureMemo[key] = entry
		e.skipMu.Unlock()
	}
	identity, err := e.capturePiebaldFailureIdentity(dbPath)
	if err != nil {
		e.skipMu.Lock()
		entry, found := e.piebaldFailureMemo[key]
		if found {
			entry.err = nil
			entry.retryNeeded = true
			e.piebaldFailureMemo[key] = entry
		}
		e.skipMu.Unlock()
		return piebaldFailureLookup{key: key, retry: found}, found
	}
	lookup := piebaldFailureLookup{
		key:        key,
		identity:   identity,
		identityOK: true,
	}

	e.skipMu.Lock()
	entry, found := e.piebaldFailureMemo[key]
	if found && entry.identity != identity {
		e.piebaldFailureMemo[key] = piebaldFailureMemoEntry{
			identity:    identity,
			retryNeeded: true,
		}
		lookup.retry = true
	} else if found {
		lookup.err = entry.err
		lookup.retry = entry.retryNeeded
	}
	e.skipMu.Unlock()
	return lookup, true
}

func (e *Engine) capturePiebaldFailureIdentity(
	dbPath string,
) (piebaldFailureIdentity, error) {
	stat := os.Stat
	if e != nil && e.stat != nil {
		stat = e.stat
	}
	dbInfo, err := stat(dbPath)
	if err != nil {
		return piebaldFailureIdentity{}, err
	}
	if dbInfo == nil {
		return piebaldFailureIdentity{}, errors.New("piebald source stat returned no file info")
	}
	walInfo, err := stat(dbPath + "-wal")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return piebaldFailureIdentity{}, err
	}
	if walInfo == nil && err == nil {
		return piebaldFailureIdentity{}, errors.New("piebald WAL stat returned no file info")
	}
	var walSize, walMtimeNS int64
	if walInfo != nil {
		walSize = walInfo.Size()
		walMtimeNS = walInfo.ModTime().UnixNano()
	}
	return piebaldFailureIdentity{
		dbPath:     filepath.Clean(dbPath),
		dbSize:     dbInfo.Size(),
		dbMtimeNS:  dbInfo.ModTime().UnixNano(),
		walSize:    walSize,
		walMtimeNS: walMtimeNS,
	}, nil
}

func piebaldFailureSourcePaths(
	source parser.SourceRef,
) (virtualPath, dbPath string, ok bool) {
	if source.Provider != parser.AgentPiebald {
		return "", "", false
	}
	virtualPath = providerDiscoveredPath(source)
	dbPath, sessionID, ok := parser.ParseVirtualSourcePathForBase(
		virtualPath, parser.PiebaldDBFilename,
	)
	if !ok {
		return "", "", false
	}
	dbPath = filepath.Clean(dbPath)
	return parser.VirtualSourcePath(dbPath, sessionID), dbPath, true
}

func (e *Engine) rememberPiebaldParseFailure(
	ctx context.Context,
	source parser.SourceRef,
	pre piebaldFailureLookup,
	parseErr error,
) {
	if parseErr == nil || !pre.identityOK {
		return
	}
	key, dbPath, ok := piebaldFailureSourcePaths(source)
	if !ok || key != pre.key {
		return
	}
	post, err := e.capturePiebaldFailureIdentity(dbPath)
	if err != nil {
		return
	}
	if pre.identity != post {
		e.setPiebaldRetry(key, post)
		return
	}
	if piebaldFailureIsTransient(ctx, parseErr) {
		return
	}

	e.skipMu.Lock()
	if e.piebaldFailureMemo == nil {
		e.piebaldFailureMemo = make(map[string]piebaldFailureMemoEntry)
	}
	e.piebaldFailureMemo[key] = piebaldFailureMemoEntry{
		identity: post,
		err:      parseErr,
	}
	e.skipMu.Unlock()
}

func (e *Engine) setPiebaldRetry(
	key string, identity piebaldFailureIdentity,
) {
	e.skipMu.Lock()
	if e.piebaldFailureMemo == nil {
		e.piebaldFailureMemo = make(map[string]piebaldFailureMemoEntry)
	}
	e.piebaldFailureMemo[key] = piebaldFailureMemoEntry{
		identity:    identity,
		retryNeeded: true,
	}
	e.skipMu.Unlock()
}

func (e *Engine) clearPiebaldFailure(source parser.SourceRef) {
	key, _, ok := piebaldFailureSourcePaths(source)
	if !ok {
		return
	}
	e.skipMu.Lock()
	delete(e.piebaldFailureMemo, key)
	e.skipMu.Unlock()
}

func (e *Engine) clearPiebaldFailureMemo() {
	e.skipMu.Lock()
	e.piebaldFailureMemo = make(map[string]piebaldFailureMemoEntry)
	e.skipMu.Unlock()
}

func (e *Engine) prunePiebaldFailures(
	roots []string, discovered map[string]struct{},
) {
	e.skipMu.Lock()
	defer e.skipMu.Unlock()
	for key := range e.piebaldFailureMemo {
		dbPath, _, ok := parser.ParseVirtualSourcePathForBase(
			key, parser.PiebaldDBFilename,
		)
		if !ok || !piebaldPathWithinRoots(dbPath, roots) {
			continue
		}
		if _, found := discovered[key]; !found {
			delete(e.piebaldFailureMemo, key)
		}
	}
}

func piebaldAuthoritativeRoots(
	roots []string, stat func(string) (os.FileInfo, error),
) []string {
	authoritative := make([]string, 0, len(roots))
	for _, root := range roots {
		_, err := stat(filepath.Join(root, parser.PiebaldDBFilename))
		if err == nil || errors.Is(err, os.ErrNotExist) {
			authoritative = append(authoritative, root)
		}
	}
	return authoritative
}

func piebaldPathWithinRoots(path string, roots []string) bool {
	path = filepath.Clean(path)
	for _, root := range roots {
		if filepath.Clean(filepath.Join(root, parser.PiebaldDBFilename)) == path {
			return true
		}
	}
	return false
}

func piebaldFailureIsTransient(ctx context.Context, err error) bool {
	if err == nil {
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrPermission) || os.IsTimeout(err) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, sql.ErrNoRows) ||
		errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, os.ErrNotExist) {
		return true
	}
	if _, ok := errors.AsType[*os.PathError](err); ok {
		return true
	}

	if sqliteErr, ok := errors.AsType[sqlite3.Error](err); ok {
		return piebaldSQLiteErrorIsTransient(sqliteErr)
	}
	if sqliteErrPtr, ok := errors.AsType[*sqlite3.Error](err); ok &&
		sqliteErrPtr != nil {
		return piebaldSQLiteErrorIsTransient(*sqliteErrPtr)
	}
	return false
}

func piebaldSQLiteErrorIsTransient(err sqlite3.Error) bool {
	switch err.Code {
	case sqlite3.ErrBusy, sqlite3.ErrLocked, sqlite3.ErrInterrupt,
		sqlite3.ErrIoErr, sqlite3.ErrFull,
		sqlite3.ErrNomem:
		return true
	default:
		return false
	}
}
