package rawsync

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/parser"
)

// Service coordinates physical custody with durable metadata acceptance.
type Service struct {
	objects           ObjectStore
	metadata          MetadataStore
	limits            ManifestLimits
	processingVersion string
}

// NewService constructs the sole raw-custody boundary used by upload APIs.
func NewService(
	objects ObjectStore,
	metadata MetadataStore,
	limits ManifestLimits,
	processingVersion string,
) (*Service, error) {
	if isNilServiceDependency(objects) {
		return nil, fmt.Errorf("%w: object store is required", ErrInvalid)
	}
	if isNilServiceDependency(metadata) {
		return nil, fmt.Errorf("%w: metadata store is required", ErrInvalid)
	}
	if err := validateManifestLimits(limits); err != nil {
		return nil, err
	}
	if err := validateServiceProcessingVersion(processingVersion); err != nil {
		return nil, err
	}
	return &Service{
		objects:           objects,
		metadata:          metadata,
		limits:            limits,
		processingVersion: processingVersion,
	}, nil
}

// FinalizeObject verifies immutable physical custody before metadata registration.
// The provider is the upload's declared source and gates custody so excluded
// providers never persist bytes; objects remain content-addressed per tenant.
func (s *Service) FinalizeObject(
	ctx context.Context,
	identity AuthIdentity,
	provider parser.AgentType,
	object ObjectRef,
	body io.Reader,
) (PutResult, error) {
	if err := validateServiceIdentity(identity); err != nil {
		return PutResult{}, err
	}
	if err := validateProvider(provider); err != nil {
		return PutResult{}, err
	}
	if err := s.validateServiceObject(object); err != nil {
		return PutResult{}, err
	}
	if body == nil {
		return PutResult{}, fmt.Errorf("%w: raw object body is required", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return PutResult{}, err
	}
	result, err := s.objects.PutObject(ctx, identity.TenantID, object, body)
	if err != nil {
		return PutResult{}, fmt.Errorf("finalizing raw object custody: %w", err)
	}
	if result.Info.Ref != object {
		return PutResult{}, fmt.Errorf("raw object custody returned a different identity: %w", ErrConflict)
	}
	if err := ctx.Err(); err != nil {
		return PutResult{}, err
	}
	if err := s.metadata.RecordVerifiedObject(ctx, identity, object); err != nil {
		return PutResult{}, fmt.Errorf("registering verified raw object: %w", err)
	}
	return result, nil
}

// MissingObjects checks physical custody, which is authoritative for upload resumption.
func (s *Service) MissingObjects(
	ctx context.Context,
	identity AuthIdentity,
	provider parser.AgentType,
	objects []ObjectRef,
) ([]ObjectRef, error) {
	if err := validateServiceIdentity(identity); err != nil {
		return nil, err
	}
	if err := validateProvider(provider); err != nil {
		return nil, err
	}
	for _, object := range objects {
		if err := s.validateServiceObject(object); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	missing, err := s.objects.MissingObjects(ctx, identity.TenantID, objects)
	if err != nil {
		return nil, fmt.Errorf("checking raw object custody: %w", err)
	}
	return missing, nil
}

// CommitManifest verifies custody, stores the canonical envelope, then accepts metadata.
func (s *Service) CommitManifest(
	ctx context.Context,
	identity AuthIdentity,
	manifest Manifest,
) (CommitResult, error) {
	canonical, err := ValidateAndCanonicalize(identity, manifest, s.limits)
	if err != nil {
		return CommitResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}
	if err := s.objects.VerifyObjects(ctx, identity.TenantID, canonical.Objects); err != nil {
		return CommitResult{}, fmt.Errorf("verifying manifest object custody: %w", err)
	}
	if err := s.metadata.RecordVerifiedObjects(ctx, identity, canonical.Objects); err != nil {
		return CommitResult{}, fmt.Errorf("registering manifest object custody: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}
	manifestResult, err := s.objects.PutManifest(ctx, canonical)
	if err != nil {
		return CommitResult{}, fmt.Errorf("finalizing canonical manifest custody: %w", err)
	}
	expectedManifestRef := ObjectRef{
		SHA256: canonical.ManifestID,
		Length: int64(len(canonical.CanonicalJSON)),
	}
	if manifestResult.Info.Ref != expectedManifestRef {
		return CommitResult{}, fmt.Errorf(
			"canonical manifest custody returned a different identity: %w", ErrConflict,
		)
	}
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}
	result, err := s.metadata.CommitManifest(ctx, canonical, s.processingVersion)
	if err != nil {
		return CommitResult{}, fmt.Errorf("accepting canonical raw manifest: %w", err)
	}
	return result, nil
}

func validateServiceIdentity(identity AuthIdentity) error {
	canonical, err := NewAuthIdentity(identity.TenantID, identity.DeviceID)
	if err != nil || canonical != identity {
		return fmt.Errorf("%w: authenticated identity is not canonical", ErrInvalid)
	}
	return nil
}

// validateServiceObject rejects noncanonical references and objects no valid
// manifest could reference, so oversized uploads never become orphans.
func (s *Service) validateServiceObject(object ObjectRef) error {
	canonical, err := NewObjectRef(object.SHA256, object.Length)
	if err != nil || canonical != object {
		return fmt.Errorf("%w: raw object reference is not canonical", ErrInvalid)
	}
	if object.Length > s.limits.MaxFileBytes {
		return fmt.Errorf(
			"%w: raw object exceeds the %d byte file limit", ErrInvalid, s.limits.MaxFileBytes,
		)
	}
	return nil
}

func validateServiceProcessingVersion(value string) error {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: processing version is not canonical", ErrInvalid)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: processing version contains a control character", ErrInvalid)
		}
	}
	return nil
}

func isNilServiceDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
