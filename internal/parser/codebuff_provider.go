package parser

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// newCodebuffProviderFactory creates a provider factory for Codebuff.
// Freebuff sessions are handled through the same provider and distinguished
// via AgentLabel rather than a separate agent type.
func newCodebuffProviderFactory(def AgentDef) ProviderFactory {
	return NewSingleFileProviderFactory(
		def,
		codebuffProviderCapabilities(),
		func(cfg ProviderConfig) singleFileSourceSet {
			return NewSingleFileSourceSet(
				def.Type,
				cfg.Roots,
				WithStreamingFileDiscovery(codebuffDiscoverEach),
				WithFileWatchRoots(func(roots []string) []WatchRoot {
					return codebuffWatchRoots(roots)
				}),
				WithFileChangedPathClassifier(
					func(root, path string, allowMissing bool) (singleFileMatch, bool) {
						return codebuffClassifyPath(root, path, allowMissing)
					},
				),
				WithFileLookup(func(root, rawID string) (singleFileMatch, bool) {
					return codebuffFindFile(root, rawID)
				}),
				WithFileFingerprint(func(src singleFileSource) (SourceFingerprint, error) {
					return codebuffFingerprintSource(src)
				}),
				WithFileParse(func(src singleFileSource, req ParseRequest) ([]ParseResult, []string, error) {
					return codebuffParseFile(src, req)
				}),
			)
		},
	)
}

// codebuffWatchRoots creates watch plans for recursive watching of
// each root. Since sessions are two levels deep under chats/, we need
// recursive watching.
func codebuffWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"chat-messages.json", "run-state.json", "chat-meta.json"},
			DebounceKey:  "codebuff:sessions:" + root,
		})
	}
	return out
}

// codebuffClassifyPath maps a changed path back to its source
// chat-messages.json. Paths are shaped like:
// <root>/<project>/chats/<timestamp>/chat-messages.json
func codebuffClassifyPath(
	root, path string, allowMissing bool,
) (singleFileMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)

	// The path should be under root. Walk up from path to find
	// the project/chats/timestamp structure.
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return singleFileMatch{}, false
	}

	parts := strings.Split(rel, string(filepath.Separator))
	// Expected: <project>/chats/<timestamp>/...
	if len(parts) < 3 || parts[1] != "chats" {
		return singleFileMatch{}, false
	}

	projectName := parts[0]
	sessionID := parts[2]
	sessionDir := filepath.Join(root, projectName, "chats", sessionID)
	chatPath := filepath.Join(sessionDir, "chat-messages.json")

	if allowMissing {
		return singleFileMatch{
			Path:        chatPath,
			ProjectHint: projectName,
		}, true
	}

	if IsRegularFile(chatPath) {
		return singleFileMatch{
			Path:        chatPath,
			ProjectHint: projectName,
		}, true
	}
	return singleFileMatch{}, false
}

// codebuffFindFile finds a session by raw session ID (timestamp) under
// the root. Searches across all project subdirectories.
func codebuffFindFile(root, rawID string) (singleFileMatch, bool) {
	projects, err := os.ReadDir(root)
	if err != nil {
		return singleFileMatch{}, false
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		chatPath := filepath.Join(root, project.Name(), "chats", rawID, "chat-messages.json")
		if IsRegularFile(chatPath) {
			return singleFileMatch{
				Path:        chatPath,
				ProjectHint: project.Name(),
			}, true
		}
	}
	return singleFileMatch{}, false
}

// codebuffFingerprintSource computes a fingerprint for the source.
// It includes the primary chat-messages.json plus companion files
// (run-state.json, chat-meta.json) for comprehensive freshness.
func codebuffFingerprintSource(src singleFileSource) (SourceFingerprint, error) {
	info, err := os.Stat(src.Path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf(
			"stat %s: source is a directory", src.Path,
		)
	}

	fingerprint := SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}

	h := sha256.New()
	if err := addSiblingMetadataFingerprintPart(
		h, "chat-messages", src.Path, info,
	); err != nil {
		return SourceFingerprint{}, err
	}

	// Include run-state.json and chat-meta.json for completeness.
	dir := filepath.Dir(src.Path)
	for _, name := range []string{"run-state.json", "chat-meta.json"} {
		companion := filepath.Join(dir, name)
		companionInfo, err := siblingMetadataFileInfo(companion)
		if err != nil {
			return SourceFingerprint{}, err
		}
		if companionInfo == nil {
			continue
		}
		fingerprint.Size += companionInfo.Size()
		if ts := companionInfo.ModTime().UnixNano(); ts > fingerprint.MTimeNS {
			fingerprint.MTimeNS = ts
		}
		if err := addSiblingMetadataFingerprintPart(
			h, name, companion, companionInfo,
		); err != nil {
			return SourceFingerprint{}, err
		}
	}

	fingerprint.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fingerprint, nil
}

// codebuffParseFile parses a single session from chat-messages.json.
func codebuffParseFile(
	src singleFileSource, req ParseRequest,
) ([]ParseResult, []string, error) {
	dir := filepath.Dir(src.Path)

	projectHint := req.Source.ProjectHint
	if projectHint == "" {
		projectHint = codebuffProjectFromPath(src.Path)
	}

	sess, msgs, err := parseCodebuffSession(
		dir, projectHint, req.Machine,
	)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}

	// Apply fingerprint metadata.
	if req.Fingerprint.Size > 0 {
		sess.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		sess.File.Mtime = req.Fingerprint.MTimeNS
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}

	return []ParseResult{{
		Session:     *sess,
		Messages:    msgs,
		UsageEvents: sess.UsageEvents,
	}}, nil, nil
}

func codebuffProviderCapabilities() Capabilities {
	caps := jsonlFileProviderSourceCapabilities()
	// Codebuff reparses the entire mutable JSON transcript on every sync,
	// so force full message replacement to avoid stale ordinals and
	// missed in-place block updates.
	caps.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
		Source: caps,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			Model:                CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Relationships:        CapabilityNotApplicable,
			TerminationStatus:    CapabilityNotApplicable,
			MalformedLineCount:   CapabilityNotApplicable,
		},
	}
}
