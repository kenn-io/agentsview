package parser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Icodemate exposes two storage families under the single icodemate agent:
// the VSCode-extension plugin path (OpenCode storage on
// ~/.local/share/icodemate, owned by the OpenCode-format source set) and the
// terminal CLI path (~/.icodemate/cli/projects), whose per-session JSONL
// transcripts match Claude Code's project layout
// (<root>/<project>/<session>.jsonl). The CLI source set is the shared
// claudeSourceSet instantiated with Icodemate's layout spec; only Parse is
// owned here, relabeling every parsed session onto Icodemate's agent and
// icodemate: ID prefix.

var _ SourceSet = (*icodemateCLISourceSet)(nil)

type icodemateCLISourceSet struct {
	claudeSourceSet
}

func newIcodemateCLISourceSet(roots []string) *icodemateCLISourceSet {
	return &icodemateCLISourceSet{
		claudeSourceSet: newClaudeLayoutSourceSet(
			claudeLayoutSpecIcodemateCLI(), roots,
		),
	}
}

// claudeLayoutSpecIcodemateCLI adapts the shared Claude projects layout to
// the ICodeMate terminal CLI. Watch coverage carries no *.jsonl glob and
// sidecarSources is on because persisted tool-result companions participate
// in freshness: a sidecar-only change must invalidate the owning transcript.
func claudeLayoutSpecIcodemateCLI() claudeLayoutSpec {
	return claudeLayoutSpec{
		agent:          AgentIcodemate,
		dirLabel:       "IcodeMate CLI project directory",
		debounceScope:  "cli-projects",
		listFiles:      IcodemateCLIProjectSessionFiles,
		sidecarSources: true,
	}
}

func (s *icodemateCLISourceSet) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := s.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("icodemate cli source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine)
	project := claudeProviderProject(ctx, firstNonEmptyJSONLString(
		req.Source.ProjectHint,
		filepath.Base(filepath.Dir(path)),
	), path)
	var persistedOutputPathResolver func(string) (string, bool)
	if req.StoredPathResolver != nil {
		persistedOutputPathResolver = func(path string) (string, bool) {
			if local, ok := req.StoredPathResolver(path); ok {
				return local, true
			}
			return req.StoredPathResolver(machine + ":" + path)
		}
	}
	results, excludedIDs, err := parseIcodemateCLISession(
		ctx, path, project, machine, persistedOutputPathResolver,
	)
	if err != nil {
		return ParseOutcome{}, err
	}
	if slices.ContainsFunc(results, func(result ParseResult) bool {
		return result.Session.IsTruncated
	}) {
		return ParseOutcome{ResultSetComplete: false}, nil
	}
	for i := range results {
		if req.Fingerprint.Size != 0 {
			results[i].Session.File.Size = req.Fingerprint.Size
		}
		if req.Fingerprint.MTimeNS != 0 {
			results[i].Session.File.Mtime = req.Fingerprint.MTimeNS
		}
		if req.Fingerprint.Hash != "" {
			results[i].Session.File.Hash = req.Fingerprint.Hash
		}
	}
	out := make([]ParseResultOutcome, 0, len(results))
	for _, result := range results {
		out = append(out, ParseResultOutcome{
			Result:      result,
			DataVersion: DataVersionCurrent,
		})
	}
	return ParseOutcome{
		Results:            out,
		ExcludedSessionIDs: excludedIDs,
		ResultSetComplete:  true,
		ForceReplace:       true,
	}, nil
}

// sourcesForToolResultPath maps a changed path under a per-session
// tool-results directory back to the transcripts whose parses read it: the
// owning session plus, for a root session's results, every subagent
// transcript beneath it. It is consulted only for specs with sidecarSources.
func (s claudeSourceSet) sourcesForToolResultPath(
	root, changedPath string,
) ([]SourceRef, error) {
	root = filepath.Clean(root)
	changedPath = filepath.Clean(changedPath)
	rel, err := filepath.Rel(root, changedPath)
	if err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	toolResultsAt := slices.Index(parts, "tool-results")
	if toolResultsAt < 2 {
		return nil, nil
	}
	toolDir := filepath.Join(append([]string{root}, parts[:toolResultsAt+1]...)...)
	ownerBase := filepath.Dir(toolDir)
	var sources []SourceRef
	seen := make(map[string]struct{})
	add := func(path string) {
		source, ok := s.sourceRef(root, path)
		if !ok {
			return
		}
		if _, ok := seen[source.Key]; ok {
			return
		}
		seen[source.Key] = struct{}{}
		sources = append(sources, source)
	}
	add(ownerBase + ".jsonl")
	if slices.Contains(parts[:toolResultsAt], "subagents") {
		return sources, nil
	}

	subagentsRoot := filepath.Join(ownerBase, "subagents")
	err = filepath.WalkDir(subagentsRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "agent-") ||
			!strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		add(path)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return sources, err
}

func claudeLayoutCompositeFingerprint(
	ctx context.Context, path string, transcriptInfo os.FileInfo,
) (hash string, size, mtime int64, err error) {
	transcriptHash, err := hashJSONLSourceFileContext(ctx, path)
	if err != nil {
		return "", 0, 0, err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "transcript\x00%s\x00", transcriptHash)
	sidecars, size, mtime, err := claudeLayoutCompositeFileInfo(
		ctx, path, transcriptInfo,
	)
	if err != nil {
		return "", 0, 0, err
	}
	transcriptDir := filepath.Dir(path)
	for _, sidecar := range sidecars {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, err
		}
		relativePath, relErr := filepath.Rel(transcriptDir, sidecar)
		if relErr != nil {
			return "", 0, 0, fmt.Errorf(
				"resolve sidecar %s relative to %s: %w",
				sidecar, path, relErr,
			)
		}
		sidecarHash, hashErr := hashJSONLSourceFileContext(ctx, sidecar)
		if hashErr != nil {
			return "", 0, 0, hashErr
		}
		_, _ = fmt.Fprintf(
			h, "sidecar\x00%s\x00%s\x00",
			filepath.ToSlash(relativePath), sidecarHash,
		)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), size, mtime, nil
}

// ClaudeLayoutCompositeFileInfo reports the same aggregate size and mtime used
// by sidecar-composite source fingerprints. The sync duplicate resolver uses
// it to compare a candidate against the committed source without ignoring
// tool-result sidecars.
func ClaudeLayoutCompositeFileInfo(path string) (size, mtime int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	_, size, mtime, err = claudeLayoutCompositeFileInfo(
		context.Background(), path, info,
	)
	return size, mtime, err
}

// ClaudeLayoutCompositeMtime reports the stat-only cutoff signal for a
// transcript and its persisted tool results. Directory mtimes participate so
// deleting a sidecar remains visible after its own file mtime disappears.
func ClaudeLayoutCompositeMtime(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	mtime := info.ModTime().UnixNano()
	for _, dir := range claudeToolResultDirs(path) {
		// The session directory remains after tool-results is removed, and its
		// mtime records that deletion. Do not fall back to the shared project
		// directory when the session directory itself is absent: sibling session
		// changes must not make this transcript look fresh.
		parentInfo, parentErr := os.Stat(filepath.Dir(dir))
		if parentErr == nil {
			mtime = max(mtime, parentInfo.ModTime().UnixNano())
		} else if !errors.Is(parentErr, os.ErrNotExist) {
			return 0, parentErr
		}
		err := filepath.WalkDir(dir, func(
			_ string, entry fs.DirEntry, walkErr error,
		) error {
			if walkErr != nil {
				return walkErr
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			mtime = max(mtime, info.ModTime().UnixNano())
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	return mtime, nil
}

func claudeLayoutCompositeFileInfo(
	ctx context.Context, path string, transcriptInfo os.FileInfo,
) (sidecars []string, size, mtime int64, err error) {
	sidecars, err = claudeLayoutSidecarFiles(ctx, path)
	if err != nil {
		return nil, 0, 0, err
	}
	size = transcriptInfo.Size()
	mtime = transcriptInfo.ModTime().UnixNano()
	for _, sidecar := range sidecars {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		info, statErr := os.Stat(sidecar)
		if statErr != nil {
			return nil, 0, 0, statErr
		}
		size += info.Size()
		mtime = max(mtime, info.ModTime().UnixNano())
	}
	return sidecars, size, mtime, nil
}

func claudeLayoutSidecarFiles(
	ctx context.Context, sessionPath string,
) ([]string, error) {
	seenDirs := make(map[string]struct{})
	var files []string
	for _, dir := range claudeToolResultDirs(sessionPath) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir = filepath.Clean(dir)
		if _, ok := seenDirs[dir]; ok {
			continue
		}
		seenDirs[dir] = struct{}{}
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				files = append(files, path)
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

// parseIcodemateCLISession parses one ICodeMate CLI JSONL transcript through
// the shared Claude graph parser, then moves every result into ICodeMate's
// namespace. Reusing the graph parser is required for fork selection,
// streaming assistant snapshots, and persisted tool-result sidecars to keep
// the same semantics as the compatible Claude transcript format.
func parseIcodemateCLISession(
	ctx context.Context, path, project, machine string,
	persistedOutputPathResolver func(string) (string, bool),
) ([]ParseResult, []string, error) {
	results, excluded, err := claudeParseFile(
		path, project, machine,
		claudeParseOptions{
			ctx: ctx, compatibleTitleEvents: true,
			persistedOutputPathResolver: persistedOutputPathResolver,
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("icodemate cli parse %s: %w", path, err)
	}

	InferRelationshipTypes(results)
	applyIcodemateCLIIdentity(results)
	for i := range excluded {
		excluded[i] = icodemateCLISessionID(excluded[i])
	}
	return results, excluded, nil
}

func icodemateCLISessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "icodemate:") {
		return id
	}
	return "icodemate:" + id
}

func applyIcodemateCLIIdentity(results []ParseResult) {
	for i := range results {
		result := &results[i]
		result.Session.Agent = AgentIcodemate
		result.Session.ID = icodemateCLISessionID(result.Session.ID)
		result.Session.ParentSessionID = icodemateCLISessionID(
			result.Session.ParentSessionID,
		)
		result.Session.SourceSessionID = icodemateCLISessionID(
			result.Session.SourceSessionID,
		)
		for j := range result.Messages {
			for k := range result.Messages[j].ToolCalls {
				call := &result.Messages[j].ToolCalls[k]
				call.SubagentSessionID = icodemateCLISessionID(
					call.SubagentSessionID,
				)
				for n := range call.ResultEvents {
					call.ResultEvents[n].SubagentSessionID =
						icodemateCLISessionID(
							call.ResultEvents[n].SubagentSessionID,
						)
				}
			}
		}
	}
}
