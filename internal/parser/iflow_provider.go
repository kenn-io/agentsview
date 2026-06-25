package parser

import (
	"context"
	"path/filepath"
	"strings"
)

// iFlow stores each chat as a JSONL transcript named session-<id>.jsonl in a
// per-project directory. It is a directory-of-files provider: discovery,
// watching, change classification, lookup, and fingerprinting come from
// DirectoryJSONLSourceSet. The ParseFile option makes that source set a full
// SourceSet so it rides the generic factory; RawSessionIDForLookup strips the
// subagent suffix from stored IDs so FindSource still matches the base file.
func newIflowProviderFactory(def AgentDef) ProviderFactory {
	return newSourceSetFactory(
		def,
		iflowProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newIflowSourceSet(cfg.Roots) },
	)
}

func newIflowSourceSet(roots []string) DirectoryJSONLSourceSet {
	return newDirectoryJSONLSourceSet(AgentIflow, roots,
		withContentHashing(),
		withSymlinkFollowing(),
		withIncludePath(isIflowSourcePath),
		withSessionIDFromPath(iflowSessionIDFromPath),
		withRawSessionIDForLookup(extractIflowBaseSessionID),
		withParseFile(iflowParseFile),
	)
}

func iflowParseFile(
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	project := iflowResolveProject(ctx, req.Source, path)
	results, err := parseIflowSession(path, project, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if len(results) == 0 {
		return nil, nil, nil
	}
	// Mirror the legacy sync path: derive continuation/subagent
	// relationship types from parent linkage before emitting.
	InferRelationshipTypes(results)
	if req.Fingerprint.Hash != "" {
		for i := range results {
			results[i].Session.File.Hash = req.Fingerprint.Hash
		}
	}
	return results, nil, nil
}

// iflowResolveProject mirrors the legacy sync project resolution for iFlow:
// start from the project directory name, then prefer a canonical project
// derived from the session's recorded cwd and git branch when available.
func iflowResolveProject(
	ctx context.Context,
	source SourceRef,
	path string,
) string {
	dirName := firstNonEmptyJSONLString(
		source.ProjectHint,
		directoryJSONLProjectFromPath(path),
	)
	project := GetProjectName(dirName)

	cwd, gitBranch := ExtractIflowProjectHints(path)
	if cwd != "" {
		if p := ExtractProjectFromCwdWithBranchContext(
			ctx, cwd, gitBranch,
		); p != "" {
			project = p
		}
	}
	return project
}

func isIflowSourcePath(root, path string) bool {
	name := filepath.Base(path)
	return strings.HasPrefix(name, "session-") &&
		strings.HasSuffix(name, ".jsonl")
}

func iflowSessionIDFromPath(root, path string) string {
	if !isIflowSourcePath(root, path) {
		return ""
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return strings.TrimPrefix(stem, "session-")
}

func iflowProviderCapabilities() Capabilities {
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
