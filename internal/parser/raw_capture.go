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

// RawCaptureProvider is the optional provider-owned physical source contract.
type RawCaptureProvider interface {
	PlanRawCapture(context.Context, SourceRef) (RawCapturePlan, error)
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
	if capabilities.Shape != RawCaptureShapeFiles ||
		capabilities.Snapshot != RawCaptureSnapshotNone {
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
