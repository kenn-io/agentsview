package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func (e *Engine) expandClaudeDuplicateCandidates(
	ctx context.Context,
	files []parser.DiscoveredFile,
) ([]parser.DiscoveredFile, error) {
	sessionIDs := make(map[parser.AgentType]map[string]struct{})
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		seen[string(file.Agent)+"\x00"+file.Path] = struct{}{}
		if !isClaudeFormatTranscriptFile(file) {
			continue
		}
		sessionID := claudeSessionIDFromPath(file.Path)
		if sessionID == "" {
			continue
		}
		if sessionIDs[file.Agent] == nil {
			sessionIDs[file.Agent] = make(map[string]struct{})
		}
		sessionIDs[file.Agent][sessionID] = struct{}{}
	}
	if len(sessionIDs) == 0 {
		return files, nil
	}

	out := files
	for agent, ids := range sessionIDs {
		for _, root := range e.sources().agentDirs[agent] {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var candidates []parser.DiscoveredFile
			if agent == parser.AgentClaude && e.claudeProjectSessionFiles != nil {
				candidates = e.claudeProjectSessionFiles(root)
			} else {
				candidates = parser.ProjectJSONLSessionCandidates(root, agent, ids)
			}
			for _, candidate := range candidates {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				sessionID := claudeSessionIDFromPath(candidate.Path)
				if _, ok := ids[sessionID]; !ok {
					continue
				}
				key := string(candidate.Agent) + "\x00" + candidate.Path
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, candidate)
			}
		}
	}
	return out, nil
}

func (e *Engine) dedupeClaudeDiscoveredFiles(ctx context.Context,
	files []parser.DiscoveredFile,
) []parser.DiscoveredFile {
	byKey := make(map[string][]parser.DiscoveredFile)
	sessionIDByKey := make(map[string]string)
	for _, file := range files {
		if !isClaudeFormatTranscriptFile(file) {
			continue
		}
		sessionID := claudeSessionIDFromPath(file.Path)
		if sessionID == "" {
			continue
		}
		key := claudeDiscoveredFileKey(file, sessionID)
		byKey[key] = append(byKey[key], file)
		sessionIDByKey[key] = sessionID
	}
	if len(byKey) == 0 {
		return files
	}

	preferred := make(map[string]parser.DiscoveredFile, len(byKey))
	for key, candidates := range byKey {
		preferred[key] = e.pickPreferredClaudeDiscoveredFile(ctx,
			sessionIDByKey[key], candidates,
		)
	}

	out := files[:0]
	seen := make(map[string]struct{}, len(preferred))
	for _, file := range files {
		if !isClaudeFormatTranscriptFile(file) {
			out = append(out, file)
			continue
		}
		sessionID := claudeSessionIDFromPath(file.Path)
		if sessionID == "" {
			out = append(out, file)
			continue
		}
		key := claudeDiscoveredFileKey(file, sessionID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, preferred[key])
	}
	return out
}

func claudeDiscoveredFileKey(
	file parser.DiscoveredFile, sessionID string,
) string {
	return string(file.Agent) + "\x00" + discoveredFileIDPrefix(file) + "\x00" + sessionID
}

// isClaudeFormatAgent reports whether an agent stores sessions as Claude
// projects-layout JSONL transcripts parsed by the shared Claude DAG pipeline.
// Like isCodexFormatAgent for TraeX, it gates the format-shaped branches
// (duplicate expansion and dedup, reconciliation identity, atomic
// multi-session DAG completion, S3 transcript stat and hydration) so the
// ICodeMate terminal CLI gets the same handling as Claude. Branches tied to
// Claude's legacy archive semantics (stale-fork-only cleanup) or to
// ICodeMate's sidecar-composite freshness stay keyed to the concrete agent.
// OpenClaude also stores this layout but parses linearly to one session per
// file, so it stays outside this set.
func isClaudeFormatAgent(agent parser.AgentType) bool {
	return ingest.IsClaudeFormatAgent(agent)
}

func isClaudeFormatTranscriptFile(file parser.DiscoveredFile) bool {
	return isClaudeFormatTranscript(file.Agent, file.Path)
}

// isClaudeFormatTranscript additionally requires a session-bearing .jsonl
// path. For ICodeMate this distinguishes CLI transcripts from the agent's
// OpenCode-storage family, which shares the same agent label.
func isClaudeFormatTranscript(agent parser.AgentType, path string) bool {
	return ingest.IsClaudeFormatTranscript(agent, path)
}

// claudeFormatArchiveSessionID maps a raw filename-derived session ID into
// the owning agent's archive namespace using its registry ID prefix
// (empty for Claude, "icodemate:" for ICodeMate).
func claudeFormatArchiveSessionID(agent parser.AgentType, rawSessionID string) string {
	def, ok := parser.AgentByType(agent)
	if !ok || def.IDPrefix == "" {
		return rawSessionID
	}
	return def.IDPrefix + strings.TrimPrefix(rawSessionID, def.IDPrefix)
}

// usesCompositeSidecarFreshness reports whether a source's freshness signals
// (effective mtime, duplicate-selection stat, polling cutoff) must include
// persisted tool-result sidecars. Only ICodeMate CLI transcripts opt in:
// Claude keeps plain-file freshness because its stored fingerprints hash only
// the transcript, and switching would invalidate every archived Claude
// fingerprint.
func usesCompositeSidecarFreshness(agent parser.AgentType, path string) bool {
	return agent == parser.AgentIcodemate && isClaudeFormatTranscript(agent, path)
}

func claudeSessionIDFromPath(path string) string {
	name := filepath.Base(path)
	sessionID, ok := strings.CutSuffix(name, ".jsonl")
	if !ok {
		return ""
	}
	return sessionID
}

func (e *Engine) pickPreferredClaudeDiscoveredFile(ctx context.Context,
	sessionID string, candidates []parser.DiscoveredFile,
) parser.DiscoveredFile {
	if len(candidates) == 1 {
		return candidates[0]
	}

	idPrefix := e.idPrefix
	if isS3SourcePath(candidates[0].Path) {
		idPrefix = s3SessionIDPrefix(candidates[0].Machine)
	}
	fullID := applyIDPrefixToID(
		idPrefix, claudeFormatArchiveSessionID(candidates[0].Agent, sessionID),
	)
	storedPath := e.db.GetSessionFilePath(ctx, fullID)
	if storedPath != "" {
		for _, candidate := range candidates {
			if e.effectiveSourcePath(candidate.Path) != storedPath {
				continue
			}
			// Per the claudeFormatTranscriptPreference policy: ICodeMate's
			// committed copy stays preferred even when its composite sidecar
			// metadata moved, while Claude's committed copy must still match
			// the stored stat. Either way a competitor wins only with strict
			// transcript append progress.
			if candidate.Agent == parser.AgentIcodemate ||
				e.claudeSourceMatchesStored(ctx, fullID, candidate) {
				best := candidate
				for _, competing := range candidates {
					if e.effectiveSourcePath(competing.Path) == storedPath ||
						!claudeCandidateHasAppendProgress(competing, candidate) {
						continue
					}
					if preferClaudeDiscoveredFile(competing, best) {
						best = competing
					}
				}
				return best
			}
		}
	}

	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if preferClaudeDiscoveredFile(candidate, best) {
			best = candidate
		}
	}
	return best
}

func (e *Engine) claudeSourceMatchesStored(ctx context.Context,
	sessionID string, file parser.DiscoveredFile,
) bool {
	size, mtime, ok := claudeFormatFileSourceInfo(file)
	if !ok {
		return false
	}
	storedSize, storedMtime, ok := e.db.GetSessionFileInfo(ctx, sessionID)
	if !ok {
		return false
	}
	if storedSize != size || storedMtime != mtime {
		return false
	}
	if file.SourceFingerprint != "" {
		storedHash, ok := e.db.GetSessionFileHash(ctx, sessionID)
		if !ok || storedHash != file.SourceFingerprint {
			return false
		}
	}
	return e.db.GetSessionDataVersion(ctx, sessionID) >= db.CurrentDataVersion()
}

func claudeCandidateHasAppendProgress(
	candidate, current parser.DiscoveredFile,
) bool {
	candidateSize, _, candidateOK := claudeTranscriptFileSourceInfo(candidate)
	currentSize, _, currentOK := claudeTranscriptFileSourceInfo(current)
	if !candidateOK || !currentOK {
		return false
	}
	return candidateSize > currentSize
}

func preferClaudeDiscoveredFile(
	candidate, current parser.DiscoveredFile,
) bool {
	candidateSize, candidateMtime, candidateOK := claudeTranscriptFileSourceInfo(candidate)
	currentSize, currentMtime, currentOK := claudeTranscriptFileSourceInfo(current)
	switch {
	case candidateOK && !currentOK:
		return true
	case !candidateOK && currentOK:
		return false
	case candidateOK && currentOK:
		candidatePrimary, candidateSecondary := claudeFormatTranscriptPreference(
			candidate.Agent, candidateSize, candidateMtime,
		)
		currentPrimary, currentSecondary := claudeFormatTranscriptPreference(
			current.Agent, currentSize, currentMtime,
		)
		if candidatePrimary != currentPrimary {
			return candidatePrimary > currentPrimary
		}
		if candidateSecondary != currentSecondary {
			return candidateSecondary > currentSecondary
		}
	}
	return candidate.Path < current.Path
}

// claudeFormatTranscriptPreference orders same-session duplicate transcripts
// across configured roots. The policy per agent:
//
// Claude transcripts are append-only, so a larger copy is strictly newer
// work: rank size first, mtime second. The committed stored copy wins only
// while its stat still matches the stored metadata (an unchanged committed
// copy); see pickPreferredClaudeDiscoveredFile.
//
// ICodeMate CLI transcripts are rewritten in place (ForceReplace parses), so
// size is not progress: a legitimately shortened rewrite must beat a larger
// stale copy. The committed stored path stays preferred unless a competing
// copy shows strict transcript append progress (a continued session moved to
// another root); among uncommitted copies, mtime ranks first and size breaks
// ties. Only transcript bytes participate — sidecar composites are reserved
// for freshness, not duplicate ranking, so bulky tool results cannot outrank
// a newer transcript.
func claudeFormatTranscriptPreference(
	agent parser.AgentType, size, mtime int64,
) (primary, secondary int64) {
	if agent == parser.AgentIcodemate {
		return mtime, size
	}
	return size, mtime
}

func claudeTranscriptFileSourceInfo(
	file parser.DiscoveredFile,
) (size, mtime int64, ok bool) {
	if isS3SourcePath(file.Path) {
		if file.TranscriptMtime != 0 {
			return file.TranscriptSize, file.TranscriptMtime, true
		}
		obj, err := statS3Object(file.Path)
		if err != nil {
			return 0, 0, false
		}
		return obj.Size, obj.LastModified.UnixNano(), true
	}
	info, err := os.Stat(file.Path)
	if err != nil {
		return 0, 0, false
	}
	return info.Size(), info.ModTime().UnixNano(), true
}

func claudeFormatFileSourceInfo(
	file parser.DiscoveredFile,
) (size, mtime int64, ok bool) {
	if isS3SourcePath(file.Path) {
		if file.SourceMtime != 0 {
			return file.SourceSize, file.SourceMtime, true
		}
		obj, err := statS3SourceObject(parser.DiscoveredFile{
			Agent: parser.AgentClaude,
			Path:  file.Path,
		})
		if err != nil {
			return 0, 0, false
		}
		return obj.Size, obj.LastModified.UnixNano(), true
	}
	if usesCompositeSidecarFreshness(file.Agent, file.Path) {
		size, mtime, err := parser.ClaudeLayoutCompositeFileInfo(file.Path)
		return size, mtime, err == nil
	}
	info, err := os.Stat(file.Path)
	if err != nil {
		return 0, 0, false
	}
	return info.Size(), info.ModTime().UnixNano(), true
}
