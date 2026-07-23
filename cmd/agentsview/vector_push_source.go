// ABOUTME: pg push adapter exposing the local vectors.db active generation
// ABOUTME: to internal/postgres as a lazily-opened read-only VectorPushSource.
package main

import (
	"context"
	"errors"
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
// memoized; a missing file or open failure is never cached, so a daemon push
// that starts before embeddings exist picks up a build that lands afterward.
// BeginExport owns one SQLite read transaction for the complete push phase. The
// read-only handle stays open for the adapter's lifetime; the creator owns
// that lifetime and must release it via closeVectorPushSource — per push in
// PGPush, at loop exit in the watch path (which reuses one adapter across
// reconnects) — since postgres.Sync never closes its source.
type vectorPushSource struct {
	cfg config.Config

	mu sync.Mutex
	ix *vector.Index
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

func (s *vectorPushSource) BeginExport(
	ctx context.Context, sessionIDs []string,
) (postgres.VectorExport, bool, error) {
	ix, err := s.openIndex(ctx)
	if err != nil || ix == nil {
		return nil, false, err
	}
	exp, ok, err := ix.BeginExport(ctx, sessionIDs)
	if err != nil {
		if errors.Is(err, vector.ErrExportNotReady) {
			return nil, false, fmt.Errorf("%w: %v", postgres.ErrVectorSourceNotReady, err)
		}
		return nil, false, fmt.Errorf("beginning vector export: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	return &vectorPushExport{export: exp}, true, nil
}

type vectorPushExport struct{ export *vector.Export }

func (e *vectorPushExport) Generation() postgres.VectorGenerationInfo {
	exp := e.export.Generation()
	return postgres.VectorGenerationInfo{Fingerprint: exp.Fingerprint, Model: exp.Model, Dimension: exp.Dimension}
}

func (e *vectorPushExport) SessionDocHashes(ctx context.Context, ids []string) (map[string]string, error) {
	return e.export.SessionDocHashes(ctx, ids)
}

func (e *vectorPushExport) SessionDocs(ctx context.Context, id string) ([]postgres.VectorPushDoc, string, error) {
	docs, hash, err := e.export.SessionDocs(ctx, id)
	if err != nil {
		return nil, "", err
	}
	return convertVectorPushDocs(docs), hash, nil
}

func (e *vectorPushExport) Close() error { return e.export.Close() }

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
