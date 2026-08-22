package capture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/artifact"
)

const (
	TranscriptBundleSchemaName    = "agentsview.one-shot-transcripts"
	TranscriptBundleSchemaVersion = 1
	sourcesDirName                = "sources"
	bundleFileName                = "bundle.json"
	maxContractSources            = 128
)

// TranscriptSource ties one provider session identity to one raw JSONL file.
type TranscriptSource struct {
	SessionID string                `json:"session_id"`
	RawSource artifact.RawSourceRef `json:"raw_source"`
}

// TranscriptBundle is the closed manifest for the sensitive, uploadable
// sources directory stored inside one capture directory.
type TranscriptBundle struct {
	Schema       Schema             `json:"schema"`
	OccurrenceID string             `json:"occurrence_id"`
	Provider     Provider           `json:"provider"`
	Sources      []TranscriptSource `json:"sources"`
	Producer     ProducerMetadata   `json:"producer"`
}

func DecodeTranscriptBundle(r io.Reader) (TranscriptBundle, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var bundle TranscriptBundle
	if err := dec.Decode(&bundle); err != nil {
		return TranscriptBundle{}, fmt.Errorf("decoding transcript bundle: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return TranscriptBundle{}, errors.New("transcript bundle contains trailing JSON")
		}
		return TranscriptBundle{}, fmt.Errorf("decoding transcript bundle trailer: %w", err)
	}
	if err := validateTranscriptBundle(bundle, maxContractSources); err != nil {
		return TranscriptBundle{}, err
	}
	return bundle, nil
}

func validateTranscriptBundle(bundle TranscriptBundle, maxSources int) error {
	if bundle.Schema.Name != TranscriptBundleSchemaName ||
		bundle.Schema.Version != TranscriptBundleSchemaVersion {
		return fmt.Errorf(
			"unsupported transcript bundle schema %q version %d",
			bundle.Schema.Name, bundle.Schema.Version,
		)
	}
	if bundle.OccurrenceID == "" {
		return errors.New("transcript bundle occurrence ID is required")
	}
	if bundle.Provider != ProviderClaude && bundle.Provider != ProviderCodex {
		return fmt.Errorf("unsupported transcript bundle provider %q", bundle.Provider)
	}
	if len(bundle.Sources) == 0 || len(bundle.Sources) > maxSources {
		return errors.New("transcript bundle source count exceeds limit")
	}
	prefix := bundleSourcePrefix(bundle.Provider) + "/"
	seenPaths := make(map[string]bool, len(bundle.Sources))
	seenSessions := make(map[string]bool, len(bundle.Sources))
	for _, source := range bundle.Sources {
		if source.SessionID == "" {
			return errors.New("transcript bundle source session ID is required")
		}
		if err := artifact.ValidateRawSource(&source.RawSource); err != nil {
			return fmt.Errorf("invalid transcript source %q: %w", source.SessionID, err)
		}
		if source.RawSource.MediaType != "application/jsonl" {
			return fmt.Errorf(
				"transcript source %q has unsupported media type %q",
				source.SessionID, source.RawSource.MediaType,
			)
		}
		if !strings.HasPrefix(source.RawSource.Path, prefix) ||
			path.Clean(source.RawSource.Path) != source.RawSource.Path {
			return fmt.Errorf(
				"transcript source %q is outside the provider layout", source.SessionID)
		}
		if seenPaths[source.RawSource.Path] {
			return fmt.Errorf("duplicate transcript source path %q", source.RawSource.Path)
		}
		if seenSessions[source.SessionID] {
			return fmt.Errorf(
				"duplicate transcript source session %q", source.SessionID)
		}
		seenPaths[source.RawSource.Path] = true
		seenSessions[source.SessionID] = true
	}
	return nil
}

func encodeTranscriptBundle(bundle TranscriptBundle, maxBytes int) ([]byte, error) {
	sort.Slice(bundle.Sources, func(i, j int) bool {
		return bundle.Sources[i].RawSource.Path < bundle.Sources[j].RawSource.Path
	})
	if err := validateTranscriptBundle(bundle, maxContractSources); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding transcript bundle: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxBytes {
		return nil, errors.New("transcript bundle exceeds size limit")
	}
	return data, nil
}

func bundleSourcePrefix(provider Provider) string {
	switch provider {
	case ProviderClaude:
		return "claude/projects"
	case ProviderCodex:
		return "codex/sessions"
	default:
		return ""
	}
}
