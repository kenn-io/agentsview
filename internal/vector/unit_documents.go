package vector

import (
	"context"
	"fmt"

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

// EncodeUnitDocuments converts one ordered session's units to replica documents.
func EncodeUnitDocuments(ctx context.Context, units []db.EmbeddableUnit, maxInputChars int, encode kitvec.EncodeFunc, batch ...kitvec.BatchOption) ([]storage.VectorPushDoc, error) {
	occurrences := make(map[string]int)
	docs := make([]storage.VectorPushDoc, 0, len(units))
	chunksByDoc := make([][]kitvec.Chunk, 0, len(units))
	var allChunks []kitvec.Chunk
	split := kitvec.SplitOptions{MaxRunes: maxInputChars, Overlap: ChunkOverlap(maxInputChars)}
	for _, unit := range units {
		key := unitDocKey(unit, occurrences)
		offsets, err := marshalOffsets(unit.Offsets)
		if err != nil {
			return nil, err
		}
		chunks := kitvec.Split(unit.Content, split)
		chunksByDoc = append(chunksByDoc, chunks)
		allChunks = append(allChunks, chunks...)
		docs = append(docs, storage.VectorPushDoc{DocKey: key, SessionID: unit.SessionID, SourceUUID: unit.SourceUUID, Ordinal: unit.Ordinal, OrdinalEnd: unit.OrdinalEnd, Subordinate: unit.Subordinate, OffsetsJSON: offsets, Content: unit.Content, ContentHash: contentHash(unit.Content)})
	}
	if len(units) == 0 {
		return docs, nil
	}
	vectors, err := kitvec.EncodeBatched(ctx, encode, allChunks, batch...)
	if err != nil {
		if !isPermanentEncodeError(err) {
			return nil, fmt.Errorf("encoding session %s: %w", units[0].SessionID, err)
		}
		var accepted []storage.VectorPushDoc
		for i, chunks := range chunksByDoc {
			vectors, err := kitvec.EncodeBatched(ctx, encode, chunks, batch...)
			if err != nil {
				if isPermanentEncodeError(err) {
					continue
				}
				return nil, fmt.Errorf("encoding session %s: %w", units[0].SessionID, err)
			}
			for j, chunk := range chunks {
				docs[i].Chunks = append(docs[i].Chunks, storage.VectorPushChunk{ChunkIndex: chunk.Index, Embedding: vectors[j]})
			}
			accepted = append(accepted, docs[i])
		}
		return accepted, nil
	}
	index := 0
	for i, chunks := range chunksByDoc {
		for _, chunk := range chunks {
			docs[i].Chunks = append(docs[i].Chunks, storage.VectorPushChunk{ChunkIndex: chunk.Index, Embedding: vectors[index]})
			index++
		}
	}
	return docs, nil
}
