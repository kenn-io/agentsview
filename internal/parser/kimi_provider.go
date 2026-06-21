package parser

import (
	"context"
	"path/filepath"
	"strings"
)

// Kimi stores each session as a wire.jsonl transcript under a per-workspace
// directory, with subagent transcripts nested under an "agents" subdirectory.
// It is a directory-of-files provider: discovery, watching, change
// classification, and fingerprinting come from JSONLSourceSet. The ParseFile
// option makes that source set a full SourceSet so it rides the generic
// factory; RawSessionIDSourceFiles reconstructs the wire.jsonl path from a
// colon-joined raw ID, which the standard filename-stem lookup cannot match.
func newKimiProviderFactory(def AgentDef) ProviderFactory {
	return newSourceSetFactory(
		def,
		kimiProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newKimiSourceSet(cfg.Roots) },
	)
}

func newKimiSourceSet(roots []string) JSONLSourceSet {
	return newJSONLSourceSet(AgentKimi, roots,
		withRecursive(),
		withSymlinkFollowing(),
		withIncludePath(isKimiSourcePath),
		withProjectHint(kimiProjectHintFromPath),
		withSessionIDFromPath(func(root, path string) string {
			if !isKimiSourcePath(root, path) {
				return ""
			}
			return kimiSessionIDFromPath(path)
		}),
		withRawSessionIDSourceFiles(kimiRawSessionIDSourceFiles),
		withParseFile(kimiParseFile),
	)
}

func kimiParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseKimiSession(path, req.Source.ProjectHint, req.Machine)
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

// kimiRawSessionIDSourceFiles reconstructs wire.jsonl candidate paths from a
// colon-joined raw ID. A two-part ID maps to <root>/<workspace>/<session>/
// wire.jsonl; a three-part ID adds the agents/ subagent layout
// <root>/<workspace>/<session>/agents/<agent>/wire.jsonl.
func kimiRawSessionIDSourceFiles(roots []string, rawID string) []string {
	parts := strings.Split(rawID, ":")
	if !kimiIDComponentsValid(parts...) {
		return nil
	}
	var candidates []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		switch len(parts) {
		case 2:
			candidates = append(
				candidates,
				filepath.Join(root, parts[0], parts[1], "wire.jsonl"),
			)
		case 3:
			candidates = append(candidates, filepath.Join(
				root, parts[0], parts[2], "agents", parts[1], "wire.jsonl",
			))
		}
	}
	return candidates
}

func isKimiSourcePath(root, path string) bool {
	parts, ok := kimiSourceRelParts(root, path)
	if !ok || len(parts) == 0 || parts[len(parts)-1] != "wire.jsonl" {
		return false
	}
	switch len(parts) {
	case 3:
		return kimiIDComponentsValid(parts[0], parts[1])
	case 5:
		return parts[2] == "agents" &&
			kimiIDComponentsValid(parts[0], parts[1], parts[3])
	default:
		return false
	}
}

func kimiProjectHintFromPath(root, path string) string {
	parts, ok := kimiSourceRelParts(root, path)
	if !ok || len(parts) == 0 {
		return ""
	}
	return DecodeKimiProjectDir(parts[0])
}

func kimiSourceRelParts(root, path string) ([]string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return nil, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false
		}
	}
	return parts, true
}

func kimiProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			Thinking:     CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			ToolResults:  CapabilitySupported,
		},
	}
}
