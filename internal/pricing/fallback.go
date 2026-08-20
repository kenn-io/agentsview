package pricing

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"regexp"
	"slices"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/pricing/catalog"
)

const fallbackVersionUnknown = "0"
const litellmSnapshotPath = "snapshot/litellm_snapshot.json.gz"
const maxFallbackSnapshotCompressedBytes = 1 << 20
const maxFallbackSnapshotJSONBytes = 8 << 20
const maxFallbackSnapshotModels = 100_000

var immutableFallbackSourceRefPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

//go:generate go run ./cmd/litellm-snapshot -out snapshot/litellm_snapshot.json.gz

//go:embed snapshot/litellm_snapshot.json.gz
var litellmSnapshotFS embed.FS

type litellmFallbackSnapshot struct {
	Version   string         `json:"version"`
	SourceRef string         `json:"source_ref"`
	Models    []ModelPricing `json:"models"`
}

var (
	fallbackPricingErr  error
	fallbackPricing     []ModelPricing
	fallbackPricingOnce sync.Once
)

// FallbackVersion is derived from the embedded snapshot; regenerate litellm_snapshot.json.gz to change it.
var FallbackVersion = fallbackVersionUnknown

// SeedVersion identifies the full seeded pricing set: the embedded
// snapshot version plus the curated supplemental alias version. The
// version-gated seed compares against this (not FallbackVersion) so
// existing databases pick up supplemental additions on binary upgrade
// even when the snapshot itself is unchanged.
var SeedVersion = fallbackVersionUnknown

// FallbackSourceRef is the immutable LiteLLM commit used to generate the
// embedded fallback catalog.
var FallbackSourceRef string

func init() {
	fallbackPricingOnce.Do(initFallbackPricing)
}

// FallbackPricing returns offline pricing from the embedded LiteLLM
// snapshot plus the curated supplemental aliases (see supplemental.go),
// sorted by model pattern. Data is copied for caller safety and
// deterministic DB seeding.
func FallbackPricing() []ModelPricing {
	fallbackPricingOnce.Do(initFallbackPricing)
	if fallbackPricingErr != nil {
		panic(fallbackPricingErr)
	}

	return cloneModelPricing(fallbackPricing)
}

func initFallbackPricing() {
	snapshot, err := decodeFallbackSnapshot()
	if err != nil {
		fallbackPricingErr = fmt.Errorf("loading liteLLM snapshot: %w", err)
		FallbackVersion = fallbackVersionUnknown
		SeedVersion = fallbackVersionUnknown
		FallbackSourceRef = ""
		log.Panicf("pricing: %v", fallbackPricingErr)
	}

	merged := append(
		cloneModelPricing(snapshot.Models),
		cloneModelPricing(supplementalPricing)...,
	)
	slices.SortFunc(merged, func(a, b ModelPricing) int {
		return strings.Compare(a.ModelPattern, b.ModelPattern)
	})
	fallbackPricing = merged
	FallbackVersion = snapshot.Version
	SeedVersion = snapshot.Version + "+supplemental-" + supplementalVersion
	FallbackSourceRef = snapshot.SourceRef
}

func decodeFallbackSnapshot() (litellmFallbackSnapshot, error) {
	return decodeFallbackSnapshotFromFS(litellmSnapshotFS)
}

func decodeFallbackSnapshotFromFS(fsys fs.FS) (litellmFallbackSnapshot, error) {
	blob, err := fs.ReadFile(fsys, litellmSnapshotPath)
	if errors.Is(err, fs.ErrNotExist) {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"embedded LiteLLM snapshot is missing; run make pricing-snapshot",
		)
	}
	if err != nil {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"reading snapshot: %w", err,
		)
	}
	if len(blob) == 0 {
		return litellmFallbackSnapshot{}, fmt.Errorf("empty snapshot")
	}
	if len(blob) > maxFallbackSnapshotCompressedBytes {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"compressed snapshot exceeds %d bytes",
			maxFallbackSnapshotCompressedBytes,
		)
	}

	reader, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"creating reader: %w", err,
		)
	}
	defer reader.Close()

	raw, err := readLimitedSnapshot(reader, maxFallbackSnapshotJSONBytes)
	if err != nil {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"decompressing snapshot: %w", err,
		)
	}

	var snapshot litellmFallbackSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"parsing snapshot json: %w", err,
		)
	}
	if snapshot.Version == "" {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"missing snapshot version",
		)
	}
	if !immutableFallbackSourceRefPattern.MatchString(snapshot.SourceRef) {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"missing immutable LiteLLM source ref",
		)
	}
	if len(snapshot.Models) == 0 {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"missing snapshot models",
		)
	}
	if len(snapshot.Models) > maxFallbackSnapshotModels {
		return litellmFallbackSnapshot{}, fmt.Errorf(
			"snapshot models exceed %d entries",
			maxFallbackSnapshotModels,
		)
	}
	for _, model := range snapshot.Models {
		if strings.TrimSpace(model.ModelPattern) == "" {
			return litellmFallbackSnapshot{}, fmt.Errorf(
				"snapshot contains model with empty pattern",
			)
		}
		if err := catalog.NormalizePricingBands(model.ModelPattern, model.Bands); err != nil {
			return litellmFallbackSnapshot{}, err
		}
	}

	slices.SortFunc(snapshot.Models, func(a, b ModelPricing) int {
		return strings.Compare(a.ModelPattern, b.ModelPattern)
	})

	return snapshot, nil
}

func cloneModelPricing(models []ModelPricing) []ModelPricing {
	cloned := slices.Clone(models)
	for i := range cloned {
		cloned[i].Bands = slices.Clone(cloned[i].Bands)
	}
	return cloned
}

func readLimitedSnapshot(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("decompressed snapshot exceeds %d bytes", limit)
	}
	return raw, nil
}
