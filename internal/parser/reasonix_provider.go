package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Reasonix stores each session as one .jsonl transcript with a sibling
// .jsonl.meta sidecar. It is a single-file provider: one file parses into one
// session, with a composite fingerprint that folds the sidecar in so a
// metadata-only write still re-parses. All behavior is wired into the shared
// single-file base via options.
func newReasonixProviderFactory(def AgentDef) ProviderFactory {
	watchSubdirs := append([]string(nil), def.WatchSubdirs...)
	return NewSingleFileProviderFactory(
		def,
		reasonixProviderCapabilities(),
		func(cfg ProviderConfig) singleFileSourceSet {
			return NewSingleFileSourceSet(
				AgentReasonix,
				cfg.Roots,
				WithStreamingFileDiscovery(reasonixDiscoverEach),
				WithFileWatchRoots(
					func(roots []string) []WatchRoot {
						return reasonixWatchRoots(roots, watchSubdirs)
					},
				),
				WithFileChangedPathClassifier(reasonixClassifyPath),
				WithFileLookup(reasonixFindFile),
				WithFileFingerprint(reasonixFingerprintSource),
				WithFileParse(reasonixParseFile),
			)
		},
	)
}

func reasonixDiscoverEach(
	ctx context.Context, root string, yield func(singleFileMatch) error,
) error {
	return streamDirectoryTree(ctx, root, func(path string, entry os.DirEntry) error {
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		if match, ok := reasonixClassifyPath(root, path, false); ok {
			return yield(match)
		}
		return nil
	})
}

func reasonixWatchRoots(roots, watchSubdirs []string) []WatchRoot {
	subdirs := watchSubdirs
	if len(subdirs) == 0 {
		subdirs = []string{""}
	}
	out := make([]WatchRoot, 0, len(roots)*len(subdirs))
	for _, root := range roots {
		for _, sub := range subdirs {
			watchPath := root
			if sub != "" {
				watchPath = filepath.Join(root, sub)
			}
			out = append(out, WatchRoot{
				Path:         watchPath,
				Recursive:    true,
				IncludeGlobs: []string{"*.jsonl", "*.jsonl.meta"},
				DebounceKey:  string(AgentReasonix) + ":" + sub + ":" + watchPath,
			})
		}
	}
	return out
}

// reasonixClassifyPath classifies a stored or changed path under root into a
// Reasonix session source. It mirrors the legacy classifyReasonixPath: a
// .jsonl.meta sidecar event maps to its sibling transcript, and only the four
// recognized layouts (project sessions, global sessions, archive, subagents)
// qualify. Reasonix performs no transcript existence check, so allowMissing is
// unused.
func reasonixClassifyPath(
	root, path string, _ bool,
) (singleFileMatch, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return singleFileMatch{}, false
	}
	if strings.HasSuffix(path, ".jsonl.meta") {
		jsonlPath := strings.TrimSuffix(path, ".meta")
		if _, err := os.Stat(jsonlPath); err != nil {
			return singleFileMatch{}, false
		}
		path = jsonlPath
		rel = strings.TrimSuffix(rel, ".meta")
	}
	if !strings.HasSuffix(path, ".jsonl") {
		return singleFileMatch{}, false
	}
	project, ok := reasonixLayoutProject(
		strings.Split(rel, string(filepath.Separator)),
	)
	if !ok {
		return singleFileMatch{}, false
	}
	return singleFileMatch{
		Path:        filepath.Clean(path),
		ProjectHint: project,
	}, true
}

func reasonixFindFile(root, rawID string) (singleFileMatch, bool) {
	path := findReasonixSourceFile(root, rawID)
	if path == "" {
		return singleFileMatch{}, false
	}
	return reasonixClassifyPath(root, path, false)
}

func reasonixFingerprintSource(
	src singleFileSource,
) (SourceFingerprint, error) {
	info, err := os.Stat(src.Path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf(
			"stat %s: source is a directory", src.Path,
		)
	}
	// Composite identity: fold the sibling .jsonl.meta sidecar into size and
	// mtime so a metadata-only write (timestamps, topic title) re-parses the
	// transcript, mirroring the legacy reasonixEffectiveInfo.
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	metaPath := src.Path + ".meta"
	if metaInfo, err := os.Stat(metaPath); err == nil {
		size += metaInfo.Size()
		if metaMTime := metaInfo.ModTime().UnixNano(); metaMTime > mtime {
			mtime = metaMTime
		}
	}
	hash, err := hashReasonixSourceFile(src.Path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Size:    size,
		MTimeNS: mtime,
		Hash:    hash,
	}, nil
}

func reasonixParseFile(
	src singleFileSource, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, usageEvents, err := parseReasonixSession(src.Path, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}
	// Use the discovered project only when metadata did not supply one via
	// workspace_root, matching the legacy processReasonix behavior.
	if req.Source.ProjectHint != "" && sess.Project == "" {
		sess.Project = req.Source.ProjectHint
	}
	// Reasonix uses a composite fingerprint (transcript plus .jsonl.meta
	// sidecar); honor it so freshness state stays in lockstep with the skip
	// cache while keeping the transcript content hash.
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
		UsageEvents: usageEvents,
	}}, nil, nil
}

func hashReasonixSourceFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}

	metaPath := path + ".meta"
	meta, err := os.Open(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("%x", h.Sum(nil)), nil
		}
		return "", fmt.Errorf("open %s: %w", metaPath, err)
	}
	defer meta.Close()

	if _, err := io.WriteString(h, "\x00reasonix-meta\x00"); err != nil {
		return "", fmt.Errorf("hash %s: %w", metaPath, err)
	}
	if _, err := io.Copy(h, meta); err != nil {
		return "", fmt.Errorf("hash %s: %w", metaPath, err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// reasonixLayoutProject validates a root-relative transcript path against the
// recognized Reasonix layouts and returns the owning project (empty for global,
// archive, and subagent sessions).
func reasonixLayoutProject(parts []string) (string, bool) {
	// Project sessions: projects/{project}/sessions/{id}.jsonl
	if len(parts) == 4 && parts[0] == "projects" && parts[2] == "sessions" &&
		strings.HasSuffix(parts[3], ".jsonl") {
		return parts[1], true
	}
	// Project sessions: projects/{project}/sessions/{id}/{id}.jsonl
	if len(parts) == 5 && parts[0] == "projects" && parts[2] == "sessions" {
		base := strings.TrimSuffix(parts[4], ".jsonl")
		if base != "" && parts[3] == base {
			return parts[1], true
		}
	}
	// Global or archive sessions: sessions/{id}.jsonl or archive/{id}.jsonl
	if len(parts) == 2 {
		if (parts[0] == "sessions" || parts[0] == "archive") &&
			strings.HasSuffix(parts[1], ".jsonl") {
			return "", true
		}
	}
	// Nested global or subagent: sessions/{id}/{id}.jsonl or
	// sessions/subagents/{id}.jsonl
	if len(parts) == 3 {
		base := strings.TrimSuffix(parts[2], ".jsonl")
		if parts[0] == "sessions" && (parts[1] == "subagents" || parts[1] == base) {
			if base != "" {
				return "", true
			}
		}
	}
	return "", false
}

func reasonixProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			StreamingDiscovery:   CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilityNotApplicable,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
