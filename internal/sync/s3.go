package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

var findCodexS3ParentSessionURI = parser.FindCodexS3ParentSessionURI

func s3SessionIDPrefix(machine string) string {
	if machine == "" {
		return ""
	}
	return machine + "~"
}

func applyIDPrefixToID(prefix, id string) string {
	if prefix == "" || id == "" || strings.HasPrefix(id, prefix) {
		return id
	}
	return prefix + id
}

func applyIDPrefixToIDs(prefix string, ids []string) []string {
	if prefix == "" || len(ids) == 0 {
		return ids
	}
	prefixed := make([]string, len(ids))
	for i, id := range ids {
		prefixed[i] = applyIDPrefixToID(prefix, id)
	}
	return prefixed
}

func applyIDPrefixToParsedResult(
	prefix string, result *parser.ParseResult,
) {
	if prefix == "" {
		return
	}
	result.Session.ID = applyIDPrefixToID(prefix, result.Session.ID)
	result.Session.ParentSessionID = applyIDPrefixToID(
		prefix, result.Session.ParentSessionID,
	)
	result.Session.SourceSessionID = applyIDPrefixToID(
		prefix, result.Session.SourceSessionID,
	)
	for i := range result.Messages {
		for j := range result.Messages[i].ToolCalls {
			call := &result.Messages[i].ToolCalls[j]
			call.SubagentSessionID = applyIDPrefixToID(
				prefix, call.SubagentSessionID,
			)
			for k := range call.ResultEvents {
				event := &call.ResultEvents[k]
				event.SubagentSessionID = applyIDPrefixToID(
					prefix, event.SubagentSessionID,
				)
			}
		}
	}
}

func safeS3TempRelPath(file parser.DiscoveredFile) (string, error) {
	p, ok := s3ProviderFor(file.Agent)
	if !ok {
		return "", fmt.Errorf("unsafe s3 object name: %q", file.Path)
	}
	return p.S3TempRelPath(file.Path)
}

func hydrateS3CodexSessionIndex(sessionPath, sessionURI string) (string, error) {
	indexURI, ok := parser.CodexS3SessionIndexURI(sessionURI)
	if !ok {
		return "", nil
	}
	local := localCodexSessionIndexPath(sessionPath)
	if local == "" {
		return "", nil
	}
	rc, err := fetchS3Object(indexURI)
	if err != nil {
		if isMissingS3Object(err) {
			return "", nil
		}
		return "", err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(local)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		_ = os.Remove(local)
		if isMissingS3Object(err) {
			return "", nil
		}
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return local, nil
}

func localCodexSessionIndexPath(sessionPath string) string {
	dir := filepath.Dir(sessionPath)
	for dir != "" {
		base := filepath.Base(dir)
		if base == "sessions" || base == "archived_sessions" {
			return filepath.Join(
				filepath.Dir(dir), parser.CodexSessionIndexFilename,
			)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func hydrateS3CodexParent(
	tempDir, childPath, configuredRoot, childURI string,
) bool {
	parentID, resolutionNeeded := parser.CodexReplayParentID(childPath)
	if !resolutionNeeded {
		return false
	}
	parentURI, ok := findCodexS3ParentSessionURI(
		configuredRoot, childURI, parentID,
	)
	if !ok || parentURI == childURI {
		return false
	}
	relPath, err := safeS3TempRelPath(parser.DiscoveredFile{
		Agent: parser.AgentCodex,
		Path:  parentURI,
	})
	if err != nil {
		return false
	}
	rc, err := fetchS3Object(parentURI)
	if err != nil {
		return false
	}
	defer rc.Close()
	parentPath := filepath.Join(tempDir, filepath.Base(relPath))
	if err := os.MkdirAll(filepath.Dir(parentPath), 0o755); err != nil {
		return false
	}
	out, err := os.Create(parentPath)
	if err != nil {
		return false
	}
	_, copyErr := io.Copy(out, rc)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(parentPath)
		return false
	}
	return true
}

// processS3Session reads a session JSONL directly from object
// storage (in-process, no persistent local mirror): download the object's
// bytes, buffer them to a transient temp file so the existing path-based
// parsers (incremental offsets, subagent layout) work unchanged, run the
// normal per-agent processor, then delete the temp file.
func (e *Engine) processS3Session(
	ctx context.Context, file parser.DiscoveredFile, sourceInfo os.FileInfo,
) processResult {
	idPrefix := s3SessionIDPrefix(file.Machine)
	sourceFingerprint := s3SourceFingerprint(file)
	sourceChanged := e.s3SourceMetadataChangedFromInfo(
		file,
		sourceInfo.Size(),
		sourceInfo.ModTime().UnixNano(),
		sourceFingerprint,
	)
	switch file.Agent {
	case parser.AgentClaude:
		sessionID := strings.TrimSuffix(sourceInfo.Name(), ".jsonl")
		fullID := applyIDPrefixToID(idPrefix, sessionID)
		primaryPath := e.db.GetSessionFilePathNotSourceMissing(fullID)
		if primaryPath == "" && !e.forceParse {
			fresh, _ := e.claudeSourceFreshWithoutPrimary(
				ctx, file.Path, file.Path, sourceInfo, sourceFingerprint,
			)
			if fresh {
				return processResult{skip: true}
			}
		}
		if e.shouldSkipFileWithPrefix(
			idPrefix, sessionID, sourceInfo, sourceFingerprint,
		) &&
			primaryPath == file.Path &&
			!e.claudeSourceNeedsStaleForkReconciliation(ctx, file.Path) {
			sess, _ := e.db.GetSession(ctx, fullID)
			if sess != nil &&
				sess.Project != "" &&
				!parser.NeedsProjectReparse(sess.Project) {
				return processResult{skip: true}
			}
		}
	case parser.AgentCodex:
		if uuid := parser.CodexSessionUUIDFromFilename(
			path.Base(file.Path),
		); uuid != "" {
			sessionID := "codex:" + uuid
			fullID := applyIDPrefixToID(idPrefix, sessionID)
			if e.shouldSkipFileWithPrefix(
				idPrefix, sessionID, sourceInfo, sourceFingerprint,
			) &&
				e.db.GetSessionFilePath(fullID) == file.Path {
				sess, _ := e.db.GetSession(ctx, fullID)
				indexNameChanged := false
				if sess != nil &&
					sess.Project != "" &&
					!parser.NeedsProjectReparse(sess.Project) {
					var indexErr error
					indexNameChanged, indexErr =
						e.s3CodexIndexSessionNameChanged(file, uuid)
					if indexErr != nil {
						return processResult{
							err:         indexErr,
							noCacheSkip: true,
						}
					}
				}
				if sess != nil &&
					sess.Project != "" &&
					!parser.NeedsProjectReparse(sess.Project) &&
					!indexNameChanged {
					return processResult{skip: true}
				}
			}
		}
	default:
		rawID := ""
		if p, ok := s3ProviderFor(file.Agent); ok {
			rawID = p.S3SessionID(file.Path)
		}
		if rawID != "" {
			fullID := applyIDPrefixToID(idPrefix, rawID)
			if !sourceChanged &&
				e.shouldSkipFileWithPrefix(
					idPrefix, rawID, sourceInfo, sourceFingerprint,
				) &&
				e.db.GetSessionFilePath(fullID) == file.Path {
				sess, _ := e.db.GetSession(ctx, fullID)
				if sess != nil &&
					sess.Project != "" &&
					!parser.NeedsProjectReparse(sess.Project) {
					return processResult{skip: true}
				}
			}
		}
	}

	relPath, err := safeS3TempRelPath(file)
	if err != nil {
		return processResult{err: err}
	}
	// Legacy/S3 parse seam: the per-agent skip gates above return lease-free,
	// so acquire the retention lease that bounds the materialized-and-parsed
	// payload just before the object is fetched and parsed. Every result from
	// here carries the lease; releaseRetention frees it after consumption.
	lease, err := e.retentionBudget().acquire(ctx, parseRetentionSourceBytes(file))
	if err != nil {
		return processResult{err: err}
	}
	rc, err := fetchS3Object(file.Path)
	if err != nil {
		return processResult{err: err, noCacheSkip: true, retentionLease: lease}
	}
	defer rc.Close()
	dir, err := os.MkdirTemp("", "avs3-")
	if err != nil {
		return processResult{err: err, noCacheSkip: true, retentionLease: lease}
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return processResult{err: err, noCacheSkip: true, retentionLease: lease}
	}
	f, err := os.Create(tmp)
	if err != nil {
		return processResult{err: err, noCacheSkip: true, retentionLease: lease}
	}
	// Stream the object straight to disk so a large session never has to be
	// held whole in memory.
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return processResult{err: err, noCacheSkip: true, retentionLease: lease}
	}
	if err := f.Close(); err != nil {
		return processResult{err: err, noCacheSkip: true, retentionLease: lease}
	}
	hydratedToolResults := false
	sawPersistedToolResults := false
	switch file.Agent {
	case parser.AgentClaude:
		rewrote, sawPersisted, err := hydrateS3ClaudeToolResults(tmp, file.Path)
		if err != nil {
			return processResult{err: err, noCacheSkip: true, retentionLease: lease}
		}
		hydratedToolResults = rewrote
		sawPersistedToolResults = sawPersisted
	case parser.AgentCodex:
		configuredRoot := ""
		if file.ProviderSource != nil {
			configuredRoot = file.ProviderSource.ConfiguredRoot
		}
		hydrateS3CodexParent(dir, tmp, configuredRoot, file.Path)
		indexPath, err := hydrateS3CodexSessionIndex(tmp, file.Path)
		if err != nil {
			return processResult{err: err, noCacheSkip: true, retentionLease: lease}
		}
		if indexPath != "" {
			defer parser.EvictCodexSessionIndex(indexPath)
		}
	default:
		configuredRoot := ""
		if file.ProviderSource != nil {
			configuredRoot = file.ProviderSource.ConfiguredRoot
		}
		if p, ok := s3ProviderFor(file.Agent); ok {
			if err := p.S3PostFetchHydrate(dir, tmp, configuredRoot, file.Path); err != nil {
				return processResult{err: err, noCacheSkip: true, retentionLease: lease}
			}
		}
	}
	if err := os.Chtimes(tmp, sourceInfo.ModTime(), sourceInfo.ModTime()); err != nil {
		return processResult{
			err:            fmt.Errorf("setting S3 source modification time: %w", err),
			noCacheSkip:    true,
			retentionLease: lease,
		}
	}
	res, err := e.parseMaterializedS3Source(ctx, file, dir, tmp)
	if err != nil {
		return processResult{err: err, noCacheSkip: true, retentionLease: lease}
	}
	// Record the real s3:// source on each parsed session rather than the
	// transient temp path (which is deleted on return), so the stored source
	// pointer reflects where the session actually came from.
	for i := range res.results {
		applyIDPrefixToParsedResult(idPrefix, &res.results[i])
		res.results[i].Session.File.Path = file.Path
		res.results[i].Session.File.Size = sourceInfo.Size()
		res.results[i].Session.File.Mtime = sourceInfo.ModTime().UnixNano()
		if sourceFingerprint != "" {
			res.results[i].Session.File.Hash = sourceFingerprint
		}
	}
	if len(res.retrySessionIDs) > 0 && idPrefix != "" {
		prefixed := make(map[string]bool, len(res.retrySessionIDs))
		for id := range res.retrySessionIDs {
			prefixed[applyIDPrefixToID(idPrefix, id)] = true
		}
		res.retrySessionIDs = prefixed
	}
	if sourceChanged || hydratedToolResults || sawPersistedToolResults {
		res.forceReplace = true
	}
	res.excludedSessionIDs = applyIDPrefixToIDs(
		idPrefix, res.excludedSessionIDs,
	)
	if file.Agent == parser.AgentClaude {
		missing, err := e.claudeSourceMissingSessionOwnershipsForCompleteResult(
			ctx,
			file.Path,
			res.excludedSessionIDs,
			res.results,
		)
		if err != nil {
			return processResult{
				err:            err,
				noCacheSkip:    true,
				retentionLease: lease,
			}
		}
		res.sourceMissingMembers = missing
		if !e.forceParseRequested(file) &&
			!res.suppressPresenceSweep && res.providerFailureCount == 0 &&
			len(res.retrySessionIDs) == 0 && !res.noCacheSkip {
			res.claudeRowlessFreshnessKey =
				e.claudeRowlessFreshnessCacheKey(file.Path, sourceFingerprint)
		}
	}
	res.retentionLease = lease
	return res
}

// parseMaterializedS3Source parses an s3:// object that has been materialized to
// a local temp file through the normal provider parse path, instead of a
// per-agent S3 parse method. The temp file is laid out under tempDir at the
// prefix-anchored path the parser expects (so the filename-derived session
// identity and any companion lookups resolve), and the provider is configured
// with tempDir as its root so pathFromSource resolves the temp path. This is the
// same parse a local source of the same agent would get; the only S3-specific
// handling (machine-ID namespacing, recording the s3:// URI as the source path,
// and forced replacement on source change) is applied by the caller.
func (e *Engine) parseMaterializedS3Source(
	ctx context.Context,
	file parser.DiscoveredFile,
	tempDir, tempPath string,
) (processResult, error) {
	machine := e.machine
	if file.Machine != "" {
		machine = file.Machine // the s3 source machine overrides the host
	}
	provider, ok := parser.NewProvider(file.Agent, parser.ProviderConfig{
		Roots:   []string{tempDir},
		Machine: machine,
	})
	if !ok {
		return processResult{}, fmt.Errorf(
			"no provider for s3 agent %s", file.Agent,
		)
	}
	source := parser.SourceRef{
		Provider:       file.Agent,
		Key:            tempPath,
		DisplayPath:    tempPath,
		FingerprintKey: tempPath,
		ProjectHint:    file.Project,
		Opaque:         parser.MaterializedFileSource{Path: tempPath},
	}
	outcome, err := provider.Parse(ctx, parser.ParseRequest{
		Source:     source,
		Machine:    machine,
		ForceParse: true,
	})
	if err != nil {
		return processResult{}, err
	}
	retrySessionIDs := make(map[string]bool)
	for _, result := range outcome.Results {
		if result.DataVersion == parser.DataVersionNeedsRetry {
			retrySessionIDs[result.Result.Session.ID] = true
		}
	}
	if len(retrySessionIDs) == 0 {
		retrySessionIDs = nil
	}
	// Do not short-circuit on an empty Results slice: a content-free source can
	// still carry ExcludedSessionIDs (a Claude /usage probe parses to no live
	// session but excludes its ID), and the caller needs those IDs to drop the
	// previously-archived row on resync. ForceReplace must survive too.
	return processResult{
		results:            parseOutcomeResults(outcome.Results),
		excludedSessionIDs: append([]string(nil), outcome.ExcludedSessionIDs...),
		forceReplace:       outcome.ForceReplace,
		retrySessionIDs:    retrySessionIDs,
	}, nil
}
