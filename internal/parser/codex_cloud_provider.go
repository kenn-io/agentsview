package parser

import (
	"context"
	"log"
	"path/filepath"
	"strings"
)

// codexCloudMirrorSuffix ends every Codex cloud mirror file. Fetcher state
// files sit beside them and never match.
const codexCloudMirrorSuffix = ".jsonl.zstd"

func newCodexCloudProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		codexCloudProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			return newCodexCloudSourceSet(cfg.Roots)
		},
	)
}

func newCodexCloudSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentCodexCloud, roots,
		WithRecursive(),
		// filepath.Ext of a mirror file is ".zstd"; the default list is
		// ".jsonl" only.
		WithExtensions(".zstd"),
		WithIncludePath(isCodexCloudMirrorPath),
		WithSessionIDFromPath(codexCloudThreadIDFromPath),
		WithLookupIDValid(func(rawID string) bool {
			return strings.TrimSpace(rawID) != ""
		}),
		WithParseFile(codexCloudParseFile),
		WithForceReplace(),
	)
}

func isCodexCloudMirrorPath(_, path string) bool {
	return codexCloudThreadIDFromPath("", path) != ""
}

func codexCloudThreadIDFromPath(_, path string) string {
	id, ok := strings.CutSuffix(filepath.Base(path), codexCloudMirrorSuffix)
	if !ok {
		return ""
	}
	return id
}

func codexCloudParseFile(
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	result, ok, err := ParseCodexCloudFile(ctx, path, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		// The monitor writes thread metadata before anything else, so a
		// file without it was written by a broken or older fetcher.
		log.Printf("codex-cloud: skipping %s: no thread metadata record", path)
		return nil, nil, nil
	}
	if req.Fingerprint.Hash != "" {
		result.Session.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{result}, nil, nil
}

func codexCloudProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.StreamingDiscovery = CapabilitySupported
	source.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
