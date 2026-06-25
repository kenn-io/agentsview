package parser

import (
	"context"
	"path/filepath"
	"strings"
)

// Zencoder stores each session as a single JSONL file in a directory. It is a
// directory-of-files provider: discovery, watching, change classification,
// lookup, and fingerprinting come from JSONLSourceSet, and the ParseFile option
// makes that source set a full SourceSet so it rides the generic factory.
func newZencoderProviderFactory(def AgentDef) ProviderFactory {
	return newSourceSetFactory(
		def,
		zencoderProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newZencoderSourceSet(cfg.Roots) },
	)
}

func newZencoderSourceSet(roots []string) JSONLSourceSet {
	return newJSONLSourceSet(AgentZencoder, roots,
		withFollowSymlinkFiles(),
		withContentHashing(),
		withIncludePath(isZencoderSourcePath),
		withSessionIDFromPath(zencoderSessionIDFromPath),
		withParseFile(zencoderParseFile),
	)
}

func zencoderParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseZencoderSession(path, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{{Session: *sess, Messages: msgs}}, nil, nil
}

func isZencoderSourcePath(root, path string) bool {
	return IsZencoderSessionFileName(filepath.Base(path))
}

func zencoderSessionIDFromPath(root, path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func zencoderProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage:  CapabilitySupported,
			Cwd:           CapabilitySupported,
			Relationships: CapabilitySupported,
			Subagents:     CapabilitySupported,
			Thinking:      CapabilitySupported,
			ToolCalls:     CapabilitySupported,
			ToolResults:   CapabilitySupported,
		},
	}
}
