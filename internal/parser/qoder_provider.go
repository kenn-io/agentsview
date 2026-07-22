package parser

import (
	"context"
	"path/filepath"
	"strings"
)

func newQoderProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		qoderProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newQoderSourceSet(cfg.Roots) },
	)
}

func newQoderSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentQoder, roots,
		WithRecursive(),
		WithSymlinkFollowing(),
		WithContentHashing(),
		WithIncludePath(isQoderSourcePath),
		WithProjectHint(qoderProjectHintFromPath),
		WithSessionIDFromPath(qoderSessionIDFromPath),
		WithLookupIDValid(isQoderLookupID),
		WithParseFile(qoderParseFile),
		WithForceReplace(),
		WithCompanionFiles(qoderCompanionFiles),
		WithCompanionTranscript(qoderCompanionTranscript),
	)
}

func qoderParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	results, excluded, err := ParseQoderSessionWithExclusions(
		path, req.Source.ProjectHint, req.Machine,
	)
	if err != nil {
		return nil, nil, err
	}
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
	return results, excluded, nil
}

func isQoderSourcePath(root, path string) bool {
	parts, ok := qoderPathParts(root, path)
	if !ok {
		return false
	}
	switch len(parts) {
	case 2:
		stem, ok := strings.CutSuffix(parts[1], ".jsonl")
		return ok &&
			!strings.HasPrefix(stem, "agent-") &&
			IsValidSessionID(stem)
	case 4:
		stem, ok := strings.CutSuffix(parts[3], ".jsonl")
		return ok &&
			IsValidSessionID(parts[1]) &&
			parts[2] == "subagents" &&
			strings.HasPrefix(stem, "agent-") &&
			IsValidSessionID(stem)
	default:
		return false
	}
}

func qoderProjectHintFromPath(root, path string) string {
	parts, ok := qoderPathParts(root, path)
	if !ok || len(parts) < 2 {
		return ""
	}
	return DecodeQoderProjectDir(parts[0])
}

func qoderSessionIDFromPath(root, path string) string {
	if !isQoderSourcePath(root, path) {
		return ""
	}
	parts, _ := qoderPathParts(root, path)
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(parts) == 4 {
		return parts[1] + ":subagent:" + stem
	}
	return stem
}

func isQoderLookupID(rawID string) bool {
	if rawID == "" {
		return false
	}
	sessionID, subagentID, hasSubagent := strings.Cut(rawID, ":subagent:")
	if !IsValidSessionID(sessionID) {
		return false
	}
	return !hasSubagent ||
		strings.HasPrefix(subagentID, "agent-") &&
			IsValidSessionID(subagentID)
}

func qoderPathParts(root, path string) ([]string, bool) {
	rel, err := filepath.Rel(root, path)
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

func qoderCompanionFiles(path string) []string {
	stem, ok := strings.CutSuffix(path, ".jsonl")
	if ok {
		return []string{stem + "-session.json"}
	}
	return nil
}

func qoderCompanionTranscript(companionPath string) (string, bool) {
	stem, ok := strings.CutSuffix(companionPath, "-session.json")
	return stem + ".jsonl", ok
}

func qoderProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.MultiSessionSource = CapabilitySupported
	source.ExcludedSessions = CapabilitySupported
	source.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
