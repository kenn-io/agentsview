// Package rawsync defines the authenticated raw-ingest custody domain.
package rawsync

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/parser"
)

var (
	ErrInvalid       = errors.New("invalid raw sync value")
	ErrNotFound      = errors.New("raw sync object not found")
	ErrConflict      = errors.New("raw sync conflict")
	ErrMissingObject = errors.New("raw sync manifest references missing object")
)

const (
	ManifestSchemaVersion = 1
	maxOpaqueIDBytes      = 128
	maxSourceKeyBytes     = 4096
)

// AuthIdentity is the tenant and device identity supplied by authentication.
type AuthIdentity struct {
	TenantID string
	DeviceID string
}

// NewAuthIdentity validates authenticated tenant and device identifiers.
func NewAuthIdentity(tenantID, deviceID string) (AuthIdentity, error) {
	if err := validateOpaqueID("tenant", tenantID); err != nil {
		return AuthIdentity{}, err
	}
	if err := validateOpaqueID("device", deviceID); err != nil {
		return AuthIdentity{}, err
	}
	return AuthIdentity{TenantID: tenantID, DeviceID: deviceID}, nil
}

// ObjectRef is the semantic identity of one immutable source object.
type ObjectRef struct {
	SHA256 string `json:"sha256"`
	Length int64  `json:"length"`
}

// NewObjectRef validates and constructs an immutable object reference.
func NewObjectRef(sha256 string, length int64) (ObjectRef, error) {
	if !isCanonicalSHA256(sha256) {
		return ObjectRef{}, fmt.Errorf("%w: object digest must be lowercase SHA-256", ErrInvalid)
	}
	if length < 0 {
		return ObjectRef{}, fmt.Errorf("%w: object length must not be negative", ErrInvalid)
	}
	return ObjectRef{SHA256: sha256, Length: length}, nil
}

// ManifestKind identifies whether a manifest captures source files or removal.
type ManifestKind string

const (
	ManifestSnapshot  ManifestKind = "snapshot"
	ManifestTombstone ManifestKind = "tombstone"
)

// Entry describes one logical source file and its ordered object slices.
type Entry struct {
	Path    string      `json:"path"`
	Type    string      `json:"type"`
	Length  int64       `json:"length"`
	Objects []ObjectRef `json:"objects"`
}

// Manifest declares one complete logical provider-source generation.
type Manifest struct {
	SchemaVersion         int              `json:"schema_version"`
	Provider              parser.AgentType `json:"provider"`
	ConfiguredRootID      string           `json:"configured_root_id"`
	SourceKey             string           `json:"source_key"`
	ExpectedParentReceipt string           `json:"expected_parent_receipt,omitempty"`
	CaptureID             string           `json:"capture_id"`
	CapturedAt            time.Time        `json:"captured_at"`
	Kind                  ManifestKind     `json:"kind"`
	Entries               []Entry          `json:"entries,omitempty"`
}

// ManifestLimits bounds work and retained metadata before object-store access.
type ManifestLimits struct {
	MaxCanonicalBytes int
	MaxEntries        int
	MaxObjects        int
	MaxPathBytes      int
	MaxFileBytes      int64
}

// DefaultManifestLimits returns the production manifest validation bounds.
func DefaultManifestLimits() ManifestLimits {
	return ManifestLimits{
		MaxCanonicalBytes: 1 << 20,
		MaxEntries:        4096,
		MaxObjects:        16384,
		MaxPathBytes:      4096,
		MaxFileBytes:      16 << 30,
	}
}

// CanonicalManifest is an authenticated, validated manifest ready for custody.
type CanonicalManifest struct {
	Identity      AuthIdentity
	Manifest      Manifest
	ManifestID    string
	CanonicalJSON []byte
	Objects       []ObjectRef
}

type canonicalEnvelope struct {
	SchemaVersion         int              `json:"schema_version"`
	TenantID              string           `json:"tenant_id"`
	DeviceID              string           `json:"device_id"`
	Provider              parser.AgentType `json:"provider"`
	ConfiguredRootID      string           `json:"configured_root_id"`
	SourceKey             string           `json:"source_key"`
	ExpectedParentReceipt string           `json:"expected_parent_receipt,omitempty"`
	CaptureID             string           `json:"capture_id"`
	CapturedAt            time.Time        `json:"captured_at"`
	Kind                  ManifestKind     `json:"kind"`
	Entries               []Entry          `json:"entries,omitempty"`
}

// ValidateAndCanonicalize binds authentication to validated canonical bytes.
func ValidateAndCanonicalize(
	identity AuthIdentity,
	manifest Manifest,
	limits ManifestLimits,
) (CanonicalManifest, error) {
	canonicalIdentity, err := NewAuthIdentity(identity.TenantID, identity.DeviceID)
	if err != nil || canonicalIdentity != identity {
		return CanonicalManifest{}, fmt.Errorf("%w: authenticated identity is not canonical", ErrInvalid)
	}
	if err := validateManifestLimits(limits); err != nil {
		return CanonicalManifest{}, err
	}
	if err := validateManifestHeader(manifest); err != nil {
		return CanonicalManifest{}, err
	}
	if err := validateManifestCardinality(manifest, limits); err != nil {
		return CanonicalManifest{}, err
	}

	canonical := manifest
	canonical.CapturedAt = manifest.CapturedAt.UTC()
	canonical.Entries = cloneEntries(manifest.Entries)
	slices.SortFunc(canonical.Entries, func(a, b Entry) int {
		return strings.Compare(a.Path, b.Path)
	})
	objects, err := validateEntries(canonical, limits)
	if err != nil {
		return CanonicalManifest{}, err
	}

	envelope := canonicalEnvelope{
		SchemaVersion:         canonical.SchemaVersion,
		TenantID:              identity.TenantID,
		DeviceID:              identity.DeviceID,
		Provider:              canonical.Provider,
		ConfiguredRootID:      canonical.ConfiguredRootID,
		SourceKey:             canonical.SourceKey,
		ExpectedParentReceipt: canonical.ExpectedParentReceipt,
		CaptureID:             canonical.CaptureID,
		CapturedAt:            canonical.CapturedAt,
		Kind:                  canonical.Kind,
		Entries:               canonical.Entries,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return CanonicalManifest{}, fmt.Errorf("encoding canonical raw manifest: %w", err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > limits.MaxCanonicalBytes {
		return CanonicalManifest{}, fmt.Errorf(
			"%w: canonical manifest exceeds %d bytes", ErrInvalid, limits.MaxCanonicalBytes,
		)
	}
	digest := sha256.Sum256(encoded)
	return CanonicalManifest{
		Identity:      identity,
		Manifest:      canonical,
		ManifestID:    hex.EncodeToString(digest[:]),
		CanonicalJSON: encoded,
		Objects:       objects,
	}, nil
}

// ValidateCanonicalManifest verifies canonical integrity without imposing a
// deployment's policy limits a second time.
func ValidateCanonicalManifest(manifest CanonicalManifest) error {
	limits := integrityManifestLimits(manifest)
	validated, err := ValidateAndCanonicalize(manifest.Identity, manifest.Manifest, limits)
	if err != nil || validated.ManifestID != manifest.ManifestID ||
		!bytes.Equal(validated.CanonicalJSON, manifest.CanonicalJSON) ||
		!slices.Equal(validated.Objects, manifest.Objects) ||
		!manifestsEqual(validated.Manifest, manifest.Manifest) {
		return fmt.Errorf("%w: canonical raw manifest is inconsistent", ErrInvalid)
	}
	return nil
}

// manifestsEqual reports whether two manifests are identical, including entry
// order and the captured-at location, so a noncanonical struct cannot ride
// along with canonical bytes.
func manifestsEqual(a, b Manifest) bool {
	return a.SchemaVersion == b.SchemaVersion &&
		a.Provider == b.Provider &&
		a.ConfiguredRootID == b.ConfiguredRootID &&
		a.SourceKey == b.SourceKey &&
		a.ExpectedParentReceipt == b.ExpectedParentReceipt &&
		a.CaptureID == b.CaptureID &&
		a.CapturedAt.Equal(b.CapturedAt) &&
		a.CapturedAt.Location() == b.CapturedAt.Location() &&
		a.Kind == b.Kind &&
		slices.EqualFunc(a.Entries, b.Entries, entriesEqual)
}

func entriesEqual(a, b Entry) bool {
	return a.Path == b.Path && a.Type == b.Type && a.Length == b.Length &&
		slices.Equal(a.Objects, b.Objects)
}

func validateManifestLimits(limits ManifestLimits) error {
	if limits.MaxCanonicalBytes <= 0 || limits.MaxEntries <= 0 ||
		limits.MaxObjects <= 0 || limits.MaxPathBytes <= 0 || limits.MaxFileBytes <= 0 {
		return fmt.Errorf("%w: manifest limits must be positive", ErrInvalid)
	}
	return nil
}

func validateManifestCardinality(manifest Manifest, limits ManifestLimits) error {
	if len(manifest.Entries) > limits.MaxEntries {
		return fmt.Errorf("%w: manifest entry limit exceeded", ErrInvalid)
	}
	objectCount := 0
	for _, entry := range manifest.Entries {
		if len(entry.Objects) > limits.MaxObjects-objectCount {
			return fmt.Errorf("%w: manifest object limit exceeded", ErrInvalid)
		}
		objectCount += len(entry.Objects)
	}
	return nil
}

func integrityManifestLimits(manifest CanonicalManifest) ManifestLimits {
	limits := ManifestLimits{
		MaxCanonicalBytes: max(1, len(manifest.CanonicalJSON)),
		MaxEntries:        max(1, len(manifest.Manifest.Entries)),
		MaxObjects:        1,
		MaxPathBytes:      1,
		MaxFileBytes:      1,
	}
	for _, entry := range manifest.Manifest.Entries {
		limits.MaxObjects += len(entry.Objects)
		limits.MaxPathBytes = max(limits.MaxPathBytes, len(entry.Path))
		limits.MaxFileBytes = max(limits.MaxFileBytes, entry.Length)
	}
	return limits
}

func validateManifestHeader(manifest Manifest) error {
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("%w: unsupported manifest schema version %d", ErrInvalid, manifest.SchemaVersion)
	}
	if err := validateProvider(manifest.Provider); err != nil {
		return err
	}
	if err := validateOpaqueID("configured root", manifest.ConfiguredRootID); err != nil {
		return err
	}
	if err := validateSourceKey(manifest.SourceKey); err != nil {
		return err
	}
	if manifest.ExpectedParentReceipt != "" && !isCanonicalSHA256(manifest.ExpectedParentReceipt) {
		return fmt.Errorf("%w: expected parent receipt must be lowercase hexadecimal", ErrInvalid)
	}
	if err := validateOpaqueID("capture", manifest.CaptureID); err != nil {
		return err
	}
	if manifest.CapturedAt.IsZero() {
		return fmt.Errorf("%w: capture time is required", ErrInvalid)
	}
	switch manifest.Kind {
	case ManifestSnapshot:
		if len(manifest.Entries) == 0 {
			return fmt.Errorf("%w: snapshot manifest requires entries", ErrInvalid)
		}
	case ManifestTombstone:
		if len(manifest.Entries) != 0 {
			return fmt.Errorf("%w: tombstone manifest cannot contain entries", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported manifest kind %q", ErrInvalid, manifest.Kind)
	}
	return nil
}

func validateSourceKey(value string) error {
	if value == "" || len(value) > maxSourceKeyBytes || !utf8.ValidString(value) {
		return fmt.Errorf("%w: source key is missing, oversized, or invalid UTF-8", ErrInvalid)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: source key contains a control character", ErrInvalid)
		}
	}
	return nil
}

func validateEntries(manifest Manifest, limits ManifestLimits) ([]ObjectRef, error) {
	if len(manifest.Entries) > limits.MaxEntries {
		return nil, fmt.Errorf("%w: manifest entry limit exceeded", ErrInvalid)
	}
	byDigest := make(map[string]ObjectRef)
	objectCount := 0
	previousPath := ""
	for _, entry := range manifest.Entries {
		if err := validateEntryPath(entry.Path, limits.MaxPathBytes); err != nil {
			return nil, err
		}
		if entry.Path == previousPath {
			return nil, fmt.Errorf("%w: duplicate manifest path %q", ErrInvalid, entry.Path)
		}
		previousPath = entry.Path
		if entry.Type != "file" {
			return nil, fmt.Errorf("%w: unsupported entry type %q", ErrInvalid, entry.Type)
		}
		if entry.Length < 0 || entry.Length > limits.MaxFileBytes {
			return nil, fmt.Errorf("%w: entry length is outside configured limits", ErrInvalid)
		}
		if len(entry.Objects) == 0 {
			return nil, fmt.Errorf("%w: file entry requires at least one object", ErrInvalid)
		}
		objectCount += len(entry.Objects)
		if objectCount > limits.MaxObjects {
			return nil, fmt.Errorf("%w: manifest object limit exceeded", ErrInvalid)
		}
		var total int64
		for _, object := range entry.Objects {
			validated, objectErr := NewObjectRef(object.SHA256, object.Length)
			if objectErr != nil || validated != object {
				return nil, fmt.Errorf("%w: invalid object reference", ErrInvalid)
			}
			if object.Length > entry.Length-total {
				return nil, fmt.Errorf("%w: object lengths exceed entry length", ErrInvalid)
			}
			total += object.Length
			if previous, ok := byDigest[object.SHA256]; ok && previous.Length != object.Length {
				return nil, fmt.Errorf("%w: digest has conflicting lengths", ErrInvalid)
			}
			byDigest[object.SHA256] = object
		}
		if total != entry.Length {
			return nil, fmt.Errorf("%w: object lengths do not equal entry length", ErrInvalid)
		}
	}
	objects := make([]ObjectRef, 0, len(byDigest))
	for _, object := range byDigest {
		objects = append(objects, object)
	}
	slices.SortFunc(objects, func(a, b ObjectRef) int {
		if byHash := strings.Compare(a.SHA256, b.SHA256); byHash != 0 {
			return byHash
		}
		return cmp.Compare(a.Length, b.Length)
	})
	return objects, nil
}

func validateEntryPath(value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		path.IsAbs(value) || path.Clean(value) != value || value == "." ||
		value == ".." || strings.HasPrefix(value, "../") ||
		strings.ContainsRune(value, '\\') || isPlatformUnsafeEntryPath(value) {
		return fmt.Errorf("%w: entry path is not a canonical relative path", ErrInvalid)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: entry path contains a control character", ErrInvalid)
		}
	}
	return nil
}

func isPlatformUnsafeEntryPath(value string) bool {
	if strings.ContainsRune(value, ':') {
		return true
	}
	for component := range strings.SplitSeq(value, "/") {
		if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return true
		}
		base, _, _ := strings.Cut(component, ".")
		upper := strings.ToUpper(base)
		switch upper {
		case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$":
			return true
		}
		if len(upper) == 4 && upper[3] >= '1' && upper[3] <= '9' &&
			(upper[:3] == "COM" || upper[:3] == "LPT") {
			return true
		}
	}
	return false
}

// validateProvider fails closed for providers the server cannot classify and
// for providers whose raw source trees must never leave the device.
func validateProvider(provider parser.AgentType) error {
	if err := validateOpaqueID("provider", string(provider)); err != nil {
		return err
	}
	def, ok := parser.AgentByType(provider)
	if !ok {
		return fmt.Errorf("%w: unknown provider %q", ErrInvalid, provider)
	}
	if def.RemoteSyncExcluded {
		return fmt.Errorf("%w: provider %q is excluded from remote sync", ErrInvalid, provider)
	}
	return nil
}

func cloneEntries(source []Entry) []Entry {
	if len(source) == 0 {
		return nil
	}
	cloned := make([]Entry, len(source))
	for i, entry := range source {
		cloned[i] = entry
		cloned[i].Objects = append([]ObjectRef(nil), entry.Objects...)
	}
	return cloned
}

func validateOpaqueID(name, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s identifier is required", ErrInvalid, name)
	}
	if len(value) > maxOpaqueIDBytes {
		return fmt.Errorf("%w: %s identifier exceeds %d bytes", ErrInvalid, name, maxOpaqueIDBytes)
	}
	if !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s identifier is not canonical UTF-8", ErrInvalid, name)
	}
	if strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%w: %s identifier contains a path separator", ErrInvalid, name)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s identifier contains a control character", ErrInvalid, name)
		}
	}
	return nil
}

func isCanonicalSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
