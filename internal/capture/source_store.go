package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

var errSourceChanged = errors.New("capture source changed while it was copied")

type persistedSourceSet struct {
	Paths     []string
	Snapshots []sourceSnapshot
}

func (s *captureState) resetPersistedSources(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.clearSourceStaging(); err != nil {
		return err
	}
	if err := s.clearPersistedSourceCopies(ctx); err != nil {
		return err
	}
	if err := s.removeAttemptArchive(); err != nil {
		return err
	}
	s.manifest.Sources = nil
	s.manifest.SourcesComplete = false
	return s.saveManifestContext(ctx)
}

func (s *captureState) clearPersistedSourceCopies(ctx context.Context) error {
	// sources is an internal, capture-owned recovery tree. An unsealed attempt
	// must remove the whole tree before it accounts for a replacement source
	// set, including files copied before a manifest write was interrupted.
	root := s.sourcesPath()
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("transcript bundle root is not a directory")
	}
	directories := make([]string, 0, s.manifest.Limits.MaxSources*4)
	examined := 0
	err = filepath.WalkDir(root, func(
		path string, entry os.DirEntry, walkErr error,
	) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		examined++
		if examined > s.manifest.Limits.MaxSources*8 {
			return errorWithReason(
				ReasonSourceLimit, "transcript bundle tree exceeds entry limit")
		}
		if err := verifyCapturePathOwner(path); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("transcript bundle contains a symbolic link")
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("transcript bundle contains an unknown entry")
		}
		return os.Remove(path)
	})
	if err != nil {
		return err
	}
	for _, directory := range slices.Backward(directories) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Remove(directory); err != nil {
			return err
		}
	}
	return nil
}

func (s *captureState) removeAttemptArchive() error {
	// An unsealed retry must rebuild accounting from exactly the new source set.
	// These fixed files belong only to this capture's isolated scratch archive.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		path := s.archivePath() + suffix
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking capture archive: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("capture archive path is not a regular file: %s", filepath.Base(path))
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing stale capture archive: %w", err)
		}
	}
	return nil
}

func (s *captureState) persistSourcePaths(
	ctx context.Context,
	livePaths []string,
) (persistedSourceSet, error) {
	snapshots, err := snapshotSources(ctx, livePaths, s.manifest.Limits)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return persistedSourceSet{}, errorWithReason(
				ReasonSourceUnavailable, "observed capture source is unavailable")
		}
		return persistedSourceSet{}, err
	}
	records := append([]TranscriptSource(nil), s.manifest.Sources...)
	prospective := append([]TranscriptSource(nil), records...)
	for _, snapshot := range snapshots {
		bundlePath, pathErr := s.sourceBundlePath(snapshot.Path)
		if pathErr != nil {
			return persistedSourceSet{}, pathErr
		}
		prospective = replaceTranscriptSource(prospective, TranscriptSource{
			SessionID: sourceSessionID(Provider(s.manifest.Provider), snapshot.Path),
			RawSource: artifact.RawSourceRef{
				Hash: strings.Repeat("0", sha256.Size*2),
				Size: snapshot.Size, MediaType: "application/jsonl", Path: bundlePath,
			},
		})
	}
	if err := validatePersistedSourceBounds(prospective, s.manifest.Limits); err != nil {
		return persistedSourceSet{}, err
	}
	if err := s.validateManifestSources(prospective); err != nil {
		return persistedSourceSet{}, err
	}
	paths := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return persistedSourceSet{}, err
		}
		record, path, copyErr := s.copySource(ctx, snapshot)
		if copyErr != nil {
			return persistedSourceSet{}, copyErr
		}
		records = replaceTranscriptSource(records, record)
		if err := validatePersistedSourceBounds(records, s.manifest.Limits); err != nil {
			return persistedSourceSet{}, err
		}
		s.manifest.Sources = append([]TranscriptSource(nil), records...)
		if err := s.saveManifestContext(ctx); err != nil {
			return persistedSourceSet{}, err
		}
		paths = append(paths, path)
	}
	return persistedSourceSet{Paths: paths, Snapshots: snapshots}, nil
}

func (s *captureState) copySource(
	ctx context.Context,
	snapshot sourceSnapshot,
) (TranscriptSource, string, error) {
	bundlePath, err := s.sourceBundlePath(snapshot.Path)
	if err != nil {
		return TranscriptSource{}, "", err
	}
	sessionID := sourceSessionID(Provider(s.manifest.Provider), snapshot.Path)
	if sessionID == "" {
		return TranscriptSource{}, "", errors.New("provider session identity is unavailable")
	}
	destination := s.sourcesPath(filepath.FromSlash(bundlePath))
	digest, err := copyStableSource(ctx, snapshot, destination, s.stagingPath())
	if err != nil {
		return TranscriptSource{}, "", err
	}
	record := TranscriptSource{
		SessionID: sessionID,
		RawSource: artifact.RawSourceRef{
			Hash: digest, Size: snapshot.Size,
			MediaType: "application/jsonl", Path: bundlePath,
		},
	}
	if err := artifact.ValidateRawSource(&record.RawSource); err != nil {
		return TranscriptSource{}, "", err
	}
	return record, destination, nil
}

func (s *captureState) sourceBundlePath(sourcePath string) (string, error) {
	relative, err := filepath.Rel(s.manifest.ProviderRoot, sourcePath)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("capture source is outside provider root")
	}
	return filepath.ToSlash(filepath.Join(
		bundleSourcePrefix(Provider(s.manifest.Provider)), relative)), nil
}

func copyStableSource(
	ctx context.Context,
	snapshot sourceSnapshot,
	destination, stagingDir string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	before, err := os.Lstat(snapshot.Path)
	if err != nil {
		return "", errorWithReason(
			ReasonSourceUnavailable, "observed capture source is unavailable")
	}
	if !before.Mode().IsRegular() || before.Size() != snapshot.Size ||
		!before.ModTime().Equal(snapshot.ModTime) {
		return "", errSourceChanged
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", fmt.Errorf("creating transcript bundle directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(destination), 0o700); err != nil {
		return "", fmt.Errorf("securing transcript bundle directory: %w", err)
	}
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return "", fmt.Errorf("creating transcript staging directory: %w", err)
	}
	if err := os.Chmod(stagingDir, 0o700); err != nil {
		return "", fmt.Errorf("securing transcript staging directory: %w", err)
	}
	source, err := os.Open(snapshot.Path)
	if err != nil {
		return "", errorWithReason(
			ReasonSourceUnavailable, "observed capture source is unavailable")
	}
	defer source.Close()
	temporary, err := os.CreateTemp(stagingDir, ".source-*")
	if err != nil {
		return "", fmt.Errorf("creating transcript source copy: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", err
	}
	hash := sha256.New()
	written, copyErr := copyWithContext(
		ctx,
		io.MultiWriter(temporary, hash),
		io.LimitReader(source, snapshot.Size+1),
	)
	if copyErr == nil && written != snapshot.Size {
		copyErr = errSourceChanged
	}
	if copyErr == nil {
		copyErr = ctx.Err()
	}
	if copyErr == nil {
		copyErr = temporary.Sync()
	}
	copyErr = errors.Join(copyErr, temporary.Close())
	if copyErr != nil {
		return "", copyErr
	}
	after, err := os.Lstat(snapshot.Path)
	if err != nil {
		return "", errorWithReason(
			ReasonSourceUnavailable, "observed capture source is unavailable")
	}
	if !after.Mode().IsRegular() || after.Size() != snapshot.Size ||
		!after.ModTime().Equal(snapshot.ModTime) {
		return "", errSourceChanged
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("installing transcript source copy: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return "", fmt.Errorf("securing transcript source copy: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	reader := &contextReader{ctx: ctx, reader: source}
	written, err := io.CopyBuffer(destination, reader, make([]byte, 64<<10))
	if err == nil {
		err = ctx.Err()
	}
	return written, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

func (s *captureState) clearSourceStaging() error {
	entries, err := os.ReadDir(s.stagingPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > s.manifest.Limits.MaxSources {
		return errorWithReason(
			ReasonSourceLimit, "transcript staging entry count exceeds limit")
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".source-") {
			return errors.New("transcript staging directory contains an unknown entry")
		}
		path := filepath.Join(s.stagingPath(), entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("transcript staging entry is not a regular file")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func replaceTranscriptSource(
	records []TranscriptSource,
	record TranscriptSource,
) []TranscriptSource {
	for i := range records {
		if records[i].RawSource.Path == record.RawSource.Path {
			records[i] = record
			return records
		}
	}
	return append(records, record)
}

func validatePersistedSourceBounds(records []TranscriptSource, limits Limits) error {
	if len(records) > limits.MaxSources {
		return errorWithReason(ReasonSourceLimit, "source count exceeds limit")
	}
	var total int64
	for _, record := range records {
		if record.RawSource.Size > limits.MaxSourceBytes {
			return errorWithReason(
				ReasonSourceBytesLimit, "capture source exceeds per-file byte limit")
		}
		total += record.RawSource.Size
		if total > limits.MaxTotalBytes {
			return errorWithReason(
				ReasonSourceBytesLimit, "capture sources exceed aggregate byte limit")
		}
	}
	return nil
}

func sourceSessionID(provider Provider, sourcePath string) string {
	if provider == ProviderCodex {
		return strings.ToLower(
			parser.CodexSessionUUIDFromFilename(filepath.Base(sourcePath)))
	}
	return strings.TrimSuffix(filepath.Base(sourcePath), ".jsonl")
}

func (s *captureState) completeTranscriptBundle(
	ctx context.Context,
	sourceVersion string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sort.Slice(s.manifest.Sources, func(i, j int) bool {
		return s.manifest.Sources[i].RawSource.Path <
			s.manifest.Sources[j].RawSource.Path
	})
	if err := s.pruneUnlistedSources(ctx); err != nil {
		return err
	}
	bundle := TranscriptBundle{
		Schema: Schema{
			Name: TranscriptBundleSchemaName, Version: TranscriptBundleSchemaVersion,
		},
		OccurrenceID: s.manifest.OccurrenceID,
		Provider:     Provider(s.manifest.Provider),
		Sources:      append([]TranscriptSource(nil), s.manifest.Sources...),
		Producer: ProducerMetadata{
			AgentsViewVersion: bounded(s.manifest.AgentsViewVersion, 128),
			ParserDataVersion: db.CurrentDataVersion(),
			Invocation:        s.manifest.Invocation,
			SourceVersion:     bounded(sourceVersion, 128),
		},
	}
	data, err := encodeTranscriptBundle(bundle, s.manifest.Limits.MaxResultBytes)
	if err != nil {
		return err
	}
	if err := atomicWriteContext(ctx, s.bundlePath(), data); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.manifest.SourcesComplete = true
	return s.saveManifestContext(ctx)
}

func (s *captureState) pruneUnlistedSources(ctx context.Context) error {
	// sources is owned by one capture. Remove JSONL left by an interrupted
	// attempt so the completed bundle is the complete uploadable source set.
	expected := make(map[string]bool, len(s.manifest.Sources))
	for _, source := range s.manifest.Sources {
		expected[filepath.Clean(s.sourcesPath(
			filepath.FromSlash(source.RawSource.Path)))] = true
	}
	examined := 0
	return filepath.WalkDir(s.sourcesPath(), func(
		path string,
		entry os.DirEntry,
		walkErr error,
	) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		examined++
		if examined > s.manifest.Limits.MaxSources*8 {
			return errorWithReason(
				ReasonSourceLimit, "transcript bundle tree exceeds entry limit")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return errors.New("transcript bundle contains a symbolic link")
		}
		if entry.IsDir() || path == s.bundlePath() || expected[filepath.Clean(path)] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("transcript bundle contains an unknown entry")
		}
		if strings.HasPrefix(entry.Name(), ".agentsview-capture-") {
			if err := verifyCapturePathOwner(path); err != nil {
				return err
			}
			return os.Remove(path)
		}
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			return errors.New("transcript bundle contains an unknown entry")
		}
		return os.Remove(path)
	})
}

func (s *captureState) loadPersistedSources(
	ctx context.Context,
) ([]string, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	info, statErr := os.Lstat(s.bundlePath())
	if statErr == nil && (!info.Mode().IsRegular() ||
		info.Size() > int64(s.manifest.Limits.MaxResultBytes)) {
		return nil, false, errors.New("transcript bundle exceeds size limit")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, false, statErr
	}
	file, err := os.Open(s.bundlePath())
	if errors.Is(err, os.ErrNotExist) {
		if s.manifest.SourcesComplete {
			return nil, false, errorWithReason(
				ReasonSourceUnavailable, "transcript bundle is unavailable")
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	bundle, decodeErr := DecodeTranscriptBundle(&contextReader{
		ctx: ctx,
		reader: io.LimitReader(
			file, int64(s.manifest.Limits.MaxResultBytes)+1),
	})
	closeErr := file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return nil, false, err
	}
	if bundle.OccurrenceID != s.manifest.OccurrenceID ||
		string(bundle.Provider) != s.manifest.Provider {
		return nil, false, errors.New("transcript bundle conflicts with capture manifest")
	}
	hasRoot := false
	for _, source := range bundle.Sources {
		if strings.EqualFold(source.SessionID, s.manifest.ProviderSessionID) {
			hasRoot = true
			break
		}
	}
	if !hasRoot {
		return nil, false, errors.New("transcript bundle omits the root provider session")
	}
	if err := validatePersistedSourceBounds(bundle.Sources, s.manifest.Limits); err != nil {
		return nil, false, err
	}
	paths, err := s.validatePersistedSourceFiles(ctx, bundle.Sources)
	if err != nil {
		return nil, false, err
	}
	if s.manifest.SourcesComplete &&
		!sameTranscriptSources(s.manifest.Sources, bundle.Sources) {
		return nil, false, errors.New("transcript bundle conflicts with persisted sources")
	}
	if !s.manifest.SourcesComplete {
		s.manifest.Sources = append([]TranscriptSource(nil), bundle.Sources...)
		s.manifest.SourcesComplete = true
		if err := s.saveManifestContext(ctx); err != nil {
			return nil, false, err
		}
	}
	return paths, true, nil
}

func sameTranscriptSources(a, b []TranscriptSource) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *captureState) validatePersistedSourceFiles(
	ctx context.Context,
	records []TranscriptSource,
) ([]string, error) {
	paths := make([]string, 0, len(records))
	var total int64
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := s.sourcesPath(filepath.FromSlash(record.RawSource.Path))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != record.RawSource.Size {
			return nil, errorWithReason(
				ReasonSourceUnavailable, "persisted transcript source is unavailable")
		}
		total += info.Size()
		if total > s.manifest.Limits.MaxTotalBytes {
			return nil, errorWithReason(
				ReasonSourceBytesLimit, "capture sources exceed aggregate byte limit")
		}
		digest, err := hashBoundedFile(ctx, path, record.RawSource.Size)
		if err != nil {
			return nil, err
		}
		if digest != record.RawSource.Hash {
			return nil, errorWithReason(
				ReasonSourceUnavailable,
				"persisted transcript source digest conflict",
			)
		}
		if err := validateJSONLLines(ctx, path, s.manifest.Limits.MaxLineBytes); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func hashBoundedFile(ctx context.Context, path string, size int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := copyWithContext(ctx, hash, io.LimitReader(file, size+1))
	if err != nil {
		return "", err
	}
	if written != size {
		return "", errors.New("persisted transcript source size changed")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *captureState) capturedProviderRoot() string {
	return s.sourcesPath(filepath.FromSlash(bundleSourcePrefix(
		Provider(s.manifest.Provider))))
}
