package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	manifestFileName = "manifest.json"
	archiveFileName  = "capture.db"
	sealedFileName   = "result.json"
	lockFileName     = "capture.lock"
	stagingDirName   = ".source-staging"
	manifestVersion  = 2
	minResultBytes   = 64 << 10
	maxManifestBytes = 256 << 10
)

var errAttemptExists = errors.New("capture failure result already exists")

type Limits struct {
	MaxOccurrenceBytes int           `json:"max_occurrence_bytes"`
	MaxSources         int           `json:"max_sources"`
	MaxSourceBytes     int64         `json:"max_source_bytes"`
	MaxTotalBytes      int64         `json:"max_total_bytes"`
	MaxLineBytes       int           `json:"max_line_bytes"`
	MaxResultBytes     int           `json:"max_result_bytes"`
	FinalizationWait   time.Duration `json:"finalization_wait"`
	Quiescence         time.Duration `json:"quiescence"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxOccurrenceBytes: 256,
		MaxSources:         maxContractSources,
		MaxSourceBytes:     64 << 20,
		MaxTotalBytes:      256 << 20,
		MaxLineBytes:       8 << 20,
		MaxResultBytes:     256 << 10,
		FinalizationWait:   15 * time.Second,
		Quiescence:         500 * time.Millisecond,
	}
}

func validateLimits(limits Limits) error {
	if limits.MaxOccurrenceBytes <= 0 || limits.MaxSources <= 0 ||
		limits.MaxSourceBytes <= 0 || limits.MaxTotalBytes <= 0 ||
		limits.MaxLineBytes <= 0 || limits.MaxResultBytes < minResultBytes ||
		limits.FinalizationWait <= 0 || limits.Quiescence <= 0 {
		return errors.New("capture limits must be positive and permit a bounded result")
	}
	if limits.MaxSourceBytes > limits.MaxTotalBytes {
		return errors.New("per-source byte limit exceeds aggregate byte limit")
	}
	if limits.Quiescence >= limits.FinalizationWait {
		return errors.New("quiescence must be shorter than finalization wait")
	}
	if limits.MaxSources > maxContractSources {
		return errors.New("source count limit exceeds result contract bound")
	}
	return nil
}

type manifest struct {
	Version           int                `json:"version"`
	OccurrenceID      string             `json:"occurrence_id"`
	Provider          string             `json:"provider"`
	ProviderSessionID string             `json:"provider_session_id,omitempty"`
	ProviderRoot      string             `json:"provider_root"`
	ProviderWorkDir   string             `json:"work_dir"`
	StartedAt         time.Time          `json:"started_at"`
	Execution         ExecutionOutcome   `json:"execution"`
	Invocation        string             `json:"invocation"`
	AgentsViewVersion string             `json:"agentsview_version,omitempty"`
	CorrelationError  ReasonCode         `json:"correlation_error,omitempty"`
	TerminalError     ReasonCode         `json:"terminal_error,omitempty"`
	SourceObserved    bool               `json:"source_observed,omitempty"`
	SourcesComplete   bool               `json:"sources_complete,omitempty"`
	Sources           []TranscriptSource `json:"sources,omitempty"`
	Limits            Limits             `json:"limits"`
	SealedDigest      string             `json:"sealed_digest,omitempty"`
}

type captureState struct {
	dir                   string
	lock                  *flock.Flock
	createdInfo           os.FileInfo
	manifest              manifest
	finalizationDeadline  time.Time
	afterPersistedSources func()
}

func createState(dir string, m manifest) (*captureState, error) {
	if dir == "" {
		return nil, errors.New("capture directory is required")
	}
	m.Version = manifestVersion
	if err := validateManifest(m); err != nil {
		return nil, err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving capture directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absDir), 0o700); err != nil {
		return nil, fmt.Errorf("creating capture parent directory: %w", err)
	}
	absDir, err = resolveCaptureDirectory(absDir)
	if err != nil {
		return nil, err
	}
	if err := verifyCaptureParentSafety(absDir); err != nil {
		return nil, fmt.Errorf("validating capture parent safety: %w", err)
	}
	if err := createSecureCaptureDirectory(absDir); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errors.New(
				"capture directory already exists; use capture report for an occurrence",
			)
		}
		return nil, fmt.Errorf("creating capture directory: %w", err)
	}
	createdInfo, err := os.Lstat(absDir)
	if err != nil {
		_ = os.Remove(absDir)
		return nil, fmt.Errorf("checking new capture directory: %w", err)
	}
	cleanup := true
	lockAcquired := false
	defer func() {
		if cleanup {
			removeIncompleteNewState(absDir, createdInfo, lockAcquired)
		}
	}()
	if err := secureCaptureDirectory(absDir); err != nil {
		return nil, fmt.Errorf("securing capture directory: %w", err)
	}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("checking new capture directory contents: %w", err)
	}
	if len(entries) != 0 {
		return nil, errors.New("new capture directory is not empty")
	}
	state := &captureState{
		dir: absDir, lock: flock.New(filepath.Join(absDir, lockFileName)),
		createdInfo: createdInfo,
	}
	locked, err := state.lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("locking capture: %w", err)
	}
	if !locked {
		return nil, errors.New("capture is already active")
	}
	lockAcquired = true
	if err := os.Chmod(filepath.Join(absDir, lockFileName), 0o600); err != nil {
		state.close()
		return nil, fmt.Errorf("securing capture lock: %w", err)
	}
	state.manifest = m
	if err := state.saveManifest(); err != nil {
		state.close()
		return nil, err
	}
	cleanup = false
	return state, nil
}

func openState(dir string) (*captureState, error) {
	absDir, _, err := inspectCaptureState(dir)
	if err != nil {
		return nil, err
	}
	validatedInfo, err := os.Lstat(absDir)
	if err != nil {
		return nil, fmt.Errorf("checking capture directory identity: %w", err)
	}
	if err := secureCaptureDirectory(absDir); err != nil {
		return nil, fmt.Errorf("securing capture directory: %w", err)
	}
	state := &captureState{dir: absDir, lock: flock.New(filepath.Join(absDir, lockFileName))}
	locked, err := state.lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("locking capture: %w", err)
	}
	if !locked {
		return nil, errors.New("capture is still active")
	}
	if err := os.Chmod(filepath.Join(absDir, lockFileName), 0o600); err != nil {
		state.close()
		return nil, fmt.Errorf("securing capture lock: %w", err)
	}
	currentInfo, err := os.Lstat(absDir)
	if err != nil || !os.SameFile(validatedInfo, currentInfo) {
		state.close()
		return nil, errors.New("capture directory changed during recovery")
	}
	if err := verifyCaptureDirectoryOwner(absDir); err != nil {
		state.close()
		return nil, fmt.Errorf("validating capture directory owner: %w", err)
	}
	recovered, err := readCaptureManifest(absDir)
	if err != nil {
		state.close()
		return nil, err
	}
	if err := validateCaptureEntries(absDir, recovered.Limits); err != nil {
		state.close()
		return nil, err
	}
	state.manifest = recovered
	return state, nil
}

func inspectCaptureState(dir string) (string, manifest, error) {
	if dir == "" {
		return "", manifest{}, errors.New("capture directory is required")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", manifest{}, fmt.Errorf("resolving capture directory: %w", err)
	}
	absDir, err = resolveCaptureDirectory(absDir)
	if err != nil {
		return "", manifest{}, err
	}
	if err := verifyCaptureParentSafety(absDir); err != nil {
		return "", manifest{}, fmt.Errorf("validating capture parent safety: %w", err)
	}
	if err := verifyCaptureDirectoryOwner(absDir); err != nil {
		return "", manifest{}, fmt.Errorf("validating capture directory owner: %w", err)
	}
	recovered, err := readCaptureManifest(absDir)
	if err != nil {
		return "", manifest{}, err
	}
	if err := validateCaptureEntries(absDir, recovered.Limits); err != nil {
		return "", manifest{}, err
	}
	return absDir, recovered, nil
}

func resolveCaptureDirectory(path string) (string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("resolving capture parent directory: %w", err)
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func readCaptureManifest(dir string) (manifest, error) {
	path := filepath.Join(dir, manifestFileName)
	info, err := os.Lstat(path)
	if err != nil {
		return manifest{}, fmt.Errorf("reading capture manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
		return manifest{}, errors.New("capture manifest is not a bounded regular file")
	}
	if err := verifyCapturePathOwner(path); err != nil {
		return manifest{}, fmt.Errorf("validating capture manifest owner: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("reading capture manifest: %w", err)
	}
	var recovered manifest
	if err := json.Unmarshal(data, &recovered); err != nil {
		return manifest{}, fmt.Errorf("decoding capture manifest: %w", err)
	}
	if err := validateManifest(recovered); err != nil {
		return manifest{}, err
	}
	return recovered, nil
}

func validateManifest(m manifest) error {
	if m.Version != manifestVersion {
		return fmt.Errorf("unsupported capture manifest version %d", m.Version)
	}
	if m.OccurrenceID == "" || len(m.OccurrenceID) > m.Limits.MaxOccurrenceBytes {
		return errors.New("capture manifest has an invalid occurrence ID")
	}
	provider := Provider(m.Provider)
	if provider != ProviderClaude && provider != ProviderCodex {
		return errors.New("capture manifest has an invalid provider")
	}
	if m.Invocation != invocationName(provider) || m.ProviderRoot == "" ||
		m.ProviderWorkDir == "" || !filepath.IsAbs(m.ProviderRoot) ||
		!filepath.IsAbs(m.ProviderWorkDir) ||
		m.StartedAt.IsZero() {
		return errors.New("capture manifest has invalid producer metadata")
	}
	if (m.ProviderSessionID != "" && !validUUID(m.ProviderSessionID)) ||
		provider == ProviderClaude && m.ProviderSessionID == "" {
		return errors.New("capture manifest has an invalid provider session ID")
	}
	if m.CorrelationError != "" &&
		m.CorrelationError != ReasonCorrelationUnavailable &&
		m.CorrelationError != ReasonCorrelationConflict {
		return errors.New("capture manifest has an invalid correlation error")
	}
	if m.CorrelationError != "" && m.ProviderSessionID != "" {
		return errors.New("capture manifest retains a failed correlation identity")
	}
	if m.TerminalError != "" && m.TerminalError != ReasonChildStartFailed {
		return errors.New("capture manifest has an invalid terminal error")
	}
	if m.TerminalError != "" && m.CorrelationError != "" {
		return errors.New("capture manifest has conflicting unrecoverable errors")
	}
	if err := validateLimits(m.Limits); err != nil {
		return fmt.Errorf("invalid capture manifest limits: %w", err)
	}
	if err := validatePersistedSourceBounds(m.Sources, m.Limits); err != nil {
		return fmt.Errorf("invalid capture manifest sources: %w", err)
	}
	return nil
}

func validateCaptureEntries(dir string, limits Limits) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading capture directory: %w", err)
	}
	if len(entries) > 16 {
		return errors.New("capture directory has unexpected contents")
	}
	hasLock := false
	for _, entry := range entries {
		name := entry.Name()
		hasLock = hasLock || name == lockFileName
		regular := name == manifestFileName || name == archiveFileName ||
			name == archiveFileName+"-wal" || name == archiveFileName+"-shm" ||
			name == archiveFileName+"-journal" || name == sealedFileName ||
			name == lockFileName || strings.HasPrefix(name, ".agentsview-capture-")
		directory := name == sourcesDirName || name == stagingDirName
		if entry.Type()&os.ModeSymlink != 0 ||
			(!regular && !directory) || regular && !entry.Type().IsRegular() ||
			directory && !entry.IsDir() {
			return fmt.Errorf("capture directory contains unexpected entry %q", name)
		}
		entryPath := filepath.Join(dir, name)
		if err := verifyCapturePathOwner(entryPath); err != nil {
			return fmt.Errorf("validating capture entry %q: %w", name, err)
		}
		if directory {
			if err := validateCaptureTreeOwners(entryPath, limits.MaxSources*8+32); err != nil {
				return err
			}
		}
	}
	if !hasLock {
		return errors.New("capture directory is missing its lock file")
	}
	return nil
}

func validateCaptureTreeOwners(root string, maxEntries int) error {
	examined := 0
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		examined++
		if examined > maxEntries {
			return errors.New("capture state exceeds recovery entry limit")
		}
		if err := verifyCapturePathOwner(path); err != nil {
			return fmt.Errorf("validating capture state owner: %w", err)
		}
		return nil
	})
}

func removeIncompleteNewState(
	dir string,
	createdInfo os.FileInfo,
	lockAcquired bool,
) {
	currentInfo, err := os.Lstat(dir)
	if err != nil || !os.SameFile(createdInfo, currentInfo) {
		return
	}
	if lockAcquired {
		_ = os.Remove(filepath.Join(dir, lockFileName))
	}
	_ = os.Remove(filepath.Join(dir, manifestFileName))
	_ = os.Remove(dir)
}

func (s *captureState) discardNewState() error {
	if s.createdInfo == nil {
		return errors.New("capture state is not newly created")
	}
	currentInfo, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("checking new capture directory: %w", err)
	}
	if !os.SameFile(s.createdInfo, currentInfo) {
		return errors.New("new capture directory changed before rollback")
	}
	if s.lock != nil {
		if err := s.lock.Unlock(); err != nil {
			return fmt.Errorf("unlocking new capture state: %w", err)
		}
		s.lock = nil
	}
	for _, path := range []string{s.manifestPath(), filepath.Join(s.dir, lockFileName)} {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing new capture state: %w", err)
		}
	}
	if err := os.Remove(s.dir); err != nil {
		return fmt.Errorf("removing new capture directory: %w", err)
	}
	return nil
}

func (s *captureState) close() {
	if s != nil && s.lock != nil {
		_ = s.lock.Unlock()
	}
}

func (s *captureState) manifestPath() string { return filepath.Join(s.dir, manifestFileName) }
func (s *captureState) archivePath() string  { return filepath.Join(s.dir, archiveFileName) }
func (s *captureState) sealedPath() string   { return filepath.Join(s.dir, sealedFileName) }
func (s *captureState) sourcesPath(parts ...string) string {
	return filepath.Join(append([]string{s.dir, sourcesDirName}, parts...)...)
}
func (s *captureState) bundlePath() string {
	return s.sourcesPath(bundleFileName)
}
func (s *captureState) stagingPath() string {
	return filepath.Join(s.dir, stagingDirName)
}

func (s *captureState) saveManifest() error {
	return s.saveManifestContext(context.Background())
}

func (s *captureState) saveManifestContext(ctx context.Context) error {
	data, err := encodeManifest(s.manifest)
	if err != nil {
		return err
	}
	return atomicWriteContext(ctx, s.manifestPath(), data)
}

func encodeManifest(value manifest) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding capture manifest: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxManifestBytes {
		return nil, errorWithReason(
			ReasonSourceLimit,
			"capture manifest exceeds recovery size limit",
		)
	}
	return data, nil
}

func (s *captureState) validateManifestSources(
	sources []TranscriptSource,
) error {
	prospective := s.manifest
	prospective.Sources = sources
	_, err := encodeManifest(prospective)
	return err
}

func (s *captureState) seal(data []byte) error {
	if len(data) > s.manifest.Limits.MaxResultBytes {
		return errors.New("capture result exceeds size limit")
	}
	ctx, cancel := s.finalizationContext()
	defer cancel()
	if err := atomicWriteContext(ctx, s.sealedPath(), data); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	s.manifest.SealedDigest = hex.EncodeToString(digest[:])
	return s.saveManifestContext(ctx)
}

func (s *captureState) finalizationContext() (context.Context, context.CancelFunc) {
	if s.finalizationDeadline.IsZero() {
		return context.WithCancel(context.Background())
	}
	return context.WithDeadline(context.Background(), s.finalizationDeadline)
}

func (s *captureState) storeAttempt(data []byte, complete bool) error {
	if complete {
		return s.seal(data)
	}
	if len(data) > s.manifest.Limits.MaxResultBytes {
		return errors.New("capture result exceeds size limit")
	}
	if existing, err := s.readAttempt(); err == nil {
		result, decodeErr := DecodeResult(strings.NewReader(string(existing)))
		if decodeErr != nil {
			return fmt.Errorf("decoding existing capture result: %w", decodeErr)
		}
		if result.Reporting.Outcome == ReportingFailed {
			return errAttemptExists
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking existing capture result: %w", err)
	}
	s.manifest.SealedDigest = ""
	if err := atomicWrite(s.sealedPath(), data); err != nil {
		return err
	}
	return s.saveManifest()
}

func (s *captureState) readAttempt() ([]byte, error) {
	data, err := os.ReadFile(s.sealedPath())
	if err != nil {
		return nil, err
	}
	if len(data) > s.manifest.Limits.MaxResultBytes {
		return nil, errors.New("capture result exceeds size limit")
	}
	return data, nil
}

func (s *captureState) restoreAttempt(data []byte) error {
	if len(data) > s.manifest.Limits.MaxResultBytes {
		return errors.New("capture result exceeds size limit")
	}
	s.manifest.SealedDigest = ""
	if err := atomicWrite(s.sealedPath(), data); err != nil {
		return err
	}
	return s.saveManifest()
}

func (s *captureState) readSealed() ([]byte, error) {
	data, err := os.ReadFile(s.sealedPath())
	if err != nil {
		return nil, err
	}
	if len(data) > s.manifest.Limits.MaxResultBytes {
		return nil, errors.New("sealed result exceeds size limit")
	}
	digest := sha256.Sum256(data)
	if s.manifest.SealedDigest == "" ||
		hex.EncodeToString(digest[:]) != s.manifest.SealedDigest {
		return nil, errors.New("sealed result digest conflict")
	}
	return data, nil
}

func atomicWrite(path string, data []byte) error {
	return atomicWriteContext(context.Background(), path, data)
}

func atomicWriteContext(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".agentsview-capture-*")
	if err != nil {
		return fmt.Errorf("creating temporary output: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("securing temporary output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temporary output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temporary output: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temporary output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("installing output: %w", err)
	}
	return nil
}

func writeResult(path string, stdout io.Writer, data []byte) error {
	if path == "-" {
		_, err := stdout.Write(data)
		return err
	}
	return atomicWrite(path, data)
}
