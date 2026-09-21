package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// CodeBuddy stores each session as an index.json manifest and individual
// messages/<msg_id>.json files under a history/<workspace_hash>/<session_id>/ directory.
func newCodeBuddyProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		codeBuddyProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newCodeBuddySourceSet(cfg.Roots) },
	)
}

type codeBuddySourceSet struct {
	JSONLSourceSet
}

func newCodeBuddySourceSet(roots []string) codeBuddySourceSet {
	return codeBuddySourceSet{NewJSONLSourceSet(AgentCodeBuddy, roots,
		WithRecursive(),
		WithExtensions(".json"),
		WithContentHashing(),
		WithIncludePath(isCodeBuddySourcePath),
		WithProjectHint(codeBuddyProjectHintFromPath),
		WithSessionIDFromPath(codeBuddySessionIDFromPath),
		WithLookupIDValid(isCodeBuddyLookupID),
		WithParseFile(codeBuddyParseFile),
		WithForceReplace(),
		WithCompanionFiles(codeBuddyCompanionFiles),
		WithCompanionTranscript(codeBuddyCompanionTranscript),
	)}
}

// Workspace metadata belongs to every session in that workspace. Resolve only
// its immediate children, and derive message owners even after a file is gone.
func (s codeBuddySourceSet) SourcesForChangedPath(ctx context.Context, req ChangedPathRequest) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Clean(req.Path)
	allowed := false
	for _, root := range s.roots {
		if s.pathAllowedByRoot(root, path) && (req.WatchRoot == "" || samePath(root, req.WatchRoot)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, nil
	}
	if filepath.Ext(path) == ".json" && filepath.Base(filepath.Dir(path)) == "messages" {
		req.Path = filepath.Join(filepath.Dir(filepath.Dir(path)), "index.json")
		return s.JSONLSourceSet.SourcesForChangedPath(ctx, req)
	}
	wsDir := filepath.Dir(path)
	if filepath.Base(path) == "index.json" && filepath.Base(filepath.Dir(wsDir)) == "history" {
		entries, err := os.ReadDir(wsDir)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var sources []SourceRef
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !entry.IsDir() {
				continue
			}
			child := req
			child.Path = filepath.Join(wsDir, entry.Name(), "index.json")
			// Only existing sessions own workspace metadata; a metadata event
			// must not manufacture tombstones for unrelated directories.
			if _, err := os.Stat(child.Path); os.IsNotExist(err) {
				continue
			}
			found, err := s.JSONLSourceSet.SourcesForChangedPath(ctx, child)
			if err != nil {
				return nil, err
			}
			sources = append(sources, found...)
		}
		return sources, nil
	}
	return s.JSONLSourceSet.SourcesForChangedPath(ctx, req)
}

func codeBuddyParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseCodeBuddySession(path, req.Source.ProjectHint, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	if req.Fingerprint.Size > 0 {
		sess.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		sess.File.Mtime = req.Fingerprint.MTimeNS
	}
	return []ParseResult{{Session: *sess, Messages: msgs}}, nil, nil
}

func isCodeBuddySourcePath(root, path string) bool {
	if filepath.Base(path) != "index.json" {
		return false
	}
	sessionDir := filepath.Dir(path)
	sessionID := filepath.Base(sessionDir)
	if !IsValidSessionID(sessionID) {
		return false
	}
	wsDir := filepath.Dir(sessionDir)
	wsID := filepath.Base(wsDir)
	if !IsValidSessionID(wsID) {
		return false
	}
	historyDir := filepath.Dir(wsDir)
	return filepath.Base(historyDir) == "history"
}

func codeBuddyProjectHintFromPath(root, path string) string {
	sessionDir := filepath.Dir(path)
	wsDir := filepath.Dir(sessionDir)
	return filepath.Base(wsDir)
}

func codeBuddySessionIDFromPath(root, path string) string {
	sessionDir := filepath.Dir(path)
	return filepath.Base(sessionDir)
}

func isCodeBuddyLookupID(rawID string) bool {
	rawID = strings.TrimPrefix(rawID, "codebuddy:")
	return IsValidSessionID(rawID)
}

func codeBuddyCompanionFiles(indexPath string) []string {
	sessionDir := filepath.Dir(indexPath)
	wsDir := filepath.Dir(sessionDir)
	var companions []string

	wsIndex := filepath.Join(wsDir, "index.json")
	if _, err := os.Stat(wsIndex); err == nil {
		companions = append(companions, wsIndex)
	}

	msgDir := filepath.Join(sessionDir, "messages")
	entries, err := os.ReadDir(msgDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				companions = append(companions, filepath.Join(msgDir, e.Name()))
			}
		}
	}
	return companions
}

func codeBuddyCompanionTranscript(companionPath string) (string, bool) {
	dir := filepath.Dir(companionPath)
	if filepath.Base(dir) == "messages" {
		sessionDir := filepath.Dir(dir)
		transcript := filepath.Join(sessionDir, "index.json")
		if _, err := os.Stat(transcript); err == nil {
			return transcript, true
		}
	}
	return "", false
}

func codeBuddyProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Sync: ProviderSyncSemantics{
			FingerprintHashRequiredForFreshness: true,
			FingerprintHashInCacheKey:           true,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilityUnsupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
