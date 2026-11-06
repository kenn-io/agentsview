// ABOUTME: replica push adapter exposing the local vectors.db active generation
// ABOUTME: to replica pushes as a lazily-opened read-only storage.VectorPushSource.
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
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
)

// vectorPushSource opens the configured vectors.db for each export. The export's
// snapshot owns its read transaction, so a later export can use a new path while
// an existing push finishes reading its original database.
type vectorPushSource struct {
	cfg     config.Config
	adopted bool

	mu sync.Mutex
}

// newVectorPushSource returns a lazy vectors.db push adapter, or nil when
// [vector] is disabled: there is then no vectors.db to open and nothing to
// push, and a nil source leaves the pusher's vector phase skipped.
func newVectorPushSource(appCfg config.Config) storage.VectorPushSource {
	if appCfg.ArchiveContent.UsageOnly() || !appCfg.Vector.Enabled {
		return nil
	}
	return &vectorPushSource{cfg: appCfg}
}

// closeVectorPushSource releases sources that own background builders.
func closeVectorPushSource(src storage.VectorPushSource) {
	c, ok := src.(io.Closer)
	if !ok {
		return
	}
	if err := c.Close(); err != nil {
		log.Printf("closing vectors.db push source: %v", err)
	}
}

// openIndex returns nil when vectors are disabled or the database is absent.
func (s *vectorPushSource) openIndex(ctx context.Context) (*vector.Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cfg.Vector.Enabled {
		return nil, nil
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
	return ix, nil
}

func (s *vectorPushSource) BeginExport(
	ctx context.Context, sessionIDs []string,
) (storage.VectorExport, bool, error) {
	ix, err := s.openIndex(ctx)
	if err != nil || ix == nil {
		return nil, false, err
	}
	// Closing the pool leaves the export transaction usable until exp.Close.
	defer func() { _ = ix.Close() }()
	exp, ok, err := ix.BeginExport(ctx, sessionIDs)
	if err != nil {
		if errors.Is(err, vector.ErrExportNotReady) {
			return nil, false, fmt.Errorf("%w: %w", storage.ErrVectorSourceNotReady, err)
		}
		return nil, false, fmt.Errorf("beginning vector export: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	// Publish the recipe only for the generation the configured recipe
	// produces: params must rebuild the fingerprint they are stored with.
	out := &vectorPushExport{export: exp}
	if configured := s.configuredGeneration(); exp.Generation().Fingerprint ==
		configured.Fingerprint {
		out.params = configured.Params
	}
	return out, true, nil
}

// configuredGeneration is the generation the configured recipe builds. It
// carries the recipe for publication unless the recipe was adopted from the
// replica.
func (s *vectorPushSource) configuredGeneration() storage.VectorGenerationInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	gen := vectorGeneration(s.cfg.Vector.Embeddings)
	info := storage.VectorGenerationInfo{
		Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: gen.Dimensions,
	}
	if !s.adopted {
		info.Params = gen.Params
	}
	return info
}

type vectorPushExport struct {
	export *vector.Export
	// params is the configured recipe when it produced the exported
	// generation, else nil.
	params map[string]string
}

func (e *vectorPushExport) Generation() storage.VectorGenerationInfo {
	exp := e.export.Generation()
	return storage.VectorGenerationInfo{
		Fingerprint: exp.Fingerprint, Model: exp.Model, Dimension: exp.Dimension,
		Params: e.params,
	}
}

func (e *vectorPushExport) SessionDocHashes(ctx context.Context, ids []string) (map[string]string, error) {
	return e.export.SessionDocHashes(ctx, ids)
}

func (e *vectorPushExport) SessionDocs(ctx context.Context, id string) ([]storage.VectorPushDoc, string, error) {
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
func convertVectorPushDocs(docs []vector.ExportDoc) []storage.VectorPushDoc {
	out := make([]storage.VectorPushDoc, len(docs))
	for i, d := range docs {
		chunks := make([]storage.VectorPushChunk, len(d.Chunks))
		for j, c := range d.Chunks {
			chunks[j] = storage.VectorPushChunk{
				ChunkIndex: c.ChunkIndex,
				Embedding:  c.Embedding,
			}
		}
		out[i] = storage.VectorPushDoc{
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
