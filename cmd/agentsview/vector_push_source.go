// ABOUTME: pg push adapter exposing the local vectors.db active generation
// ABOUTME: to internal/postgres as a lazily-opened read-only VectorPushSource.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/vector"
)

// vectorPushSource adapts a read-only vector.Index to
// postgres.VectorPushSource, opening vectors.db lazily so a pg push whose
// target has vectors disabled never touches the file. Only a successful open is
// memoized so the three interface methods of one push share one connection; a
// missing file or open failure is never cached, so a daemon push that starts
// before embeddings exist picks up a build that lands afterward. Generation
// snapshots the active generation and SessionDocHashes/SessionDocs read the
// snapshotted ordinal, so a build that activates mid-push cannot pair this
// generation's fingerprint with a newer generation's docs; the next Generation
// call refreshes the snapshot, keeping successive daemon pushes current. The
// read-only handle stays open for the adapter's lifetime; the creator owns
// that lifetime and must release it via closeVectorPushSource — per push in
// PGPush, at loop exit in the watch path (which reuses one adapter across
// reconnects) — since postgres.Sync never closes its source.
type vectorPushSource struct {
	cfg config.Config

	mu sync.Mutex
	ix *vector.Index

	// snap/snapOK hold the active generation captured by the most recent
	// Generation call; SessionDocHashes and SessionDocs read snap.Ordinal.
	snap   vector.ActiveExport
	snapOK bool
}

// newVectorPushSource returns a lazy vectors.db push adapter, or nil when
// [vector] is disabled: there is then no vectors.db to open and nothing to
// push, and a nil source leaves postgres.Sync's vector phase skipped.
func newVectorPushSource(appCfg config.Config) postgres.VectorPushSource {
	if !appCfg.Vector.Enabled {
		return nil
	}
	return &vectorPushSource{cfg: appCfg}
}

// closeVectorPushSource releases a push source's memoized read-only
// vectors.db handle. postgres.Sync never closes its source — the creator
// owns the handle's lifetime — so every call site that builds a source must
// close it when the push (or watch loop) is done, or repeated pushes leak
// one SQLite handle each. Safe on a nil source and on sources holding no
// handle.
func closeVectorPushSource(src postgres.VectorPushSource) {
	c, ok := src.(io.Closer)
	if !ok {
		return
	}
	if err := c.Close(); err != nil {
		log.Printf("closing vectors.db push source: %v", err)
	}
}

// openIndex opens vectors.db read-only and memoizes only a successful open. A
// missing file (no build yet) and any open failure are NOT cached, so a later
// call — a daemon push that starts before embeddings exist — reopens and picks
// up a build that lands afterward. A nil index without error means the file is
// absent ("nothing to push"); the pointer's own nil checks gate every
// dereference.
func (s *vectorPushSource) openIndex(ctx context.Context) (*vector.Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ix != nil {
		return s.ix, nil
	}
	path := s.cfg.Vector.ResolvedDBPath(s.cfg.DataDir)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat vectors.db: %w", err)
	}
	ix, err := vector.Open(ctx, path, true, s.cfg.Vector.Embeddings.MaxInputChars)
	if err != nil {
		return nil, fmt.Errorf("opening vectors.db: %w", err)
	}
	s.ix = ix
	return ix, nil
}

// Close releases the memoized read-only vectors.db handle, if one was opened.
// Safe to call multiple times; a later interface call reopens the file via
// openIndex's uncached-miss path.
func (s *vectorPushSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ix == nil {
		return nil
	}
	err := s.ix.Close()
	s.ix = nil
	return err
}

// setSnapshot records the active generation captured by Generation.
func (s *vectorPushSource) setSnapshot(exp vector.ActiveExport, ok bool) {
	s.mu.Lock()
	s.snap, s.snapOK = exp, ok
	s.mu.Unlock()
}

// snapshot returns the active generation captured by the most recent
// Generation call, and whether one is set.
func (s *vectorPushSource) snapshot() (vector.ActiveExport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap, s.snapOK
}

// Generation implements postgres.VectorPushSource and snapshots the active
// generation for the rest of the push. Contract: Generation must be called
// before SessionDocHashes and SessionDocs, which read the snapshotted ordinal;
// the postgres push (pushVectors) calls Generation once first, then again as
// the pre-eviction safety re-check. Snapshotting here means a build that
// activates mid-push cannot mix this generation's fingerprint with a newer
// generation's docs, while the next Generation call refreshes the snapshot so
// successive pushes stay current.
//
// A generation with missing embeddings is refused with an error wrapping
// postgres.ErrVectorSourceNotReady rather than exported: the mirror only
// changes during builds, so missing coverage means a build is rewriting the
// generation right now (a same-fingerprint full rebuild clears and refills it
// in place) or one was interrupted. Exporting that partial view would evict
// or overwrite valid PG vectors; the push after the build completes sends
// everything that changed.
func (s *vectorPushSource) Generation(
	ctx context.Context,
) (postgres.VectorGenerationInfo, bool, error) {
	ix, err := s.openIndex(ctx)
	if err != nil || ix == nil {
		s.setSnapshot(vector.ActiveExport{}, false)
		return postgres.VectorGenerationInfo{}, false, err
	}
	exp, ok, err := ix.ActiveExport(ctx)
	if err != nil {
		s.setSnapshot(vector.ActiveExport{}, false)
		return postgres.VectorGenerationInfo{}, false,
			fmt.Errorf("reading active generation: %w", err)
	}
	if !ok {
		s.setSnapshot(vector.ActiveExport{}, false)
		return postgres.VectorGenerationInfo{}, false, nil
	}
	info, err := ix.GenerationByID(ctx, exp.Ordinal)
	if err != nil {
		s.setSnapshot(vector.ActiveExport{}, false)
		return postgres.VectorGenerationInfo{}, false,
			fmt.Errorf("reading active generation coverage: %w", err)
	}
	if info.Missing > 0 {
		s.setSnapshot(vector.ActiveExport{}, false)
		return postgres.VectorGenerationInfo{}, false, fmt.Errorf(
			"%w: %d document(s) pending",
			postgres.ErrVectorSourceNotReady, info.Missing)
	}
	s.setSnapshot(exp, true)
	return postgres.VectorGenerationInfo{
		Fingerprint: exp.Fingerprint,
		Model:       exp.Model,
		Dimension:   exp.Dimension,
	}, true, nil
}

// SessionDocHashes implements postgres.VectorPushSource, reading the ordinal
// snapshotted by Generation. An absent file or no active snapshot yields an
// empty (non-nil) map: no local sessions, so the push evicts any PG state this
// pusher previously owned.
func (s *vectorPushSource) SessionDocHashes(
	ctx context.Context,
) (map[string]string, error) {
	ix, err := s.openIndex(ctx)
	if err != nil {
		return nil, err
	}
	exp, ok := s.snapshot()
	if ix == nil || !ok {
		return map[string]string{}, nil
	}
	return ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal)
}

// SessionDocs implements postgres.VectorPushSource, exporting one session's
// docs at the ordinal snapshotted by Generation and mapping each mirror doc and
// its chunk vectors field-by-field onto the push types. The returned hash is
// the aggregate of exactly the exported doc set, computed inside the export's
// own read snapshot; the push defers the session when it no longer matches the
// delta-scan hash.
func (s *vectorPushSource) SessionDocs(
	ctx context.Context, sessionID string,
) ([]postgres.VectorPushDoc, string, error) {
	ix, err := s.openIndex(ctx)
	if err != nil {
		return nil, "", err
	}
	exp, ok := s.snapshot()
	if ix == nil || !ok {
		return nil, "", nil
	}
	docs, aggHash, err := ix.ExportSessionDocs(ctx, exp.Ordinal, sessionID)
	if err != nil {
		return nil, "", err
	}
	return convertVectorPushDocs(docs), aggHash, nil
}

// convertVectorPushDocs copies exported mirror docs onto the backend-agnostic
// push types. It is a mechanical field-by-field copy, isolated here so the
// mapping is testable against the raw export API.
func convertVectorPushDocs(docs []vector.ExportDoc) []postgres.VectorPushDoc {
	out := make([]postgres.VectorPushDoc, len(docs))
	for i, d := range docs {
		chunks := make([]postgres.VectorPushChunk, len(d.Chunks))
		for j, c := range d.Chunks {
			chunks[j] = postgres.VectorPushChunk{
				ChunkIndex: c.ChunkIndex,
				Embedding:  c.Embedding,
			}
		}
		out[i] = postgres.VectorPushDoc{
			DocKey:      d.DocKey,
			SessionID:   d.SessionID,
			SourceUUID:  d.SourceUUID,
			Ordinal:     d.Ordinal,
			OrdinalEnd:  d.OrdinalEnd,
			Subordinate: d.Subordinate,
			OffsetsJSON: d.OffsetsJSON,
			Content:     d.Content,
			ContentHash: d.ContentHash,
			Chunks:      chunks,
		}
	}
	return out
}
