package vector

import (
	"context"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	kitvec "go.kenn.io/kit/vector"
)

func unitDocKey(unit db.EmbeddableUnit, occurrences map[string]int) string {
	occurrence := 1
	if unit.SourceUUID != "" {
		key := unit.SessionID + "\x00" + unit.SourceUUID
		occurrences[key]++
		occurrence = occurrences[key]
	}
	return DocKey(unit.Kind, unit.SessionID, unit.SourceUUID, unit.Ordinal, occurrence)
}

// UnitDocuments creates the stable document metadata for one ordered session.
func UnitDocuments(units []db.EmbeddableUnit) ([]storage.VectorPushDoc, error) {
	occurrences := make(map[string]int)
	docs := make([]storage.VectorPushDoc, 0, len(units))
	for _, unit := range units {
		offsets, err := marshalOffsets(unit.Offsets)
		if err != nil {
			return nil, err
		}
		docs = append(docs, storage.VectorPushDoc{DocKey: unitDocKey(unit, occurrences), SessionID: unit.SessionID, SourceUUID: unit.SourceUUID, Ordinal: unit.Ordinal, OrdinalEnd: unit.OrdinalEnd, Subordinate: unit.Subordinate, OffsetsJSON: offsets, Content: unit.Content, ContentHash: contentHash(unit.Content)})
	}
	return docs, nil
}

type unitFillStore struct {
	docs  []storage.VectorPushDoc
	saved map[string][]storage.VectorPushChunk
}

func (s *unitFillStore) PendingForGeneration(_ context.Context, _ string, after *string, limit int) ([]kitvec.Pending[string], error) {
	var pending []kitvec.Pending[string]
	for _, doc := range s.docs {
		if after != nil && doc.DocKey <= *after {
			continue
		}
		if _, ok := s.saved[doc.DocKey]; ok {
			continue
		}
		pending = append(pending, kitvec.Pending[string]{Doc: doc.DocKey, Content: doc.Content})
		if len(pending) == limit {
			break
		}
	}
	return pending, nil
}

func (s *unitFillStore) SaveVectors(_ context.Context, _ string, key string, _ any, vectors []kitvec.ChunkVector) error {
	chunks := make([]storage.VectorPushChunk, 0, len(vectors))
	for _, v := range vectors {
		chunks = append(chunks, storage.VectorPushChunk{ChunkIndex: v.ChunkIndex, Embedding: v.Vector})
	}
	s.saved[key] = chunks
	return nil
}

func (*unitFillStore) LiveGenerations(context.Context) ([]string, error) {
	return nil, errors.New("session fill does not support search")
}

func (*unitFillStore) QueryGeneration(context.Context, string, kitvec.Vector, int) ([]kitvec.Hit[string], error) {
	return nil, errors.New("session fill does not support search")
}

// FillUnitDocuments uses kit's fill flow and reports rejected document keys.
func FillUnitDocuments(ctx context.Context, docs []storage.VectorPushDoc, maxInputChars, concurrency int, encode kitvec.EncodeFunc, batch ...kitvec.BatchOption) ([]storage.VectorPushDoc, []string, error) {
	store := &unitFillStore{docs: slices.Clone(docs), saved: make(map[string][]storage.VectorPushChunk)}
	slices.SortFunc(store.docs, func(a, b storage.VectorPushDoc) int { return strings.Compare(a.DocKey, b.DocKey) })
	var rejected []string
	_, err := kitvec.Fill[string, string](ctx, store, "session", encode,
		kitvec.WithFillSplit[string](kitvec.SplitOptions{MaxRunes: maxInputChars, Overlap: ChunkOverlap(maxInputChars)}),
		kitvec.WithFillBatch[string](batch...),
		kitvec.WithFillConcurrency[string](concurrency),
		kitvec.WithFillBatchErrorIsolation[string](isPermanentEncodeError),
		kitvec.WithFillEncodeError[string](func(key string, err error) bool {
			if !skipPermanentEncodeError(key, err) {
				return false
			}
			rejected = append(rejected, key)
			return true
		}),
	)
	if err != nil {
		return nil, nil, err
	}
	rejectedSet := make(map[string]bool, len(rejected))
	for _, key := range rejected {
		rejectedSet[key] = true
	}
	accepted := make([]storage.VectorPushDoc, 0, len(docs)-len(rejected))
	for _, doc := range docs {
		if rejectedSet[doc.DocKey] {
			continue
		}
		doc.Chunks = store.saved[doc.DocKey]
		accepted = append(accepted, doc)
	}
	return accepted, rejected, nil
}
