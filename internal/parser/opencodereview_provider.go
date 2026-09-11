package parser

import (
	"context"
	"path/filepath"
	"strings"
)

func newOpenCodeReviewProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		openCodeReviewProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			return newOpenCodeReviewSourceSet(cfg.Roots)
		},
	)
}

func newOpenCodeReviewSourceSet(roots []string) DirectoryJSONLSourceSet {
	return NewDirectoryJSONLSourceSet(
		AgentOpenCodeReview,
		roots,
		WithSymlinkFollowing(),
		WithIncludePath(isOpenCodeReviewSourcePath),
		WithSessionIDFromPath(openCodeReviewSessionIDFromPath),
		WithParseFile(openCodeReviewParseFile),
		WithForceReplace(),
	)
}

func isOpenCodeReviewSourcePath(root, path string) bool {
	if !IsDirectoryJSONLPath(root, path) || filepath.Ext(path) != ".jsonl" {
		return false
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return IsValidSessionID(stem)
}

func openCodeReviewSessionIDFromPath(root, path string) string {
	if !isOpenCodeReviewSourcePath(root, path) {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func openCodeReviewParseFile(
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	result, err := parseOpenCodeReviewFile(
		ctx, path, req.Source.ProjectHint, req.Machine, req.Fingerprint,
	)
	if err != nil {
		return nil, nil, err
	}
	if result == nil {
		return nil, nil, nil
	}
	return []ParseResult{*result}, nil, nil
}

func openCodeReviewProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			ToolResultEvents:     CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			TerminationStatus:    CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			TruncationStatus:     CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
