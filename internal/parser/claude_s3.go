package parser

import (
	"slices"
	"sort"
	"strings"
)

func (p *claudeProvider) S3Scanner() S3SessionScanner {
	return claudeS3Scanner()
}

func (p *claudeProvider) S3SessionID(uri string) string {
	name := pathBase(uri)
	id, ok := strings.CutSuffix(name, ".jsonl")
	if !ok {
		return ""
	}
	return id
}

func (p *claudeProvider) S3TempRelPath(objectPath string) (string, error) {
	return s3TempRelPathAfterRawAgent(objectPath, string(AgentClaude), nil)
}

func (p *claudeProvider) S3StatSession(uri string) (S3Object, error) {
	return StatClaudeS3Session(uri)
}

func (p *claudeProvider) S3PostFetchHydrate(
	tempDir, tempPath, configuredRoot, objectURI string,
) error {
	return nil
}

// StatClaudeS3Session returns metadata for a Claude transcript plus matching
// tool-result sidecars that can change parsed content without changing JSONL.
func StatClaudeS3Session(uri string) (S3Object, error) {
	obj, err := statS3Object(uri)
	if err != nil {
		return S3Object{}, err
	}
	return foldClaudeS3SidecarMetadata(obj, func(root string) []S3Object {
		objects, err := listS3Objects(root)
		if err != nil {
			return nil
		}
		return objects
	}), nil
}

func foldClaudeS3SidecarMetadata(
	obj S3Object, list func(root string) []S3Object,
) S3Object {
	for _, root := range claudeS3SidecarRoots(obj.URI) {
		for _, sidecar := range list(root) {
			obj = foldS3ObjectMetadata(obj, sidecar)
		}
	}
	return obj
}

func claudeS3SidecarRoots(uri string) []string {
	sessionPath := strings.TrimSuffix(uri, ".jsonl")
	if sessionPath == "" || sessionPath == uri {
		return nil
	}
	roots := []string{sessionPath + "/tool-results"}
	if strings.HasPrefix(pathBase(sessionPath), "agent-") {
		if idx := strings.LastIndex(sessionPath, "/subagents/"); idx > 0 {
			roots = append(roots, sessionPath[:idx]+"/tool-results")
		}
	}
	return roots
}

// claudeS3Scanner lists Claude session JSONL under an s3:// projects root,
// mirroring DiscoverClaudeProjects' selection rules:
//   - top-level <project>/<uuid>.jsonl   (skip names starting "agent-")
//   - subagents .../subagents/.../agent-*.jsonl
//
// Project is the first path segment under the root (e.g. "-home-user-proj").
func claudeS3Scanner() S3SessionScanner {
	return S3SessionScanner{
		Agent:    AgentClaude,
		Keep:     keepClaudeS3Session,
		Project:  func(_ string, segs []string) string { return segs[0] },
		Sidecars: claudeS3SidecarObjects,
	}
}

// claudeS3SubagentTranscriptPaths lists candidate subagent transcript objects,
// mirroring the local walk in ClaudeSubagentTranscriptPaths. A root session
// lists its own subagents prefix; a subagent lists the enclosing root's prefix
// so nested descendants can be linked before relationship traversal scopes the
// result. A listing error reads as no subagents, matching the local walk
// swallowing filesystem errors: the caller's usage query degrades to stale
// delegated totals instead of failing.
func claudeS3SubagentTranscriptPaths(sessionPath string) []string {
	base := sessionPath[strings.LastIndex(sessionPath, "/")+1:]
	stem := strings.TrimSuffix(base, ".jsonl")
	if stem == "" {
		return nil
	}
	prefix := strings.TrimSuffix(sessionPath, ".jsonl") + "/subagents"
	if strings.HasPrefix(stem, "agent-") {
		const marker = "/subagents/"
		idx := strings.LastIndex(sessionPath, marker)
		if idx < 0 {
			return nil
		}
		prefix = sessionPath[:idx] + "/subagents"
	}
	objects, err := listS3Objects(prefix)
	if err != nil {
		return nil
	}
	var paths []string
	for _, obj := range objects {
		if obj.URI == sessionPath {
			continue
		}
		name := obj.URI[strings.LastIndex(obj.URI, "/")+1:]
		if strings.HasPrefix(name, "agent-") &&
			strings.HasSuffix(name, ".jsonl") {
			paths = append(paths, obj.URI)
		}
	}
	sort.Strings(paths)
	return paths
}

// keepClaudeS3Session selects Claude transcript objects: a top-level
// <project>/<uuid>.jsonl (excluding agent-* names and any subagents path), or a
// subagent under .../subagents/.../agent-*.jsonl.
func keepClaudeS3Session(rel string, segs []string) bool {
	if !strings.HasSuffix(rel, ".jsonl") || len(segs) < 2 {
		return false
	}
	base := segs[len(segs)-1]
	if len(segs) >= 4 && segs[2] == "subagents" {
		return strings.HasPrefix(base, "agent-")
	}
	return len(segs) == 2 && !strings.HasPrefix(base, "agent-") &&
		!slices.Contains(segs, "subagents")
}

// claudeS3SidecarObjects returns the tool-results sidecar objects (under the
// session's tool-results prefix, plus the parent session's for subagents) whose
// metadata folds into the transcript's freshness identity. It filters the bulk
// listing rather than re-listing per prefix, since the scan already holds every
// object under the root.
func claudeS3SidecarObjects(uri string, all []S3Object) []S3Object {
	var matched []S3Object
	for _, sidecarRoot := range claudeS3SidecarRoots(uri) {
		prefix := strings.TrimSuffix(sidecarRoot, "/") + "/"
		for _, candidate := range all {
			if strings.HasPrefix(candidate.URI, prefix) {
				matched = append(matched, candidate)
			}
		}
	}
	return matched
}
