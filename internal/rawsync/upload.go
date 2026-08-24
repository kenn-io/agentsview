package rawsync

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

const (
	DefaultUploadChunkBytes = int64(4 << 20)
	DefaultUploadSessionTTL = 24 * time.Hour

	uploadIDPrefix      = "upl_"
	uploadIDRandomSize  = 16
	uploadIDEncodedSize = 22
	maxUploadSessionTTL = 7 * 24 * time.Hour
	uploadFinalizeLocks = 64
)

// UploadOffsetConflictError reports the authoritative resumable offset.
type UploadOffsetConflictError struct {
	CurrentOffset int64
}

func (e *UploadOffsetConflictError) Error() string {
	return "raw upload offset changed"
}

func (e *UploadOffsetConflictError) Unwrap() error { return ErrConflict }

// UploadChecksumMismatchError reports that staged bytes were discarded.
type UploadChecksumMismatchError struct {
	CurrentOffset int64
}

func (e *UploadChecksumMismatchError) Error() string {
	return "raw upload checksum did not match"
}

func (e *UploadChecksumMismatchError) Unwrap() error { return ErrConflict }

// UploadSession is one durable, device-bound object transfer.
type UploadSession struct {
	ID         string
	Identity   AuthIdentity
	Provider   parser.AgentType
	Object     ObjectRef
	Offset     int64
	Generation int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Complete   bool
}

// UploadSessionStore owns durable session metadata and restart-safe staged bytes.
type UploadSessionStore interface {
	Create(context.Context, UploadSession) (UploadSession, bool, error)
	Status(context.Context, AuthIdentity, string, time.Time) (UploadSession, error)
	Append(
		context.Context, AuthIdentity, string, int64, []byte, time.Time,
	) (UploadSession, error)
	Open(
		context.Context, AuthIdentity, string, time.Time,
	) (UploadSession, io.ReadCloser, error)
	Reset(
		context.Context, AuthIdentity, string, int64, time.Time,
	) (UploadSession, error)
	Complete(context.Context, AuthIdentity, string, time.Time) (UploadSession, error)
}

// UploadCustody is the existing verified raw-object boundary used at finalization.
type UploadCustody interface {
	MissingObjects(
		context.Context, AuthIdentity, parser.AgentType, []ObjectRef,
	) ([]ObjectRef, error)
	FinalizeObject(
		context.Context, AuthIdentity, parser.AgentType, ObjectRef, io.Reader,
	) (PutResult, error)
}

// UploadService coordinates exact-offset staging with independently verified custody.
type UploadService struct {
	store   UploadSessionStore
	custody UploadCustody
	ttl     time.Duration
	random  io.Reader
	now     func() time.Time
	locks   [uploadFinalizeLocks]chan struct{}
}

// NewUploadService constructs the resumable raw-object transfer boundary.
func NewUploadService(
	store UploadSessionStore,
	custody UploadCustody,
	ttl time.Duration,
) (*UploadService, error) {
	if isNilServiceDependency(store) {
		return nil, fmt.Errorf("%w: upload session store is required", ErrInvalid)
	}
	if isNilServiceDependency(custody) {
		return nil, fmt.Errorf("%w: upload custody service is required", ErrInvalid)
	}
	if ttl <= 0 || ttl > maxUploadSessionTTL {
		return nil, fmt.Errorf(
			"%w: upload session lifetime must be greater than zero and at most %s",
			ErrInvalid, maxUploadSessionTTL,
		)
	}
	service := &UploadService{
		store: store, custody: custody, ttl: ttl, random: rand.Reader, now: time.Now,
	}
	for index := range service.locks {
		service.locks[index] = make(chan struct{}, 1)
		service.locks[index] <- struct{}{}
	}
	return service, nil
}

// Start creates or resumes one upload, or reports already-verified custody.
func (s *UploadService) Start(
	ctx context.Context,
	identity AuthIdentity,
	provider parser.AgentType,
	object ObjectRef,
) (UploadSession, bool, error) {
	if err := validateServiceIdentity(identity); err != nil {
		return UploadSession{}, false, err
	}
	if err := validateProvider(provider); err != nil {
		return UploadSession{}, false, err
	}
	canonical, err := NewObjectRef(object.SHA256, object.Length)
	if err != nil || canonical != object {
		return UploadSession{}, false, fmt.Errorf("%w: invalid upload object", ErrInvalid)
	}
	missing, err := s.custody.MissingObjects(ctx, identity, provider, []ObjectRef{object})
	if err != nil {
		return UploadSession{}, false, fmt.Errorf("checking upload object custody: %w", err)
	}
	if len(missing) == 0 {
		return UploadSession{
			Identity: identity, Provider: provider, Object: object,
			Offset: object.Length, Complete: true,
		}, false, nil
	}
	if len(missing) != 1 || missing[0] != object {
		return UploadSession{}, false, fmt.Errorf(
			"upload custody returned a different missing object: %w", ErrConflict,
		)
	}
	uploadID, err := s.newUploadID()
	if err != nil {
		return UploadSession{}, false, fmt.Errorf("generating raw upload identity: %w", err)
	}
	now := s.now().UTC()
	record := UploadSession{
		ID: uploadID, Identity: identity, Provider: provider, Object: object,
		CreatedAt: now, ExpiresAt: now.Add(s.ttl),
	}
	session, created, err := s.store.Create(ctx, record)
	if err != nil {
		return UploadSession{}, false, fmt.Errorf("creating raw upload session: %w", err)
	}
	if err := validateStoredUploadSession(session, identity, session.ID); err != nil ||
		session.Provider != provider || session.Object != object || session.Complete {
		return UploadSession{}, false, fmt.Errorf(
			"upload session store returned a different transfer: %w", ErrConflict,
		)
	}
	return session, created, nil
}

// Status returns the authoritative offset for one device-bound upload.
func (s *UploadService) Status(
	ctx context.Context,
	identity AuthIdentity,
	uploadID string,
) (UploadSession, error) {
	if err := validateServiceIdentity(identity); err != nil {
		return UploadSession{}, err
	}
	if err := validateUploadID(uploadID); err != nil {
		return UploadSession{}, err
	}
	session, err := s.store.Status(ctx, identity, uploadID, s.now().UTC())
	if err != nil {
		return UploadSession{}, fmt.Errorf("loading raw upload session: %w", err)
	}
	if err := validateStoredUploadSession(session, identity, uploadID); err != nil {
		return UploadSession{}, fmt.Errorf("validating raw upload session: %w", err)
	}
	return session, nil
}

// Append writes one exact-offset chunk and finalizes full objects idempotently.
func (s *UploadService) Append(
	ctx context.Context,
	identity AuthIdentity,
	uploadID string,
	expectedOffset int64,
	chunk []byte,
) (UploadSession, error) {
	if err := validateServiceIdentity(identity); err != nil {
		return UploadSession{}, err
	}
	if err := validateUploadID(uploadID); err != nil {
		return UploadSession{}, err
	}
	if expectedOffset < 0 || int64(len(chunk)) > DefaultUploadChunkBytes {
		return UploadSession{}, fmt.Errorf("%w: invalid raw upload chunk", ErrInvalid)
	}
	now := s.now().UTC()
	session, err := s.store.Append(ctx, identity, uploadID, expectedOffset, chunk, now)
	if err != nil {
		return UploadSession{}, fmt.Errorf("appending raw upload chunk: %w", err)
	}
	if err := validateStoredUploadSession(session, identity, uploadID); err != nil {
		return UploadSession{}, fmt.Errorf("validating appended raw upload session: %w", err)
	}
	if session.Complete {
		if session.Offset != expectedOffset {
			return UploadSession{}, &UploadOffsetConflictError{
				CurrentOffset: session.Offset,
			}
		}
		return session, nil
	}
	expectedAfter := expectedOffset + int64(len(chunk))
	if session.Offset != expectedAfter {
		return UploadSession{}, fmt.Errorf(
			"upload session store advanced to a different offset: %w", ErrConflict,
		)
	}
	if session.Offset < session.Object.Length {
		if len(chunk) == 0 {
			return UploadSession{}, fmt.Errorf("%w: empty upload chunk made no progress", ErrInvalid)
		}
		return session, nil
	}
	return s.finalizeSerialized(ctx, identity, uploadID, session, now)
}

func (s *UploadService) finalizeSerialized(
	ctx context.Context,
	identity AuthIdentity,
	uploadID string,
	session UploadSession,
	now time.Time,
) (UploadSession, error) {
	digest := sha256.Sum256([]byte(uploadID))
	lock := s.locks[int(digest[0])%len(s.locks)]
	select {
	case <-ctx.Done():
		return UploadSession{}, ctx.Err()
	case <-lock:
	}
	defer func() { lock <- struct{}{} }()

	current, err := s.store.Status(ctx, identity, uploadID, now)
	if err != nil {
		return UploadSession{}, fmt.Errorf("rechecking raw upload before finalization: %w", err)
	}
	if err := validateStoredUploadSession(current, identity, uploadID); err != nil ||
		current.Provider != session.Provider || current.Object != session.Object {
		return UploadSession{}, fmt.Errorf("raw upload changed before finalization: %w", ErrConflict)
	}
	if current.Complete {
		return current, nil
	}
	if current.Offset != current.Object.Length || current.Offset != session.Offset {
		return UploadSession{}, fmt.Errorf("raw upload offset changed before finalization: %w", ErrConflict)
	}
	return s.finalize(ctx, identity, uploadID, current, now)
}

func (s *UploadService) finalize(
	ctx context.Context,
	identity AuthIdentity,
	uploadID string,
	session UploadSession,
	now time.Time,
) (UploadSession, error) {
	opened, reader, err := s.store.Open(ctx, identity, uploadID, now)
	if err != nil {
		if completed, ok := s.completedAfterFinalizeRace(
			ctx, identity, uploadID, session, now,
		); ok {
			return completed, nil
		}
		return UploadSession{}, fmt.Errorf("opening staged raw upload: %w", err)
	}
	if err := validateStoredUploadSession(opened, identity, uploadID); err != nil ||
		opened.Object != session.Object || opened.Offset != session.Offset {
		_ = reader.Close()
		return UploadSession{}, fmt.Errorf("staged raw upload changed: %w", ErrConflict)
	}
	hash := sha256.New()
	readBytes, readErr := io.Copy(hash, &uploadContextReader{ctx: ctx, reader: reader})
	readErr = errors.Join(readErr, reader.Close())
	if readErr != nil {
		return UploadSession{}, fmt.Errorf("hashing staged raw upload: %w", readErr)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if readBytes != session.Object.Length || digest != session.Object.SHA256 {
		reset, resetErr := s.store.Reset(
			ctx, identity, uploadID, opened.Generation, now,
		)
		if resetErr != nil {
			return UploadSession{}, fmt.Errorf("resetting mismatched raw upload: %w", resetErr)
		}
		mismatch := &UploadChecksumMismatchError{CurrentOffset: 0}
		if err := validateStoredUploadSession(reset, identity, uploadID); err != nil ||
			reset.Offset != 0 || reset.Complete {
			return UploadSession{}, errors.Join(
				mismatch, fmt.Errorf("upload store returned an invalid reset: %w", ErrConflict),
			)
		}
		return reset, mismatch
	}

	opened, reader, err = s.store.Open(ctx, identity, uploadID, now)
	if err != nil {
		if completed, ok := s.completedAfterFinalizeRace(
			ctx, identity, uploadID, session, now,
		); ok {
			return completed, nil
		}
		return UploadSession{}, fmt.Errorf("reopening verified raw upload: %w", err)
	}
	if err := validateStoredUploadSession(opened, identity, uploadID); err != nil ||
		opened.Object != session.Object || opened.Offset != session.Offset {
		_ = reader.Close()
		return UploadSession{}, fmt.Errorf("verified raw upload changed: %w", ErrConflict)
	}
	result, finalizeErr := s.custody.FinalizeObject(
		ctx, identity, session.Provider, session.Object,
		&uploadContextReader{ctx: ctx, reader: reader},
	)
	finalizeErr = errors.Join(finalizeErr, reader.Close())
	if finalizeErr != nil {
		return UploadSession{}, fmt.Errorf("finalizing staged raw upload: %w", finalizeErr)
	}
	if result.Info.Ref != session.Object {
		return UploadSession{}, fmt.Errorf("raw upload custody changed identity: %w", ErrConflict)
	}
	completed, err := s.store.Complete(ctx, identity, uploadID, now)
	if err != nil {
		return UploadSession{}, fmt.Errorf("completing raw upload session: %w", err)
	}
	if err := validateStoredUploadSession(completed, identity, uploadID); err != nil ||
		!completed.Complete || completed.Offset != completed.Object.Length {
		return UploadSession{}, fmt.Errorf("upload store returned invalid completion: %w", ErrConflict)
	}
	return completed, nil
}

func (s *UploadService) completedAfterFinalizeRace(
	ctx context.Context,
	identity AuthIdentity,
	uploadID string,
	expected UploadSession,
	now time.Time,
) (UploadSession, bool) {
	current, err := s.store.Status(ctx, identity, uploadID, now)
	if err != nil || validateStoredUploadSession(current, identity, uploadID) != nil ||
		current.Provider != expected.Provider || current.Object != expected.Object ||
		!current.Complete {
		return UploadSession{}, false
	}
	return current, true
}

type uploadContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *uploadContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func (s *UploadService) newUploadID() (string, error) {
	buffer := make([]byte, uploadIDRandomSize)
	if _, err := io.ReadFull(s.random, buffer); err != nil {
		return "", err
	}
	return uploadIDPrefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func validateUploadID(value string) error {
	encoded, ok := strings.CutPrefix(value, uploadIDPrefix)
	if !ok || len(encoded) != uploadIDEncodedSize {
		return fmt.Errorf("%w: invalid raw upload identity", ErrInvalid)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != uploadIDRandomSize ||
		base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return fmt.Errorf("%w: invalid raw upload identity", ErrInvalid)
	}
	return nil
}

// ValidateUploadSession rejects noncanonical durable session records.
func ValidateUploadSession(session UploadSession) error {
	if err := validateUploadID(session.ID); err != nil {
		return err
	}
	if err := validateServiceIdentity(session.Identity); err != nil {
		return err
	}
	if err := validateProvider(session.Provider); err != nil {
		return err
	}
	canonical, err := NewObjectRef(session.Object.SHA256, session.Object.Length)
	if err != nil || canonical != session.Object ||
		session.Offset < 0 || session.Offset > session.Object.Length ||
		session.Generation < 0 ||
		session.CreatedAt.IsZero() || session.ExpiresAt.IsZero() ||
		!session.ExpiresAt.After(session.CreatedAt) ||
		(session.Complete && session.Offset != session.Object.Length) {
		return fmt.Errorf("%w: invalid raw upload session", ErrInvalid)
	}
	return nil
}

func validateStoredUploadSession(
	session UploadSession,
	identity AuthIdentity,
	uploadID string,
) error {
	if session.ID != uploadID || session.Identity != identity ||
		ValidateUploadSession(session) != nil {
		return ErrConflict
	}
	return nil
}

var _ UploadCustody = (*Service)(nil)
