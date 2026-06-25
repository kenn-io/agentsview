package parser

import (
	"context"
	"path/filepath"
	"strings"
)

// WorkBuddy stores each session as a JSONL file in a project directory, with
// subagent transcripts nested under a "subagents" subdirectory. It is a
// directory-of-files provider: discovery, watching, change classification,
// lookup, and fingerprinting come from JSONLSourceSet, and the ParseFile option
// makes that source set a full SourceSet so it rides the generic factory.
func newWorkBuddyProviderFactory(def AgentDef) ProviderFactory {
	return newSourceSetFactory(
		def,
		workBuddyProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newWorkBuddySourceSet(cfg.Roots) },
	)
}

func newWorkBuddySourceSet(roots []string) JSONLSourceSet {
	return newJSONLSourceSet(AgentWorkBuddy, roots,
		withRecursive(),
		withSymlinkFollowing(),
		withContentHashing(),
		withIncludePath(isWorkBuddySourcePath),
		withProjectHint(workBuddyProjectHintFromPath),
		withSessionIDFromPath(workBuddySessionIDFromPath),
		withLookupIDValid(isWorkBuddyLookupID),
		withParseFile(workBuddyParseFile),
	)
}

func workBuddyParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseWorkBuddySession(path, req.Source.ProjectHint, req.Machine)
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

func isWorkBuddySourcePath(root, path string) bool {
	parts, ok := workBuddyPathParts(root, path)
	if !ok {
		return false
	}
	switch len(parts) {
	case 2:
		stem, ok := strings.CutSuffix(parts[1], ".jsonl")
		return ok && IsValidSessionID(stem)
	case 4:
		return IsValidSessionID(parts[1]) &&
			parts[2] == "subagents" &&
			strings.HasSuffix(parts[3], ".jsonl")
	default:
		return false
	}
}

func workBuddyProjectHintFromPath(root, path string) string {
	parts, ok := workBuddyPathParts(root, path)
	if !ok || len(parts) < 2 {
		return ""
	}
	return parts[0]
}

func workBuddySessionIDFromPath(root, path string) string {
	if !isWorkBuddySourcePath(root, path) {
		return ""
	}
	parts, _ := workBuddyPathParts(root, path)
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(parts) == 4 {
		return parts[1] + ":subagent:" + stem
	}
	return stem
}

func isWorkBuddyLookupID(rawID string) bool {
	if rawID == "" {
		return false
	}
	sessionID, subagentID, hasSubagent := strings.Cut(rawID, ":subagent:")
	if !IsValidSessionID(sessionID) {
		return false
	}
	return !hasSubagent || IsValidSessionID(subagentID)
}

func workBuddyPathParts(root, path string) ([]string, bool) {
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

func workBuddyProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
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
