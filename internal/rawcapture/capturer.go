// Package rawcapture captures provider-owned source files into the durable raw
// upload outbox without parsing their content.
package rawcapture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
)

var (
	ErrSourceChanged       = errors.New("rawcapture: source changed during capture")
	ErrUnsupportedSnapshot = errors.New("rawcapture: provider snapshot requirement is not implemented")
	ErrUnsupportedProvider = errors.New("rawcapture: provider does not support raw capture")
	errCleanupIncomplete   = errors.New("rawcapture: cleanup incomplete")
)

// Status is the observable outcome of one capture request.
type Status string

const (
	StatusCaptured  Status = "captured"
	StatusUnchanged Status = "unchanged"
	StatusDegraded  Status = "degraded"
)

type captureMode uint8

const (
	captureFull captureMode = iota
	captureAppend
)

type observedCaptureEntry struct {
	planned  parser.RawCaptureEntry
	root     *os.Root
	relative string
	file     *os.File
	info     os.FileInfo
	identity string
}

type capturedFileState struct {
	length       int64
	modTimeNS    int64
	fileIdentity string
	prefixSHA256 string
}

type captureAssessment struct {
	mode        captureMode
	objectBytes int64
	entries     []rawcheckpoint.CapturedEntry
	appendBases []rawcheckpoint.CapturedEntry
	unchanged   bool
}

// Result identifies the durable source generation created by Capture.
type Result struct {
	Status    Status
	CaptureID string
	Source    rawcheckpoint.SourceIdentity
}

type capturePhase uint8

const (
	capturePhaseBeforeRead capturePhase = iota + 1
	capturePhaseAfterRead
)

// Capturer turns one provider source into a stable durable generation.
type Capturer struct {
	store          *rawcheckpoint.Store
	files          fileOperations
	manifestLimits rawsync.ManifestLimits
	capturePhase   func(capturePhase, string)
	discardObjects func(context.Context, []rawsync.ObjectRef) error
}

// New constructs a capturer over one durable checkpoint store.
func New(store *rawcheckpoint.Store) *Capturer {
	return &Capturer{
		store: store, files: defaultFileOperations(),
		manifestLimits: rawsync.DefaultManifestLimits(),
		discardObjects: store.DiscardUnreferencedObjects,
	}
}

// Capture captures one complete provider source without invoking its parser.
func (c *Capturer) Capture(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
) (result Result, resultErr error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	capabilities := provider.Capabilities().RawCapture
	if capabilities.Snapshot != parser.RawCaptureSnapshotNone ||
		capabilities.Shape == parser.RawCaptureShapeSQLite {
		return Result{}, ErrUnsupportedSnapshot
	}
	plan, supported, err := parser.ResolveRawCapturePlan(ctx, provider, source)
	if err != nil {
		return Result{}, err
	}
	if !supported {
		return Result{}, ErrUnsupportedProvider
	}
	scope, err := openCapturePlanScope(plan)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, scope.Close())
		if resultErr != nil {
			result = Result{}
		}
	}()
	root, err := c.store.ResolveConfiguredRoot(ctx, source.Provider, plan.ConfiguredRoot)
	if err != nil {
		return Result{}, err
	}
	identity := rawcheckpoint.SourceIdentity{
		Provider:         source.Provider,
		ConfiguredRootID: root.ID,
		SourceKey:        plan.SourceKey,
	}
	base, hasBase, err := c.store.CaptureBase(ctx, identity)
	if err != nil {
		return Result{}, err
	}
	observed, sourceBytes, err := c.observePlan(scope)
	if err != nil {
		return Result{}, err
	}
	defer closeObservedEntries(observed)
	provisional := c.provisionalAssessment(observed, sourceBytes, base, hasBase)
	reservationBytes := captureReservationBytes(provisional)
	reservation, err := c.store.ReserveSourceCapture(ctx, identity, reservationBytes)
	if errors.Is(err, rawcheckpoint.ErrOutboxFull) {
		return Result{Status: StatusDegraded, Source: identity}, nil
	}
	if err != nil {
		return Result{}, err
	}
	committed := false
	cleanupIncomplete := false
	var installed []rawsync.ObjectRef
	finishPublication := func() {}
	defer func() {
		if !committed {
			cleanupErr := c.discardObjects(
				context.Background(), installed,
			)
			if cleanupErr == nil && !cleanupIncomplete {
				cleanupErr = c.store.ReleaseReservation(
					context.Background(), reservation.ID,
				)
			}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
		finishPublication()
	}()
	assessment, err := c.assessCapture(ctx, observed, sourceBytes, base, hasBase)
	if err != nil {
		return Result{}, err
	}
	if assessment.unchanged {
		if err := validateCapturePlanStillCurrent(ctx, provider, source, plan, scope); err != nil {
			return Result{}, err
		}
		for i := range observed {
			if err := c.verifyCapturedFile(ctx, observed[i], capturedFileState{
				length:       observed[i].info.Size(),
				modTimeNS:    observed[i].info.ModTime().UnixNano(),
				fileIdentity: observed[i].identity,
				prefixSHA256: assessment.entries[i].PrefixSHA256,
			}); err != nil {
				return Result{}, err
			}
		}
		if err := c.store.CompleteUnchangedCapture(ctx, reservation.ID, identity); err != nil {
			return Result{}, err
		}
		committed = true
		return Result{Status: StatusUnchanged, Source: identity}, nil
	}
	captureID, err := newCaptureID()
	if err != nil {
		return Result{}, err
	}
	capturedAt := c.storeNow()
	if err := c.validateForUpload(identity, captureID, capturedAt, assessment.entries); err != nil {
		if assessment.mode != captureAppend {
			return Result{}, err
		}
		assessment = c.fullAssessment(observed, sourceBytes)
		if err := c.validateForUpload(identity, captureID, capturedAt, assessment.entries); err != nil {
			return Result{}, err
		}
	}
	requiredBytes := captureReservationBytes(assessment)
	if requiredBytes > reservation.ReservedBytes {
		if err := c.store.ReleaseReservation(ctx, reservation.ID); err != nil {
			return Result{}, err
		}
		reservation = rawcheckpoint.Reservation{}
		reservation, err = c.store.ReserveSourceCapture(ctx, identity, requiredBytes)
		if errors.Is(err, rawcheckpoint.ErrOutboxFull) {
			return Result{Status: StatusDegraded, Source: identity}, nil
		}
		if err != nil {
			return Result{}, err
		}
	}
	finishPublication = c.store.BeginObjectPublication()
	if c.capturePhase != nil {
		c.capturePhase(capturePhaseBeforeRead, "")
	}

	entries := make([]rawcheckpoint.CapturedEntry, 0, len(plan.Entries))
	capturedFiles := make([]capturedFileState, 0, len(plan.Entries))
	for i := range observed {
		planned := observed[i].planned
		var entry rawcheckpoint.CapturedEntry
		var newlyInstalled bool
		if assessment.mode == captureAppend && planned.Appendable {
			entry, newlyInstalled, err = c.captureAppendFile(
				ctx, observed[i], assessment.appendBases[i],
			)
		} else if assessment.mode == captureAppend {
			entry, err = c.captureReusedFile(ctx, observed[i], assessment.appendBases[i])
		} else {
			entry, newlyInstalled, err = c.captureFile(ctx, observed[i])
		}
		if newlyInstalled && len(entry.Objects) != 0 {
			installed = append(installed, entry.Objects[len(entry.Objects)-1])
		}
		if err != nil {
			cleanupIncomplete = errors.Is(err, errCleanupIncomplete)
			return Result{}, err
		}
		entries = append(entries, entry)
		capturedFiles = append(capturedFiles, capturedFileState{
			length:       entry.Length,
			modTimeNS:    entry.ModTimeNS,
			fileIdentity: entry.FileIdentity,
			prefixSHA256: entry.PrefixSHA256,
		})
	}
	for i := range observed {
		if err := c.verifyCapturedFile(ctx, observed[i], capturedFiles[i]); err != nil {
			return Result{}, err
		}
	}
	if err := validateCapturePlanStillCurrent(ctx, provider, source, plan, scope); err != nil {
		return Result{}, err
	}
	if err := c.validateForUpload(identity, captureID, capturedAt, entries); err != nil {
		return Result{}, err
	}
	predecessor := ""
	if hasBase {
		predecessor = base.CaptureID
	}
	generation := rawcheckpoint.CapturedGeneration{
		CaptureID:            captureID,
		Source:               identity,
		PredecessorCaptureID: predecessor,
		CapturedAt:           capturedAt,
		Kind:                 rawsync.ManifestSnapshot,
		Entries:              entries,
	}
	if err := c.store.CommitCapture(ctx, reservation.ID, generation); err != nil {
		return Result{}, err
	}
	committed = true
	return Result{
		Status: StatusCaptured, CaptureID: captureID, Source: identity,
	}, nil
}

func (c *Capturer) provisionalAssessment(
	observed []observedCaptureEntry,
	sourceBytes int64,
	base rawcheckpoint.CaptureBaseState,
	hasBase bool,
) captureAssessment {
	full := c.fullAssessment(observed, sourceBytes)
	if !hasBase || len(observed) != len(base.Entries) {
		return full
	}
	byPath := make(map[string]rawcheckpoint.CapturedEntry, len(base.Entries))
	for _, entry := range base.Entries {
		byPath[entry.Path] = entry
	}
	prospective := make([]rawcheckpoint.CapturedEntry, 0, len(observed))
	var suffixBytes int64
	for _, current := range observed {
		previous, ok := byPath[current.planned.Path]
		if !ok || previous.Appendable != current.planned.Appendable ||
			previous.FileIdentity != current.identity {
			return full
		}
		entry := cloneCapturedEntry(previous)
		if current.planned.Appendable {
			if current.info.Size() < previous.Length {
				return full
			}
			entry.Length = current.info.Size()
			entry.ModTimeNS = current.info.ModTime().UnixNano()
			if current.info.Size() > previous.Length {
				growth := current.info.Size() - previous.Length
				suffixBytes += growth
				entry.Objects = append(entry.Objects,
					placeholderObjectRef(len(entry.Objects), growth))
			}
		} else if current.info.Size() != previous.Length {
			return full
		}
		prospective = append(prospective, entry)
	}
	return captureAssessment{
		mode: captureAppend, objectBytes: suffixBytes, entries: prospective,
	}
}

func captureReservationBytes(assessment captureAssessment) int64 {
	objectReferences := 0
	for _, entry := range assessment.entries {
		objectReferences += len(entry.Objects)
	}
	metadataBytes := rawcheckpoint.CaptureMetadataCharge(
		len(assessment.entries), objectReferences,
	)
	if assessment.objectBytes > math.MaxInt64-metadataBytes {
		return math.MaxInt64
	}
	return assessment.objectBytes + metadataBytes
}

func validateCapturePlanStillCurrent(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
	initial parser.RawCapturePlan,
	scope *capturePlanScope,
) error {
	current, supported, err := parser.ResolveRawCapturePlan(ctx, provider, source)
	if err != nil {
		return err
	}
	if !supported || !sameCapturePlan(initial, current) || !scope.MatchesRoots(current) {
		return ErrSourceChanged
	}
	return nil
}

func (c *Capturer) observePlan(
	scope *capturePlanScope,
) ([]observedCaptureEntry, int64, error) {
	observed := make([]observedCaptureEntry, 0, len(scope.entries))
	var sourceBytes int64
	for _, entry := range scope.entries {
		pathInfo, err := c.files.stat(entry.planned.LocalPath)
		if err != nil {
			closeObservedEntries(observed)
			if sourcePathChangedError(err) {
				return nil, 0, ErrSourceChanged
			}
			return nil, 0, fmt.Errorf(
				"rawcapture: stat %q: %w", entry.planned.Path, sanitizeFilesystemError(err),
			)
		}
		file, err := entry.root.Open(entry.relative)
		if err != nil {
			closeObservedEntries(observed)
			if sourcePathChangedError(err) {
				return nil, 0, ErrSourceChanged
			}
			return nil, 0, fmt.Errorf(
				"rawcapture: open %q: %w", entry.planned.Path, sanitizeFilesystemError(err),
			)
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			closeObservedEntries(observed)
			return nil, 0, fmt.Errorf(
				"rawcapture: stat %q: %w", entry.planned.Path, sanitizeFilesystemError(err),
			)
		}
		if !info.Mode().IsRegular() || info.Size() < 0 ||
			!pathInfo.Mode().IsRegular() || !os.SameFile(pathInfo, info) ||
			sourceBytes > int64(^uint64(0)>>1)-info.Size() {
			file.Close()
			closeObservedEntries(observed)
			return nil, 0, ErrSourceChanged
		}
		identity := stableFileIdentity(file, info)
		if identity == "" {
			file.Close()
			closeObservedEntries(observed)
			return nil, 0, ErrSourceChanged
		}
		sourceBytes += info.Size()
		observed = append(observed, observedCaptureEntry{
			planned: entry.planned, root: entry.root, relative: entry.relative,
			file: file, info: info, identity: identity,
		})
	}
	return observed, sourceBytes, nil
}

func sourcePathChangedError(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		sanitizeFilesystemError(err).Error() == "path escapes from parent"
}

func (c *Capturer) assessCapture(
	ctx context.Context,
	observed []observedCaptureEntry,
	sourceBytes int64,
	base rawcheckpoint.CaptureBaseState,
	hasBase bool,
) (captureAssessment, error) {
	full := c.fullAssessment(observed, sourceBytes)
	if !hasBase || len(observed) != len(base.Entries) {
		return full, nil
	}
	byPath := make(map[string]rawcheckpoint.CapturedEntry, len(base.Entries))
	for _, entry := range base.Entries {
		byPath[entry.Path] = entry
	}
	prospective := make([]rawcheckpoint.CapturedEntry, 0, len(observed))
	appendBases := make([]rawcheckpoint.CapturedEntry, 0, len(observed))
	var suffixBytes int64
	for _, current := range observed {
		previous, ok := byPath[current.planned.Path]
		if !ok || previous.Appendable != current.planned.Appendable ||
			previous.FileIdentity != current.identity {
			return full, nil
		}
		appendBases = append(appendBases, cloneCapturedEntry(previous))
		if current.planned.Appendable {
			if current.info.Size() < previous.Length {
				return full, nil
			}
			digest, length, err := hashOpenFilePrefixContext(
				ctx, current.file, previous.Length,
			)
			if err != nil {
				return captureAssessment{}, fmt.Errorf(
					"rawcapture: hash %q: %w", current.planned.Path,
					sanitizeFilesystemError(err),
				)
			}
			if length != previous.Length || digest != previous.PrefixSHA256 {
				return full, nil
			}
			entry := cloneCapturedEntry(previous)
			entry.Length = current.info.Size()
			entry.ModTimeNS = current.info.ModTime().UnixNano()
			if current.info.Size() > previous.Length {
				growth := current.info.Size() - previous.Length
				suffixBytes += growth
				entry.Objects = append(entry.Objects, placeholderObjectRef(len(entry.Objects), growth))
			}
			prospective = append(prospective, entry)
			continue
		}
		digest, length, err := hashOpenFileContext(ctx, current.file)
		if err != nil {
			return captureAssessment{}, fmt.Errorf(
				"rawcapture: hash %q: %w", current.planned.Path,
				sanitizeFilesystemError(err),
			)
		}
		if length != previous.Length || digest != previous.PrefixSHA256 {
			return full, nil
		}
		prospective = append(prospective, cloneCapturedEntry(previous))
	}
	if suffixBytes == 0 {
		return captureAssessment{entries: prospective, unchanged: true}, nil
	}
	return captureAssessment{
		mode: captureAppend, objectBytes: suffixBytes, entries: prospective,
		appendBases: appendBases,
	}, nil
}

func (c *Capturer) fullAssessment(
	observed []observedCaptureEntry,
	sourceBytes int64,
) captureAssessment {
	entries := make([]rawcheckpoint.CapturedEntry, 0, len(observed))
	for i, current := range observed {
		entries = append(entries, rawcheckpoint.CapturedEntry{
			Path:         current.planned.Path,
			Length:       current.info.Size(),
			ModTimeNS:    current.info.ModTime().UnixNano(),
			FileIdentity: current.identity,
			PrefixSHA256: placeholderObjectRef(i, current.info.Size()).SHA256,
			Appendable:   current.planned.Appendable,
			Objects:      []rawsync.ObjectRef{placeholderObjectRef(i, current.info.Size())},
		})
	}
	return captureAssessment{mode: captureFull, objectBytes: sourceBytes, entries: entries}
}

func placeholderObjectRef(index int, length int64) rawsync.ObjectRef {
	return rawsync.ObjectRef{SHA256: fmt.Sprintf("%064x", index+1), Length: length}
}

func (c *Capturer) validateForUpload(
	identity rawcheckpoint.SourceIdentity,
	captureID string,
	capturedAt time.Time,
	entries []rawcheckpoint.CapturedEntry,
) error {
	manifestEntries := make([]rawsync.Entry, 0, len(entries))
	for _, entry := range entries {
		manifestEntries = append(manifestEntries, rawsync.Entry{
			Path: entry.Path, Type: "file", Length: entry.Length,
			Objects: append([]rawsync.ObjectRef(nil), entry.Objects...),
		})
	}
	return rawsync.ValidateManifestForUpload(rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         identity.Provider,
		ConfiguredRootID: identity.ConfiguredRootID,
		SourceKey:        identity.SourceKey,
		CaptureID:        captureID,
		CapturedAt:       capturedAt,
		Kind:             rawsync.ManifestSnapshot,
		Entries:          manifestEntries,
	}, c.manifestLimits)
}

func (c *Capturer) storeNow() time.Time {
	return time.Now().UTC()
}

func newCaptureID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("rawcapture: generate capture ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func cloneCapturedEntry(entry rawcheckpoint.CapturedEntry) rawcheckpoint.CapturedEntry {
	entry.Objects = append([]rawsync.ObjectRef(nil), entry.Objects...)
	return entry
}

func sameCapturePlan(a, b parser.RawCapturePlan) bool {
	if a.ConfiguredRoot != b.ConfiguredRoot || a.CaptureRoot != b.CaptureRoot ||
		a.SourceKey != b.SourceKey || len(a.Entries) != len(b.Entries) {
		return false
	}
	for i := range a.Entries {
		if a.Entries[i] != b.Entries[i] {
			return false
		}
	}
	return true
}

func closeObservedEntries(entries []observedCaptureEntry) {
	for i := range entries {
		_ = entries[i].file.Close()
	}
}
