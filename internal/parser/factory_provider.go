package parser

import (
	"context"
	"path/filepath"
	"strings"
)

// Factory Droid stores each session as <uuid>.jsonl inside a per-cwd
// directory (the cwd path with "/" replaced by "-"), plus a
// <uuid>.settings.json sidecar holding session-level token totals. It is a
// directory-of-files provider on DirectoryJSONLSourceSet; the sidecar is an
// optional companion so a settings write re-parses the transcript for usage
// and an unreadable sidecar does not block the transcript. Every parse
// replaces the stored messages: a tool_result appended after a sync folds
// into an earlier tool call, which an append-only write would never update.
func newFactoryProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		factoryProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newFactorySourceSet(cfg.Roots) },
	)
}

func newFactorySourceSet(roots []string) DirectoryJSONLSourceSet {
	return NewDirectoryJSONLSourceSet(AgentFactory, roots,
		WithContentHashing(),
		WithSymlinkFollowing(),
		WithIncludePath(isFactorySourcePath),
		WithSessionIDFromPath(factorySessionIDFromPath),
		WithCompanionFiles(func(transcript string) []string {
			return []string{factorySettingsPath(transcript)}
		}),
		WithOptionalCompanion(func(path string) bool {
			return strings.HasSuffix(path, factorySettingsSuffix)
		}),
		WithCompanionTranscript(factoryTranscriptForCompanion),
		WithParseFile(factoryParseFile),
		WithForceReplace(),
	)
}

func factoryParseFile(
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	hint := firstNonEmptyJSONLString(
		req.Source.ProjectHint, DirectoryJSONLProjectFromPath(path),
	)
	results, err := parseFactorySession(ctx, path, GetProjectName(hint), req.Machine)
	if err != nil {
		return nil, nil, err
	}
	// The fingerprint aggregates transcript + settings.json companion, so
	// the stored file identity must mirror it or freshness never matches.
	for i := range results {
		if req.Fingerprint.Size > 0 {
			results[i].Session.File.Size = req.Fingerprint.Size
		}
		if req.Fingerprint.MTimeNS > 0 {
			results[i].Session.File.Mtime = req.Fingerprint.MTimeNS
		}
		if req.Fingerprint.Hash != "" {
			results[i].Session.File.Hash = req.Fingerprint.Hash
		}
	}
	return results, nil, nil
}

func isFactorySourcePath(_, path string) bool {
	name := filepath.Base(path)
	return strings.HasSuffix(name, ".jsonl") &&
		IsValidSessionID(strings.TrimSuffix(name, ".jsonl"))
}

func factorySessionIDFromPath(root, path string) string {
	if !isFactorySourcePath(root, path) {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func factoryTranscriptForCompanion(companion string) (string, bool) {
	name := filepath.Base(companion)
	stem, ok := strings.CutSuffix(name, factorySettingsSuffix)
	if !ok || !IsValidSessionID(stem) {
		return "", false
	}
	return filepath.Join(filepath.Dir(companion), stem+".jsonl"), true
}

func factoryProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.ForceReplaceOnParse = CapabilitySupported
	caps := Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
	// A sidecar rewrite can keep the combined size and land in the same
	// mtime tick, so freshness needs the content hash.
	caps.Sync.FingerprintHashRequiredForFreshness = true
	return caps
}
