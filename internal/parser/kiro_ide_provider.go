package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Kiro IDE stores sessions in two on-disk layouts: an old format keyed by a
// "<workspace-hash>:<filename-hash>" pair pointing at a <ws>/<file>.chat file,
// and a new format where a UUID names a workspace-sessions/<ws>/<uuid>.json
// file. It is a directory-of-files provider: discovery, watching, change
// classification, and fingerprinting come from JSONLSourceSet. The ParseFile
// option makes that source set a full SourceSet so it rides the generic
// factory; RawSessionIDSourceFiles reconstructs the file path for the old
// colon-joined IDs, which the filename-stem discovery scan cannot match.
func newKiroIDEProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		kiroIDEProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newKiroIDESourceSet(cfg.Roots) },
	)
}

func newKiroIDESourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentKiroIDE, roots,
		WithRecursive(),
		WithExtensions(".chat", ".json"),
		WithContentHashing(),
		WithIncludePath(isKiroIDESourcePath),
		WithSessionIDFromPath(kiroIDESessionIDFromPath),
		WithRawSessionIDSourceFiles(kiroIDERawSessionIDSourceFiles),
		WithParseFile(kiroIDEParseFile),
	)
}

func kiroIDEParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseKiroIDESession(path, req.Machine)
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

// kiroIDERawSessionIDSourceFiles reconstructs candidate file paths from a raw
// session ID for both Kiro IDE layouts. The old format
// "<workspace-hash>:<filename-hash>" maps to <root>/<ws>/<file>.chat. The new
// format is a UUID whose file lives at workspace-sessions/<ws>/<uuid>.json, so
// every workspace-sessions subdirectory under each root yields a candidate.
// FindSource gates each candidate on existence via the shared path lookup.
func kiroIDERawSessionIDSourceFiles(roots []string, rawID string) []string {
	wsHash, fileHash, hasColon := strings.Cut(rawID, ":")
	oldFormat := hasColon && IsValidSessionID(wsHash) && IsValidSessionID(fileHash)
	newFormat := IsValidSessionID(rawID)
	if !oldFormat && !newFormat {
		return nil
	}
	var candidates []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		if oldFormat {
			candidates = append(
				candidates,
				filepath.Join(root, wsHash, fileHash+".chat"),
			)
		}
		if newFormat {
			wsSessionsDir := filepath.Join(root, "workspace-sessions")
			entries, err := os.ReadDir(wsSessionsDir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				candidates = append(candidates, filepath.Join(
					wsSessionsDir, entry.Name(), rawID+".json",
				))
			}
		}
	}
	return candidates
}

func isKiroIDESourcePath(root, path string) bool {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	if len(parts) == 2 {
		if parts[0] == "default" || parts[0] == "dev_data" ||
			parts[0] == "index" || parts[0] == "workspace-sessions" ||
			strings.HasPrefix(parts[0], ".") {
			return false
		}
		return strings.HasSuffix(parts[1], ".chat")
	}
	return len(parts) == 3 &&
		parts[0] == "workspace-sessions" &&
		!strings.HasPrefix(parts[1], ".") &&
		parts[2] != "sessions.json" &&
		strings.HasSuffix(parts[2], ".json")
}

func kiroIDESessionIDFromPath(root, path string) string {
	if !isKiroIDESourcePath(root, path) {
		return ""
	}
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 2 {
		return parts[0] + ":" + strings.TrimSuffix(parts[1], ".chat")
	}
	if len(parts) == 3 {
		return strings.TrimSuffix(parts[2], ".json")
	}
	return ""
}

func kiroIDEProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			SessionName:  CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			Model:        CapabilitySupported,
		},
	}
}
