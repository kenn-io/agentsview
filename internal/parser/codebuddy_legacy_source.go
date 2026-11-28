package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// codeBuddySourceSet reads IDE 4.x sessions: an index.json manifest plus
// messages/<msg_id>.json files under history/<workspace_hash>/<session_id>/.
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
		// A converted conversation's CBC transcript names the same session;
		// the engine ranks this manifest against the transcript it archived.
		WithSessionReconciliationIdentity(codeBuddySessionIDFromPath),
		WithLookupIDValid(isCodeBuddyLookupID),
		WithParseFile(codeBuddyParseFile),
		WithForceReplace(),
		WithCompanionFiles(codeBuddyCompanionFiles),
		WithCompanionTranscript(codeBuddyCompanionTranscript),
	)}
}

// SourcesForChangedPath resolves workspace metadata to its immediate session
// children, and message files to their owning manifest, even after a file is
// gone.
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
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseCodeBuddySession(ctx, path, req.Source.ProjectHint, req.Machine)
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
	if !IsValidSessionID(filepath.Base(sessionDir)) {
		return false
	}
	wsDir := filepath.Dir(sessionDir)
	if !IsValidSessionID(filepath.Base(wsDir)) {
		return false
	}
	return filepath.Base(filepath.Dir(wsDir)) == "history"
}

func codeBuddyProjectHintFromPath(root, path string) string {
	return filepath.Base(filepath.Dir(filepath.Dir(path)))
}

func codeBuddySessionIDFromPath(root, path string) string {
	return filepath.Base(filepath.Dir(path))
}

func isCodeBuddyLookupID(rawID string) bool {
	return IsValidSessionID(strings.TrimPrefix(rawID, "codebuddy:"))
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
	if entries, err := os.ReadDir(msgDir); err == nil {
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
		transcript := filepath.Join(filepath.Dir(dir), "index.json")
		if _, err := os.Stat(transcript); err == nil {
			return transcript, true
		}
	}
	return "", false
}
