package pricing

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"regexp"
	"sync"
)

const genAISnapshotPath = "snapshot/genai_prices.json.gz"
const maxGenAISnapshotCompressedBytes = 2 << 20
const maxGenAISnapshotJSONBytes = 8 << 20

var immutableGenAISourceRefPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

//go:generate go run ./cmd/genai-snapshot -out snapshot/genai_prices.json.gz

//go:embed snapshot/genai_prices.json.gz
var genAISnapshotFS embed.FS

type genAIFallbackSnapshot struct {
	Version   string         `json:"version"`
	SourceRef string         `json:"source_ref"`
	Data      jsontext.Value `json:"data"`
}

var (
	embeddedGenAIDocument     GenAIDocument
	embeddedGenAIDocumentErr  error
	embeddedGenAIDocumentOnce sync.Once
)

// EmbeddedGenAIDocument returns the pinned GenAI Prices v2 JSON shipped with
// the binary, parsed into the same runtime representation as a live refresh.
func EmbeddedGenAIDocument() GenAIDocument {
	embeddedGenAIDocumentOnce.Do(initEmbeddedGenAIDocument)
	if embeddedGenAIDocumentErr != nil {
		panic(embeddedGenAIDocumentErr)
	}
	return embeddedGenAIDocument
}

func initEmbeddedGenAIDocument() {
	snapshot, err := decodeGenAISnapshotFromFS(genAISnapshotFS)
	if err != nil {
		embeddedGenAIDocumentErr = fmt.Errorf(
			"loading GenAI Prices snapshot: %w", err,
		)
		return
	}
	embeddedGenAIDocument, embeddedGenAIDocumentErr = ParseGenAIDocument(
		snapshot.Data, snapshot.Version, snapshot.SourceRef,
	)
}

func decodeGenAISnapshotFromFS(fsys fs.FS) (genAIFallbackSnapshot, error) {
	blob, err := fs.ReadFile(fsys, genAISnapshotPath)
	if err != nil {
		return genAIFallbackSnapshot{}, fmt.Errorf("reading snapshot: %w", err)
	}
	if len(blob) == 0 {
		return genAIFallbackSnapshot{}, fmt.Errorf("empty snapshot")
	}
	if len(blob) > maxGenAISnapshotCompressedBytes {
		return genAIFallbackSnapshot{}, fmt.Errorf(
			"compressed snapshot exceeds %d bytes",
			maxGenAISnapshotCompressedBytes,
		)
	}
	reader, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return genAIFallbackSnapshot{}, fmt.Errorf("creating reader: %w", err)
	}
	defer reader.Close()
	raw, err := readLimitedSnapshot(reader, maxGenAISnapshotJSONBytes)
	if err != nil {
		return genAIFallbackSnapshot{}, fmt.Errorf("decompressing snapshot: %w", err)
	}
	var snapshot genAIFallbackSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return genAIFallbackSnapshot{}, fmt.Errorf("parsing snapshot JSON: %w", err)
	}
	if snapshot.Version == "" {
		return genAIFallbackSnapshot{}, fmt.Errorf("missing snapshot version")
	}
	if !immutableGenAISourceRefPattern.MatchString(snapshot.SourceRef) {
		return genAIFallbackSnapshot{}, fmt.Errorf(
			"missing immutable GenAI Prices source ref",
		)
	}
	if len(snapshot.Data) == 0 {
		return genAIFallbackSnapshot{}, fmt.Errorf("missing snapshot data")
	}
	return snapshot, nil
}
