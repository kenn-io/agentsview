package parser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const evenerTranscriptSuffix = ".transcript.jsonl"

func newEvenerProviderFactory(def AgentDef) ProviderFactory {
	return evenerProviderFactory{NewSourceSetFactory(def, evenerProviderCapabilities(), func(cfg ProviderConfig) SourceSet {
		return evenerSourceSet{singleFileSourceSet: NewSingleFileSourceSet(def.Type, cfg.Roots,
			WithStreamingFileDiscovery(evenerDiscoverEach),
			WithFileWatchRoots(evenerWatchRoots),
			WithFileChangedPathClassifier(evenerClassifyPath),
			WithFileLookup(evenerFindFile),
			WithFileFingerprint(evenerFingerprintSource),
			WithFileParse(evenerFileParser(context.Background())),
		), remote: cfg.PathRewriter != nil}
	})}
}

type evenerSourceSet struct {
	singleFileSourceSet
	remote bool
}

func (s evenerSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	return collectDiscoveredSources(ctx, s.DiscoverEach)
}

func (s evenerSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	for index, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return err
		}
		if isS3URI(root) {
			continue
		}
		err := evenerDiscoverEach(ctx, root, func(match singleFileMatch) error {
			// Compare configured roots rather than retaining every discovered source.
			for _, earlier := range s.roots[:index] {
				if _, ok := evenerClassifyPath(earlier, match.Path, false); ok {
					return nil
				}
			}
			return yield(s.sourceRef(root, match))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s evenerSourceSet) SourcesForChangedPath(ctx context.Context, req ChangedPathRequest) ([]SourceRef, error) {
	sources, err := s.singleFileSourceSet.SourcesForChangedPath(ctx, req)
	if err != nil {
		return nil, err
	}
	if s.remote && (strings.HasSuffix(req.Path, evenerTranscriptSuffix) || strings.HasSuffix(req.Path, ".meta.json")) {
		// Remote delta imports have no periodic reconciliation to revisit
		// children when a parent transcript or its metadata changes. Returning
		// no exact sources requests the existing provider-local fallback.
		return nil, nil
	}
	return sources, nil
}

func evenerDiscoverEach(ctx context.Context, root string, yield func(singleFileMatch) error) error {
	// Visit only session directories, with bounded batches even for flat archives.
	scanSessions := func(dir string) error {
		return streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), evenerTranscriptSuffix) {
				return nil
			}
			if match, ok := evenerClassifyPath(root, filepath.Join(dir, entry.Name()), false); ok {
				if err := yield(match); err != nil {
					return discoveryYieldError{cause: err}
				}
			}
			return nil
		})
	}
	dir := filepath.Join(root, "sessions")
	if filepath.Base(root) == "sessions" {
		dir = root
	}
	if err := scanSessions(dir); err != nil {
		if cause, ok := discoveryYieldCause(err); ok {
			return cause
		}
		if !os.IsNotExist(err) || errors.Is(err, errStreamingDirectoryChanged) {
			return err
		}
	}
	err := streamDirectoryEntries(ctx, filepath.Join(root, "projects"), func(project os.DirEntry) error {
		if !project.IsDir() {
			return nil
		}
		err := scanSessions(filepath.Join(root, "projects", project.Name(), "sessions"))
		if _, ok := discoveryYieldCause(err); ok {
			return err
		}
		if os.IsNotExist(err) && !errors.Is(err, errStreamingDirectoryChanged) {
			return nil
		}
		return err
	})
	if cause, ok := discoveryYieldCause(err); ok {
		return cause
	}
	if os.IsNotExist(err) && !errors.Is(err, errStreamingDirectoryChanged) {
		return nil
	}
	return err
}

func evenerWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{Path: root, Recursive: true, IncludeGlobs: []string{"*.transcript.jsonl", "*.meta.json"}, DebounceKey: "evener:" + root})
	}
	return out
}

func evenerMetadataPath(path string) string {
	return strings.TrimSuffix(path, evenerTranscriptSuffix) + ".meta.json"
}

func evenerSafeID(id string) bool {
	return isSafeSinglePathComponent(id) && !strings.ContainsAny(id, "\\:\x00")
}

func evenerClassifyPath(root, path string, allowMissing bool) (singleFileMatch, bool) {
	root, path = filepath.Clean(root), filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return singleFileMatch{}, false
	}
	if stem, ok := strings.CutSuffix(path, ".meta.json"); ok {
		path = stem + evenerTranscriptSuffix
		rel = strings.TrimSuffix(rel, ".meta.json") + evenerTranscriptSuffix
	}
	id, ok := strings.CutSuffix(filepath.Base(path), evenerTranscriptSuffix)
	if !ok {
		return singleFileMatch{}, false
	}
	if !evenerSafeID(id) {
		return singleFileMatch{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	valid := len(parts) == 4 && parts[0] == "projects" && parts[2] == "sessions" ||
		len(parts) == 2 && parts[0] == "sessions" || len(parts) == 1 && filepath.Base(root) == "sessions"
	if !valid || (!allowMissing && !IsRegularFile(path)) {
		return singleFileMatch{}, false
	}
	// A valid lexical layout must not follow a symlink into another source root.
	resolvedRoot, rootErr := filepath.EvalSymlinks(root)
	resolvedDir, dirErr := filepath.EvalSymlinks(filepath.Dir(path))
	if rootErr == nil && dirErr == nil && resolvedRoot != resolvedDir && !pathUnderRoot(resolvedRoot, resolvedDir) {
		return singleFileMatch{}, false
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return singleFileMatch{}, false
	}
	return singleFileMatch{Path: path}, true
}

func evenerFindFile(root, id string) (singleFileMatch, bool) {
	if !evenerSafeID(id) {
		return singleFileMatch{}, false
	}
	var found singleFileMatch
	// The lookup reads directory names only; it never parses unrelated sessions.
	_ = evenerDiscoverEach(context.Background(), root, func(match singleFileMatch) error {
		if strings.TrimSuffix(filepath.Base(match.Path), evenerTranscriptSuffix) == id {
			found = match
		}
		return nil
	})
	return found, found.Path != ""
}

func (s evenerSourceSet) FindSource(ctx context.Context, req FindSourceRequest) (SourceRef, bool, error) {
	if req.RawSessionID != "" && !evenerSafeID(req.RawSessionID) {
		return SourceRef{}, false, nil
	}
	// Stored paths are hints, not permission to return another session's file.
	for _, hint := range []*string{&req.StoredFilePath, &req.FingerprintKey} {
		if *hint != "" && req.RawSessionID != "" && strings.TrimSuffix(filepath.Base(*hint), evenerTranscriptSuffix) != req.RawSessionID {
			*hint = ""
		}
	}
	return s.singleFileSourceSet.FindSource(ctx, req)
}

func (s evenerSourceSet) ComputeMultiFileStatHash(path string) uint64 {
	meta := evenerMetadataPath(path)
	if info, err := os.Lstat(meta); err == nil && !info.Mode().IsRegular() {
		return 0
	}
	parent, err := evenerParentTranscriptPath(path)
	if err != nil {
		return 0
	}
	parentMeta := ""
	if parent != "" {
		parentMeta = evenerMetadataPath(parent)
		info, err := os.Lstat(parentMeta)
		if err != nil && !os.IsNotExist(err) || err == nil && !info.Mode().IsRegular() {
			return 0
		}
	}
	if evenerParentFileInfo(parent) == nil {
		parent = ""
	}
	return fileStatTupleDigest(0xE7, path, meta, parent, parentMeta)
}

// evenerParentFileInfo excludes symlinks before either freshness path reads
// an optional parent; unverifiable parents leave the child's history intact.
func evenerParentFileInfo(path string) os.FileInfo {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	return info
}

func evenerFingerprintSource(src singleFileSource) (SourceFingerprint, error) {
	info, err := os.Lstat(src.Path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if !info.Mode().IsRegular() {
		return SourceFingerprint{}, fmt.Errorf("evener transcript is not a regular file")
	}
	// The warm stat-digest gate uses the primary transcript's mtime. Metadata
	// freshness is represented by the per-file digest and required content hash.
	fp := SourceFingerprint{Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}
	h := sha256.New()
	if err := addSiblingMetadataFingerprintPart(h, "transcript", src.Path, info); err != nil {
		return SourceFingerprint{}, err
	}
	meta := evenerMetadataPath(src.Path)
	mi, err := os.Lstat(meta)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return SourceFingerprint{}, err
	default:
		if !mi.Mode().IsRegular() {
			return SourceFingerprint{}, fmt.Errorf("evener metadata is not a regular file")
		}
		fp.Size += mi.Size()
		if err := addSiblingMetadataFingerprintPart(h, "metadata", meta, mi); err != nil {
			return SourceFingerprint{}, err
		}
	}
	// Fork prefix validation reads one immediate parent. Its arrival, removal,
	// or rewrite must invalidate the child without counting parent bytes as
	// part of the child raw source.
	parent, err := evenerParentTranscriptPath(src.Path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if parent != "" {
		for _, dependency := range []struct{ label, path string }{
			{"parent", parent},
			{"parent_metadata", evenerMetadataPath(parent)},
		} {
			info, err := os.Lstat(dependency.path)
			switch {
			case os.IsNotExist(err):
				_, _ = fmt.Fprintf(h, "%s:missing\n", dependency.label)
			case err != nil || !info.Mode().IsRegular():
				_, _ = fmt.Fprintf(h, "%s:unavailable\n", dependency.label)
			default:
				if err := addSiblingMetadataFingerprintPart(h, dependency.label, dependency.path, info); err != nil {
					// Missing metadata permits parent import; inaccessible
					// metadata does not. Keep their freshness markers distinct.
					_, _ = fmt.Fprintf(h, "%s:unavailable\n", dependency.label)
				}
			}
		}
	}
	fp.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fp, nil
}

func evenerFileParser(ctx context.Context) func(singleFileSource, ParseRequest) ([]ParseResult, []string, error) {
	return func(src singleFileSource, req ParseRequest) ([]ParseResult, []string, error) {
		sess, msgs, err := parseEvenerSession(ctx, src.Path, req.Machine)
		if err != nil || sess == nil {
			return nil, nil, err
		}
		// A full provider parse owns the source title, including metadata removal.
		sess.SessionNamePresent = true
		if req.Fingerprint.Hash != "" {
			sess.File.Size = req.Fingerprint.Size
			sess.File.Mtime = req.Fingerprint.MTimeNS
			sess.File.Hash = req.Fingerprint.Hash
		}
		return []ParseResult{{Session: *sess, Messages: msgs}}, nil, nil
	}
}

func (s evenerSourceSet) Parse(ctx context.Context, req ParseRequest) (ParseOutcome, error) {
	if req.Fingerprint.Hash == "" {
		fingerprint, err := s.Fingerprint(ctx, req.Source)
		if err != nil {
			return ParseOutcome{}, err
		}
		req.Fingerprint = fingerprint
	}
	// Bind the request context into the single-file parser callback.
	s.cfg.parseFile = evenerFileParser(ctx)
	out, err := s.singleFileSourceSet.Parse(ctx, req)
	if err != nil {
		return out, err
	}
	for _, result := range out.Results {
		if result.Result.Session.IsTruncated {
			// A writer may have replaced the file with a shorter unfinished
			// transcript. Do not replace archived history until it is framed.
			return ParseOutcome{ResultSetComplete: false}, nil
		}
	}
	out.ForceReplace = len(out.Results) > 0
	return out, nil
}

func evenerProviderCapabilities() Capabilities {
	caps := jsonlFileProviderSourceCapabilities()
	caps.CompositeFingerprint = CapabilitySupported
	caps.ForceReplaceOnParse = CapabilitySupported
	caps.MultiFileStatHash = CapabilitySupported
	return Capabilities{
		RawCapture: RawCaptureCapabilities{
			Support: CapabilitySupported,
			Shape:   RawCaptureShapeFiles,
			Append:  RawCaptureAppendReplaceOnly,
		},
		Source: caps,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
			Relationships:        CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{FingerprintHashRequiredForFreshness: true},
	}
}

// The provider adapter exposes physical companions to the existing raw capture
// pipeline. The source set remains responsible for normalized session parsing.
type evenerProviderFactory struct{ ProviderFactory }

func (f evenerProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	return &evenerProvider{f.ProviderFactory.NewProvider(cfg).(*SourceSetProvider)}
}

type evenerProvider struct{ *SourceSetProvider }

var _ StreamingRawCaptureSourceProvider = (*evenerProvider)(nil)

func (p *evenerProvider) DiscoverRawCaptureSourcesEach(ctx context.Context, yield func(SourceRef) error) (bool, error) {
	err := p.DiscoverEach(withRawCaptureStreamingTraversal(ctx), yield)
	return err == nil, err
}

func (p *evenerProvider) PlanRawCapture(ctx context.Context, source SourceRef) (RawCapturePlan, error) {
	if err := ctx.Err(); err != nil {
		return RawCapturePlan{}, err
	}
	src, ok := source.Opaque.(singleFileSource)
	if !ok || src.Root == "" || src.Path == "" || isS3URI(src.Root) {
		return RawCapturePlan{}, invalidRawCapturePlan("evener source is not a local discovered transcript")
	}
	if _, ok := evenerClassifyPath(src.Root, src.Path, false); !ok {
		return RawCapturePlan{}, invalidRawCapturePlan("evener source is outside its configured layout")
	}
	plan := RawCapturePlan{ConfiguredRoot: src.Root, CaptureRoot: src.Root, SourceKey: source.Key}
	for _, path := range []string{src.Path, evenerMetadataPath(src.Path)} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && path != src.Path {
			continue
		}
		if err != nil {
			return RawCapturePlan{}, err
		}
		if !info.Mode().IsRegular() {
			return RawCapturePlan{}, invalidRawCapturePlan("evener capture source is not a regular file")
		}
		rel, err := filepath.Rel(src.Root, path)
		if err != nil {
			return RawCapturePlan{}, err
		}
		plan.Entries = append(plan.Entries, RawCaptureEntry{Path: filepath.ToSlash(rel), LocalPath: path})
	}
	return plan, nil
}
