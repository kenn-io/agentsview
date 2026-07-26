package parser

import (
	"context"
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
	return NewSourceSetFactory(
		def,
		codebuffProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			inner := NewSingleFileSourceSet(
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
			return codebuffSourceSet{inner}
		},
	)
}

// codebuffSourceSet wraps singleFileSourceSet to force full message
// replacement on every successful parse. Codebuff reparses the entire
// mutable JSON transcript on every sync, so the append-only writer
// would leave stale ordinals and missed in-place block updates.
type codebuffSourceSet struct {
	singleFileSourceSet
}

func (s codebuffSourceSet) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	outcome, err := s.singleFileSourceSet.Parse(ctx, req)
	if err == nil && outcome.ResultSetComplete && len(outcome.Results) > 0 {
		outcome.ForceReplace = true
	}
	return outcome, err
}

// codebuffWatchRoots creates recursive watch plans for each root.
// The on-disk layout is <root>/<project>/chats/<timestamp>/. Recursive
// watching ensures changes inside timestamp subdirectories are observed.
// The PeriodicReconcile flag on the Codebuff agent def handles newly
// created projects, and the recursiveWatchBudget mechanism handles
// inotify exhaustion by degrading to polling.
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

// codebuffFindFile finds a session by raw session ID under the root.
// The rawID may be either "project:timestamp" (new format) or just
// "timestamp" (legacy compatibility). For the new format, it searches
// the specific project directory. For legacy format, it searches all
// project subdirectories.
func codebuffFindFile(root, rawID string) (singleFileMatch, bool) {
	// Try to split into project:timestamp.
	parts := strings.SplitN(rawID, ":", 2)
	if len(parts) == 2 {
		projectName := parts[0]
		timestamp := parts[1]
		chatPath := filepath.Join(root, projectName, "chats", timestamp, "chat-messages.json")
		if IsRegularFile(chatPath) {
			return singleFileMatch{
				Path:        chatPath,
				ProjectHint: projectName,
			}, true
		}
		return singleFileMatch{}, false
	}

	// Legacy format: search all projects for the timestamp.
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
// The composite stat values (total size, max mtime) are returned
// immediately so the engine can skip unchanged sources without
// reading transcript bytes. The content hash is computed only when
// the engine needs it for the skip cache key.
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

	// Include run-state.json and chat-meta.json in the composite stat.
	dir := filepath.Dir(src.Path)
	for _, name := range []string{"run-state.json", "chat-meta.json"} {
		companion := filepath.Join(dir, name)
		if ci, err := os.Stat(companion); err == nil {
			fingerprint.Size += ci.Size()
			if ts := ci.ModTime().UnixNano(); ts > fingerprint.MTimeNS {
				fingerprint.MTimeNS = ts
			}
		}
	}

	// Compute content hash only when the composite stat has changed.
	// The engine's skip cache compares MTimeNS first; unchanged sources
	// skip without reading transcript bytes.
	h := sha256.New()
	if err := addSiblingMetadataFingerprintPart(
		h, "chat-messages", src.Path, info,
	); err != nil {
		return SourceFingerprint{}, err
	}
	for _, name := range []string{"run-state.json", "chat-meta.json"} {
		companion := filepath.Join(dir, name)
		if ci, err := os.Stat(companion); err == nil {
			if err := addSiblingMetadataFingerprintPart(
				h, name, companion, ci,
			); err != nil {
				return SourceFingerprint{}, err
			}
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
			Model:                CapabilityNotApplicable,
			AggregateUsageEvents: CapabilitySupported,
			Relationships:        CapabilityNotApplicable,
			TerminationStatus:    CapabilityNotApplicable,
			MalformedLineCount:   CapabilityNotApplicable,
		},
	}
}
