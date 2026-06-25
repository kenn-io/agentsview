package parser

import (
	"context"
	"path/filepath"
	"strings"
)

// Qwen stores each chat as a JSONL transcript under a per-project
// directory. It is a directory-of-files provider: discovery, watching,
// change classification, lookup, and fingerprinting come from
// JSONLSourceSet, and the ParseFile option makes that source set a full
// SourceSet so it rides the generic factory.
func newQwenProviderFactory(def AgentDef) ProviderFactory {
	return newSourceSetFactory(
		def,
		qwenProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newQwenSourceSet(cfg.Roots) },
	)
}

func newQwenSourceSet(roots []string) JSONLSourceSet {
	return newJSONLSourceSet(AgentQwen, roots,
		withRecursive(),
		withSymlinkFollowing(),
		withIncludePath(isQwenSourcePath),
		withProjectHint(qwenProjectHintFromPath),
		withSessionIDFromPath(qwenSessionIDFromPath),
		withParseFile(qwenParseFile),
	)
}

func qwenParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseQwenSession(path, req.Source.ProjectHint, req.Machine)
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

func isQwenSourcePath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 3 &&
		parts[0] != "" && parts[0] != "." && parts[0] != ".." &&
		parts[1] == "chats" &&
		parts[2] != "" && parts[2] != "." && parts[2] != ".." &&
		strings.HasSuffix(parts[2], ".jsonl")
}

func qwenProjectHintFromPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 {
		return ""
	}
	return GetProjectName(parts[0])
}

func qwenSessionIDFromPath(root, path string) string {
	if !isQwenSourcePath(root, path) {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func qwenProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
