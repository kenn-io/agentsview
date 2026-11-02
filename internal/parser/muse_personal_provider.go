package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func newMusePersonalProviderFactory(def AgentDef) ProviderFactory {
	return musePersonalProviderFactory{NewSourceSetFactory(def, musePersonalProviderCapabilities(), func(cfg ProviderConfig) SourceSet {
		return musePersonalSourceSet{NewSingleFileSourceSet(def.Type, cfg.Roots,
			WithStreamingFileDiscovery(musePersonalDiscoverEach),
			WithFileWatchRoots(func(roots []string) []WatchRoot {
				var out []WatchRoot
				for _, root := range roots {
					out = append(out, WatchRoot{Path: root, Recursive: true, IncludeGlobs: []string{"*.jsonl", "sessions.json"}, DebounceKey: "muse-personal:" + root})
				}
				return out
			}),
			WithFileChangedPathClassifier(musePersonalClassifyPath),
			WithFileLookup(musePersonalFindFile),
			WithFileFingerprint(musePersonalFingerprintSource),
			WithFileParse(musePersonalFileParser),
		)}
	})}
}

type musePersonalSourceSet struct{ singleFileSourceSet }

// MusePersonalSessionFileShape recognizes only transcript/index paths relative
// to a configured agents root. Remote transport uses it to validate stale files.
func MusePersonalSessionFileShape(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) == 3 && strings.HasPrefix(parts[0], "agent-") && musePersonalUUID(strings.TrimPrefix(parts[0], "agent-")) && parts[1] == "sessions" && (parts[2] == "sessions.json" || strings.HasSuffix(parts[2], ".jsonl") && musePersonalUUID(strings.TrimSuffix(parts[2], ".jsonl")))
}

func musePersonalClassifyPath(root, path string, allowMissing bool) (singleFileMatch, bool) {
	match, ok, _ := musePersonalInspectPath(root, path, allowMissing)
	return match, ok
}

// Keep uncertain reads separate from positively foreign or absent paths. Raw
// reconciliation must not infer deletion from an unfinished rewrite or I/O error.
func musePersonalInspectPath(root, path string, allowMissing bool) (singleFileMatch, bool, error) {
	root, path = filepath.Clean(root), filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok || !MusePersonalSessionFileShape(rel) || filepath.Base(path) == "sessions.json" {
		return singleFileMatch{}, false, nil
	}
	for _, candidate := range []string{filepath.Dir(filepath.Dir(path)), filepath.Dir(path), path} {
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			if allowMissing {
				continue
			}
			return singleFileMatch{}, false, nil
		}
		if err != nil {
			return singleFileMatch{}, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || candidate == path && !info.Mode().IsRegular() || candidate != path && !info.IsDir() {
			return singleFileMatch{}, false, nil
		}
	}
	if !allowMissing {
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			return singleFileMatch{}, false, nil
		}
		if err != nil {
			return singleFileMatch{}, false, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return singleFileMatch{}, false, err
		}
		if !info.Mode().IsRegular() {
			return singleFileMatch{}, false, nil
		}
		if _, err = musePersonalReadHeader(f, path); err != nil {
			if errors.Is(err, errMusePersonalInvalidHeader) {
				return singleFileMatch{}, false, nil
			}
			return singleFileMatch{}, false, err
		}
	}
	return singleFileMatch{Path: path}, true, nil
}

func musePersonalDiscoverEach(ctx context.Context, root string, yield func(singleFileMatch) error) error {
	if isS3URI(root) {
		return nil
	}
	return streamDirectoryEntries(ctx, root, func(entry os.DirEntry) error {
		if err := ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return err
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "agent-") || !musePersonalUUID(strings.TrimPrefix(entry.Name(), "agent-")) {
			return nil
		}
		return musePersonalScanSessions(ctx, root, filepath.Join(root, entry.Name(), "sessions"), yield)
	})
}

func musePersonalScanSessions(ctx context.Context, root, dir string, yield func(singleFileMatch) error) error {
	safe, err := musePersonalSessionsDir(root, dir)
	if err != nil {
		return err
	}
	if !safe {
		return nil
	}
	return streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
		if err := ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		match, ok, err := musePersonalInspectPath(root, filepath.Join(dir, entry.Name()), false)
		if err != nil {
			return err
		}
		if ok {
			return yield(match)
		}
		return nil
	})
}

func musePersonalFindFile(ctx context.Context, root, id string) (singleFileMatch, bool) {
	if !musePersonalUUID(id) {
		return singleFileMatch{}, false
	}
	var found singleFileMatch
	_ = musePersonalDiscoverEach(ctx, root, func(m singleFileMatch) error {
		if strings.TrimSuffix(filepath.Base(m.Path), ".jsonl") == id {
			found = m
		}
		return nil
	})
	return found, found.Path != ""
}

func musePersonalFingerprintSource(ctx context.Context, src singleFileSource) (SourceFingerprint, error) {
	_, ok, err := musePersonalInspectPath(src.Root, src.Path, false)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if !ok {
		return SourceFingerprint{}, errors.New("invalid Muse Personal source")
	}
	info, err := os.Stat(src.Path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	fp := SourceFingerprint{Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}
	h := sha256.New()
	if err := addSiblingMetadataFingerprintPart(ctx, h, "transcript", src.Path, info); err != nil {
		return SourceFingerprint{}, err
	}
	index := filepath.Join(filepath.Dir(src.Path), "sessions.json")
	mi, err := siblingMetadataFileInfoStrict(index)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if mi != nil {
		fp.Size += mi.Size()
		if err := addSiblingMetadataFingerprintPart(ctx, h, "index", index, mi); err != nil {
			return SourceFingerprint{}, err
		}
	}
	fp.Hash = hex.EncodeToString(h.Sum(nil))
	return fp, nil
}

func musePersonalFileParser(ctx context.Context, src singleFileSource, req ParseRequest) ([]ParseResult, []string, error) {
	_, ok, err := musePersonalInspectPath(src.Root, src.Path, false)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, errors.New("invalid Muse Personal source")
	}
	sess, msgs, err := parseMusePersonalSession(ctx, src.Path, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	sess.File.Size = req.Fingerprint.Size
	sess.File.Mtime = req.Fingerprint.MTimeNS
	sess.File.Hash = req.Fingerprint.Hash
	return []ParseResult{{Session: *sess, Messages: msgs}}, nil, nil
}

func (s musePersonalSourceSet) Parse(ctx context.Context, req ParseRequest) (ParseOutcome, error) {
	if req.Fingerprint.Hash == "" {
		fp, err := s.Fingerprint(ctx, req.Source)
		if errors.Is(err, errMusePersonalIncompleteHeader) {
			return ParseOutcome{ResultSetComplete: false}, nil
		}
		if err != nil {
			return ParseOutcome{}, err
		}
		req.Fingerprint = fp
	}
	s.cfg.parseFile = musePersonalFileParser
	out, err := s.singleFileSourceSet.Parse(ctx, req)
	if errors.Is(err, errMusePersonalIncompleteHeader) {
		return ParseOutcome{ResultSetComplete: false}, nil
	}
	if err != nil {
		return out, err
	}
	for _, r := range out.Results {
		if r.Result.Session.IsTruncated {
			return ParseOutcome{ResultSetComplete: false}, nil
		}
	}
	out.ForceReplace = len(out.Results) > 0
	return out, nil
}

func musePersonalProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.CompositeFingerprint = CapabilitySupported
	source.ForceReplaceOnParse = CapabilitySupported
	source.MultiFileStatHash = CapabilitySupported
	return Capabilities{RawCapture: RawCaptureCapabilities{Support: CapabilitySupported, Shape: RawCaptureShapeFiles, Append: RawCaptureAppendMany}, Source: source, Content: ContentCapabilities{FirstMessage: CapabilitySupported, Thinking: CapabilitySupported, ToolCalls: CapabilitySupported, ToolResults: CapabilitySupported, ToolResultEvents: CapabilitySupported, MalformedLineCount: CapabilitySupported, TruncationStatus: CapabilitySupported}, Sync: ProviderSyncSemantics{FingerprintHashRequiredForFreshness: true}}
}

func (s musePersonalSourceSet) sourceRef(root string, match singleFileMatch) SourceRef {
	ref := s.singleFileSourceSet.sourceRef(root, match)
	ref.Key = "muse-personal:" + strings.TrimSuffix(filepath.Base(match.Path), ".jsonl")
	return ref
}

func (s musePersonalSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	return collectDiscoveredSources(ctx, s.DiscoverEach)
}

func (s musePersonalSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	for i, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return err
		}
		err := musePersonalDiscoverEach(ctx, root, func(match singleFileMatch) error {
			for _, earlier := range s.roots[:i] {
				if _, ok := musePersonalClassifyPath(earlier, match.Path, false); ok {
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

func (s musePersonalSourceSet) FindSource(ctx context.Context, req FindSourceRequest) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	if req.RawSessionID != "" && !musePersonalUUID(req.RawSessionID) {
		return SourceRef{}, false, nil
	}
	for _, hint := range []string{req.StoredFilePath, req.FingerprintKey} {
		if req.RawSessionID != "" && strings.TrimSuffix(filepath.Base(hint), ".jsonl") != req.RawSessionID {
			continue
		}
		for _, root := range s.roots {
			if match, ok := musePersonalClassifyPath(root, hint, false); ok {
				return s.sourceRef(root, match), true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		var found singleFileMatch
		err := musePersonalDiscoverEach(ctx, root, func(match singleFileMatch) error {
			if strings.TrimSuffix(filepath.Base(match.Path), ".jsonl") == req.RawSessionID {
				found = match
			}
			return nil
		})
		if err != nil {
			return SourceRef{}, false, err
		}
		if found.Path != "" {
			return s.sourceRef(root, found), true, nil
		}
	}
	return SourceRef{}, false, nil
}

func musePersonalSessionsDir(root, dir string) (bool, error) {
	rel, ok := relUnder(root, dir)
	if !ok {
		return false, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "agent-") || !musePersonalUUID(strings.TrimPrefix(parts[0], "agent-")) || parts[1] != "sessions" {
		return false, nil
	}
	for _, path := range []string{filepath.Dir(dir), dir} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, nil
		}
	}
	return true, nil
}

func (s musePersonalSourceSet) SourcesForChangedPath(ctx context.Context, req ChangedPathRequest) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if req.WatchRoot != "" && !samePath(root, req.WatchRoot) && !pathUnderRoot(root, req.WatchRoot) {
			continue
		}
		if filepath.Base(req.Path) == "sessions.json" {
			dir := filepath.Dir(req.Path)
			safe, err := musePersonalSessionsDir(root, dir)
			if err != nil {
				return nil, err
			}
			if !safe {
				continue
			}
			var out []SourceRef
			err = musePersonalScanSessions(ctx, root, dir, func(match singleFileMatch) error { out = append(out, s.sourceRef(root, match)); return nil })
			return out, err
		}
		if match, ok := musePersonalClassifyPath(root, req.Path, jsonlMissingPathFallbackAllowed(req)); ok {
			return []SourceRef{s.sourceRef(root, match)}, nil
		}
	}
	return nil, nil
}

func (s musePersonalSourceSet) ComputeMultiFileStatHash(path string) uint64 {
	// The engine caches a rootless probe provider for this optional interface.
	// Validate the path's own bounded layout rather than configured s.roots.
	root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	if _, ok := musePersonalClassifyPath(root, path, false); !ok {
		return 0
	}
	index := filepath.Join(filepath.Dir(path), "sessions.json")
	if info, err := os.Lstat(index); err == nil && !info.Mode().IsRegular() {
		return 0
	}
	return fileStatTupleDigest(0xD5, path, index)
}

type musePersonalProviderFactory struct{ ProviderFactory }

func (f musePersonalProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	return &musePersonalProvider{f.ProviderFactory.NewProvider(cfg).(*SourceSetProvider)}
}

type musePersonalProvider struct{ *SourceSetProvider }

var _ StreamingRawCaptureSourceProvider = (*musePersonalProvider)(nil)

func (p *musePersonalProvider) DiscoverRawCaptureSourcesEach(ctx context.Context, yield func(SourceRef) error) (bool, error) {
	err := p.DiscoverEach(withRawCaptureStreamingTraversal(ctx), yield)
	return err == nil, err
}

func (p *musePersonalProvider) PlanRawCapture(ctx context.Context, source SourceRef) (RawCapturePlan, error) {
	if err := ctx.Err(); err != nil {
		return RawCapturePlan{}, err
	}
	src, ok := source.Opaque.(singleFileSource)
	if !ok || src.Root == "" || isS3URI(src.Root) {
		return RawCapturePlan{}, invalidRawCapturePlan("Muse Personal source is not a local discovered transcript")
	}
	if _, ok := musePersonalClassifyPath(src.Root, src.Path, false); !ok {
		return RawCapturePlan{}, invalidRawCapturePlan("Muse Personal source is outside its configured layout")
	}
	expectedKey := "muse-personal:" + strings.TrimSuffix(filepath.Base(src.Path), ".jsonl")
	if source.Key != expectedKey {
		return RawCapturePlan{}, invalidRawCapturePlan("Muse Personal source identity does not match its transcript")
	}
	plan := RawCapturePlan{ConfiguredRoot: src.Root, CaptureRoot: src.Root, SourceKey: source.Key}
	for _, path := range []string{src.Path, filepath.Join(filepath.Dir(src.Path), "sessions.json")} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && path != src.Path {
			continue
		}
		if err != nil {
			return RawCapturePlan{}, err
		}
		if !info.Mode().IsRegular() {
			return RawCapturePlan{}, invalidRawCapturePlan("Muse Personal capture input is not a regular file")
		}
		rel, err := filepath.Rel(src.Root, path)
		if err != nil {
			return RawCapturePlan{}, err
		}
		plan.Entries = append(plan.Entries, RawCaptureEntry{Path: filepath.ToSlash(rel), LocalPath: path, Appendable: path == src.Path})
	}
	return plan, nil
}
