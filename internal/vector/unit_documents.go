package vector

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

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

// VectorSpool holds one session's encoded vectors in a private temporary file.
type VectorSpool struct {
	file    *os.File
	offsets map[string]int64
}

func NewVectorSpool() (*VectorSpool, error) {
	file, err := os.CreateTemp("", "agentsview-vectors-*")
	if err != nil {
		return nil, fmt.Errorf("create vector spool: %w", err)
	}
	return &VectorSpool{file: file, offsets: make(map[string]int64)}, nil
}

func (s *VectorSpool) Close() error {
	return errors.Join(s.file.Close(), os.Remove(s.file.Name()))
}

func (s *VectorSpool) Chunks(key string) ([]storage.VectorPushChunk, error) {
	offset, ok := s.offsets[key]
	if !ok {
		return nil, fmt.Errorf("vector spool missing document %s", key)
	}
	if _, err := s.file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	var chunks []storage.VectorPushChunk
	if err := gob.NewDecoder(s.file).Decode(&chunks); err != nil {
		return nil, err
	}
	return chunks, nil
}

type unitFillStore struct {
	docs          []storage.VectorPushDoc
	spool         *VectorSpool
	maxInputChars int
}

func (s *unitFillStore) PendingForGeneration(_ context.Context, _ string, after *string, limit int) ([]kitvec.Pending[string], error) {
	var pending []kitvec.Pending[string]
	chunks := 0
	start := 0
	if after != nil {
		start = sort.Search(len(s.docs), func(i int) bool { return s.docs[i].DocKey > *after })
	}
	for _, doc := range s.docs[start:] {
		if _, ok := s.spool.offsets[doc.DocKey]; ok {
			continue
		}
		count := documentChunkBound(utf8.RuneCountInString(doc.Content), s.maxInputChars)
		if len(pending) > 0 && chunks+count > maxChunksPerDocument {
			break
		}
		pending = append(pending, kitvec.Pending[string]{Doc: doc.DocKey, Content: doc.Content})
		chunks += count
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
	offset, err := s.spool.file.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek vector spool: %w", err)
	}
	if err := gob.NewEncoder(s.spool.file).Encode(chunks); err != nil {
		return fmt.Errorf("write vector spool: %w", err)
	}
	s.spool.offsets[key] = offset
	return nil
}

func (*unitFillStore) LiveGenerations(context.Context) ([]string, error) {
	return nil, errors.New("session fill does not support search")
}

func (*unitFillStore) QueryGeneration(context.Context, string, kitvec.Vector, int) ([]kitvec.Hit[string], error) {
	return nil, errors.New("session fill does not support search")
}

// maxChunksPerDocument bounds chunks and vectors in a fill page as well as in
// one document, which kit materializes together before encoding.
const maxChunksPerDocument = 4096

// DocumentExceedsChunkLimit reports whether a document's estimated window count
// exceeds the fill bound. Blank windows can make the actual chunk count lower.
func DocumentExceedsChunkLimit(content string, maxInputChars int) bool {
	return documentChunkBound(utf8.RuneCountInString(content), maxInputChars) > maxChunksPerDocument
}

// rejectOversizedDocuments splits docs into those kit may fill and the keys of
// those whose estimated window count exceeds maxChunksPerDocument. It counts
// runes instead of splitting, so rejecting a document allocates nothing.
func rejectOversizedDocuments(docs []storage.VectorPushDoc, maxInputChars int) ([]storage.VectorPushDoc, []string) {
	eligible := make([]storage.VectorPushDoc, 0, len(docs))
	var rejected []string
	for _, doc := range docs {
		if DocumentExceedsChunkLimit(doc.Content, maxInputChars) {
			log.Printf("vector fill: skipping document %s: estimated window count exceeds %d", doc.DocKey, maxChunksPerDocument)
			rejected = append(rejected, doc.DocKey)
			continue
		}
		eligible = append(eligible, doc)
	}
	return eligible, rejected
}

// documentChunkBound mirrors kit's windowing: chunks advance by maxInputChars
// minus the overlap. Blank windows produce no chunk, so this is an upper bound.
func documentChunkBound(runes, maxInputChars int) int {
	if maxInputChars <= 0 || runes <= maxInputChars {
		return 1
	}
	stride := maxInputChars - min(ChunkOverlap(maxInputChars), maxInputChars-1)
	return (runes-maxInputChars+stride-1)/stride + 1
}

// FillUnitDocuments uses kit's fill flow and reports rejected document keys.
func FillUnitDocuments(ctx context.Context, docs []storage.VectorPushDoc, spool *VectorSpool, maxInputChars, concurrency int, encode kitvec.EncodeFunc, batch ...kitvec.BatchOption) ([]storage.VectorPushDoc, []string, error) {
	eligible, rejected := rejectOversizedDocuments(docs, maxInputChars)
	store := &unitFillStore{docs: eligible, spool: spool, maxInputChars: maxInputChars}
	slices.SortFunc(store.docs, func(a, b storage.VectorPushDoc) int { return strings.Compare(a.DocKey, b.DocKey) })
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
		accepted = append(accepted, doc)
	}
	return accepted, rejected, nil
}
