package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Qwen stores each chat as a JSONL transcript under a per-project
// directory. It is a directory-of-files provider: discovery, watching,
// change classification, lookup, and fingerprinting come from
// JSONLSourceSet, and the ParseFile option makes that source set a full
// SourceSet so it rides the generic factory.
func newQwenProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		qwenProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newQwenSourceSet(cfg.Roots) },
	)
}

func newQwenSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentQwen, roots,
		WithRecursive(),
		WithSymlinkFollowing(),
		WithIncludePath(isQwenSourcePath),
		WithProjectHint(qwenProjectHintFromPath),
		WithSessionIDFromPath(qwenSessionIDFromPath),
		WithStoredPathFallbackRoot(qwenStoredPathRoot),
		WithParseFile(qwenParseFile),
		// Qwen persisted a full-file content hash (file_hash) in the legacy
		// processQwen path. Without this the provider fingerprint hash is empty
		// and a resync clears the stored file_hash to NULL.
		WithContentHashing(),
	)
}

// qwenStoredPathRoot synthesizes the configured root for a stored Qwen source
// path that is no longer under any configured QWEN_PROJECTS_DIR. The Qwen
// layout is <root>/<encoded-project>/chats/<stem>.jsonl, so the root is the
// grandparent of the chats/ directory. It validates the path is a real Qwen
// source shape and still exists so a stale DB row does not resolve to a missing
// file. This restores the legacy processQwen single-session resync, which
// parsed the DB-stored file_path directly without a root-containment check; the
// provider FindSource path otherwise fails such a resync with "provider source
// not found". Mirrors qwenPawStoredPathRoot for the QwenPaw sibling.
func qwenStoredPathRoot(storedPath string) (string, bool) {
	path := filepath.Clean(storedPath)
	chatsDir := filepath.Dir(path)
	if filepath.Base(chatsDir) != "chats" {
		return "", false
	}
	root := filepath.Dir(filepath.Dir(chatsDir))
	if root == "" || root == "." {
		return "", false
	}
	if !isQwenSourcePath(root, path) {
		return "", false
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", false
	}
	return root, true
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
