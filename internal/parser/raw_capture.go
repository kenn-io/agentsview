package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/rawpath"
)

const ProviderFeatureRawCapture = "raw capture"

var ErrInvalidRawCapturePlan = errors.New("invalid raw capture plan")

// RawCaptureShape describes the physical shape a provider exposes for raw
// capture. The zero value is unsupported.
type RawCaptureShape uint8

const (
	RawCaptureShapeUnsupported RawCaptureShape = iota
	RawCaptureShapeFiles
	RawCaptureShapeSQLite
)

// RawCaptureAppendPolicy declares whether a provider can safely extend one
// entry while retaining the prior generation's ordered object references.
type RawCaptureAppendPolicy uint8

const (
	RawCaptureAppendReplaceOnly RawCaptureAppendPolicy = iota
	RawCaptureAppendOne
)

// RawCaptureSnapshotRequirement declares how capture obtains a consistent
// source view. The zero value requires no provider-specific snapshot.
type RawCaptureSnapshotRequirement uint8

const (
	RawCaptureSnapshotNone RawCaptureSnapshotRequirement = iota
	RawCaptureSnapshotOnlineBackup
)

// RawCaptureCapabilities declares a provider's raw source shape. Providers
// default to unsupported and must implement RawCaptureProvider when enabled.
type RawCaptureCapabilities struct {
	Support  CapabilitySupport
	Shape    RawCaptureShape
	Append   RawCaptureAppendPolicy
	Snapshot RawCaptureSnapshotRequirement
}

// RawCaptureEntry maps one provider-owned local file to its logical manifest
// path. Path always uses slash separators and is relative to CaptureRoot.
type RawCaptureEntry struct {
	Path       string
	LocalPath  string
	Appendable bool
}

// RawCapturePlan is the complete physical membership of one logical source.
type RawCapturePlan struct {
	ConfiguredRoot string
	CaptureRoot    string
	SourceKey      string
	Entries        []RawCaptureEntry
}

// RawCaptureDiscovery reports physical sources and whether every configured
// root was enumerated successfully. Incomplete results may still be captured,
// but callers must not infer deletions from omitted sources.
type RawCaptureDiscovery struct {
	Sources  []SourceRef
	Complete bool
}

// RawCaptureProvider is the optional provider-owned physical source contract.
type RawCaptureProvider interface {
	PlanRawCapture(context.Context, SourceRef) (RawCapturePlan, error)
}

// RawCaptureSourceProvider optionally exposes physical raw sources separately
// from the provider's normalized per-session discovery surface.
type RawCaptureSourceProvider interface {
	RawCaptureSourcesForChangedPath(
		context.Context, ChangedPathRequest,
	) ([]SourceRef, error)
}

// StreamingRawCaptureSourceProvider emits physical raw sources without
// retaining the provider's complete discovery result in memory.
type StreamingRawCaptureSourceProvider interface {
	DiscoverRawCaptureSourcesEach(
		context.Context, func(SourceRef) error,
	) (complete bool, err error)
}

type rawCaptureDiscoveryProgressContextKey struct{}

// WithRawCaptureDiscoveryProgress installs the periodic auditor's bounded
// traversal handshake. Raw-capture providers report each filesystem entry or
// configured root before examining it so the auditor can suspend the scan.
func WithRawCaptureDiscoveryProgress(
	ctx context.Context,
	progress func() error,
) context.Context {
	return context.WithValue(ctx, rawCaptureDiscoveryProgressContextKey{}, progress)
}

// ReportRawCaptureDiscoveryProgress reports one physical discovery step when
// a bounded traversal handshake is installed. Other discovery callers pay no
// callback cost beyond the context lookup.
func ReportRawCaptureDiscoveryProgress(ctx context.Context) error {
	progress, _ := ctx.Value(rawCaptureDiscoveryProgressContextKey{}).(func() error)
	if progress == nil {
		return nil
	}
	return progress()
}

// StreamRawCaptureSources discovers physical raw sources through the bounded
// callback surface required by raw-capture providers.
func StreamRawCaptureSources(
	ctx context.Context,
	provider Provider,
	yield func(SourceRef) error,
) (bool, error) {
	if streaming, ok := provider.(StreamingRawCaptureSourceProvider); ok {
		return streaming.DiscoverRawCaptureSourcesEach(ctx, yield)
	}
	return false, UnsupportedProviderFeatureError{
		Provider: provider.Definition().Type,
		Feature:  ProviderFeatureRawCapture,
	}
}

// DiscoverRawCaptureSources discovers physical raw sources without parsing.
func DiscoverRawCaptureSources(
	ctx context.Context,
	provider Provider,
) (RawCaptureDiscovery, error) {
	if _, ok := provider.(StreamingRawCaptureSourceProvider); ok {
		var sources []SourceRef
		complete, err := StreamRawCaptureSources(ctx, provider, func(source SourceRef) error {
			sources = append(sources, source)
			return nil
		})
		sortJSONLSources(sources)
		return RawCaptureDiscovery{Sources: sources, Complete: complete}, err
	}
	if provider.Capabilities().RawCapture.Support == CapabilitySupported {
		return RawCaptureDiscovery{}, UnsupportedProviderFeatureError{
			Provider: provider.Definition().Type,
			Feature:  ProviderFeatureRawCapture,
		}
	}
	if streaming, ok := provider.(StreamingDiscoverer); ok {
		return collectRawCaptureDiscovery(ctx, streaming.DiscoverEach)
	}
	sources, err := provider.Discover(ctx)
	return RawCaptureDiscovery{Sources: sources, Complete: err == nil}, err
}

func collectRawCaptureDiscovery(
	ctx context.Context,
	discover func(context.Context, func(SourceRef) error) error,
) (RawCaptureDiscovery, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	err := discover(ctx, func(source SourceRef) error {
		addJSONLSource(source, &sources, seen)
		return nil
	})
	sortJSONLSources(sources)
	return RawCaptureDiscovery{Sources: sources, Complete: err == nil}, err
}

func rawCaptureIncompleteRootError(
	provider AgentType,
	root string,
	err error,
) (error, bool) {
	if _, ok := errors.AsType[DiscoveryIncompleteError](err); ok {
		return err, true
	}
	if errors.Is(err, errStreamingDirectoryChanged) {
		return incompleteDiscoveryError(
			provider, "discover raw capture root "+root, err,
		), true
	}
	if _, ok := errors.AsType[*os.PathError](err); ok {
		return incompleteDiscoveryError(
			provider, "discover raw capture root "+root, err,
		), true
	}
	return err, false
}

// RawCaptureSourcesForChangedPath maps a watcher event to physical raw
// sources without routing database events through per-session discovery.
func RawCaptureSourcesForChangedPath(
	ctx context.Context,
	provider Provider,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if physical, ok := provider.(RawCaptureSourceProvider); ok {
		return physical.RawCaptureSourcesForChangedPath(ctx, req)
	}
	return provider.SourcesForChangedPath(ctx, req)
}

// ResolveRawCapturePlan returns and validates a provider-owned raw source plan.
// Generic callers never inspect SourceRef.Opaque.
func ResolveRawCapturePlan(
	ctx context.Context,
	provider Provider,
	source SourceRef,
) (RawCapturePlan, bool, error) {
	if provider.Capabilities().RawCapture.Support != CapabilitySupported {
		return RawCapturePlan{}, false, nil
	}
	providerType := provider.Definition().Type
	if source.Provider != providerType {
		return RawCapturePlan{}, false, invalidRawCapturePlan(
			"source provider %q does not match %q", source.Provider, providerType,
		)
	}
	planner, ok := provider.(RawCaptureProvider)
	if !ok {
		return RawCapturePlan{}, false, UnsupportedProviderFeatureError{
			Provider: providerType,
			Feature:  ProviderFeatureRawCapture,
		}
	}
	plan, err := planner.PlanRawCapture(ctx, source)
	if err != nil {
		return RawCapturePlan{}, false, err
	}
	validated, err := validateRawCapturePlan(provider.Capabilities().RawCapture, source, plan)
	if err != nil {
		return RawCapturePlan{}, false, err
	}
	return validated, true, nil
}

func validateRawCapturePlan(
	capabilities RawCaptureCapabilities,
	source SourceRef,
	plan RawCapturePlan,
) (RawCapturePlan, error) {
	switch {
	case capabilities.Shape == RawCaptureShapeFiles &&
		capabilities.Snapshot == RawCaptureSnapshotNone:
	case capabilities.Shape == RawCaptureShapeSQLite &&
		capabilities.Snapshot == RawCaptureSnapshotOnlineBackup &&
		capabilities.Append == RawCaptureAppendReplaceOnly:
	default:
		return RawCapturePlan{}, invalidRawCapturePlan("unsupported source shape or snapshot requirement")
	}
	if plan.SourceKey == "" || plan.SourceKey != source.Key {
		return RawCapturePlan{}, invalidRawCapturePlan("source key does not match provider source")
	}
	configuredRoot, err := validateRawCaptureRoot("configured", plan.ConfiguredRoot)
	if err != nil {
		return RawCapturePlan{}, err
	}
	captureRoot, err := validateRawCaptureRoot("capture", plan.CaptureRoot)
	if err != nil {
		return RawCapturePlan{}, err
	}
	if len(plan.Entries) == 0 {
		return RawCapturePlan{}, invalidRawCapturePlan("source has no entries")
	}

	entries := append([]RawCaptureEntry(nil), plan.Entries...)
	seen := make(map[string]struct{}, len(entries))
	appendable := 0
	for i := range entries {
		logical := entries[i].Path
		if err := rawpath.Validate(logical, rawpath.DefaultMaxBytes); err != nil {
			return RawCapturePlan{}, invalidRawCapturePlan("entry path %q is not a safe relative path", logical)
		}
		if _, exists := seen[logical]; exists {
			return RawCapturePlan{}, invalidRawCapturePlan("entry path %q is duplicated", logical)
		}
		seen[logical] = struct{}{}

		localPath := filepath.Clean(entries[i].LocalPath)
		if !filepath.IsAbs(localPath) {
			return RawCapturePlan{}, invalidRawCapturePlan("entry %q path must be absolute", logical)
		}
		resolvedPath, err := filepath.EvalSymlinks(localPath)
		if err != nil {
			return RawCapturePlan{}, invalidRawCapturePlan(
				"resolve entry %q: %s", logical, rawCaptureFilesystemError(err),
			)
		}
		if !rawCapturePathWithin(captureRoot, resolvedPath) &&
			!rawCapturePathWithin(configuredRoot, resolvedPath) {
			return RawCapturePlan{}, invalidRawCapturePlan("entry %q escapes provider roots", logical)
		}
		info, err := os.Stat(resolvedPath)
		if err != nil {
			return RawCapturePlan{}, invalidRawCapturePlan(
				"stat entry %q: %s", logical, rawCaptureFilesystemError(err),
			)
		}
		if !info.Mode().IsRegular() {
			return RawCapturePlan{}, invalidRawCapturePlan("entry %q is not a regular file", logical)
		}
		entries[i].LocalPath = filepath.Clean(resolvedPath)
		if entries[i].Appendable {
			appendable++
		}
	}
	switch capabilities.Append {
	case RawCaptureAppendReplaceOnly:
		if appendable != 0 {
			return RawCapturePlan{}, invalidRawCapturePlan("replace-only source has an appendable entry")
		}
	case RawCaptureAppendOne:
		if appendable != 1 {
			return RawCapturePlan{}, invalidRawCapturePlan("source must have exactly one appendable entry")
		}
	default:
		return RawCapturePlan{}, invalidRawCapturePlan("unknown append policy %d", capabilities.Append)
	}
	if capabilities.Shape == RawCaptureShapeSQLite && len(entries) != 1 {
		return RawCapturePlan{}, invalidRawCapturePlan("SQLite source must have exactly one entry")
	}
	slices.SortFunc(entries, func(a, b RawCaptureEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	return RawCapturePlan{
		ConfiguredRoot: configuredRoot,
		CaptureRoot:    captureRoot,
		SourceKey:      plan.SourceKey,
		Entries:        entries,
	}, nil
}

func validateRawCaptureRoot(kind, root string) (string, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return "", invalidRawCapturePlan("%s root must be absolute", kind)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", invalidRawCapturePlan(
			"resolve %s root: %s", kind, rawCaptureFilesystemError(err),
		)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", invalidRawCapturePlan(
			"stat %s root: %s", kind, rawCaptureFilesystemError(err),
		)
	}
	if !info.IsDir() {
		return "", invalidRawCapturePlan("%s root is not a directory", kind)
	}
	return filepath.Clean(resolved), nil
}

func rawCapturePathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func invalidRawCapturePlan(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRawCapturePlan, fmt.Sprintf(format, args...))
}

func rawCaptureFilesystemError(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "not found"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.Is(err, os.ErrInvalid):
		return "invalid path"
	default:
		return "filesystem error"
	}
}
