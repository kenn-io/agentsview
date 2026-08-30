// Package rawupload drains the durable laptop outbox through the raw-ingest
// client and advances checkpoints only after a server receipt is durable.
package rawupload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawsync"
)

// missingObjectBatchSize keeps negotiation requests comfortably below the
// one-MiB control-plane limit even at maximum canonical object-ref size.
const missingObjectBatchSize = 2048

const (
	transientRetryDelay = time.Minute
	maximumRetryDelay   = time.Hour
)

// ErrPermanentFailure marks an upload rejection that was durably blocked.
var ErrPermanentFailure = errors.New("rawupload: permanent upload failure")

// Transport is the rawclient surface required by the outbox uploader.
type Transport interface {
	MissingObjects(
		context.Context, parser.AgentType, []rawsync.ObjectRef,
	) ([]rawsync.ObjectRef, error)
	UploadObject(
		context.Context, parser.AgentType, rawsync.ObjectRef, io.ReaderAt,
	) error
	CommitManifest(context.Context, rawsync.Manifest) (rawsync.CommitResult, error)
}

// Result summarizes one acknowledged generation without retaining source
// content or credentials.
type Result struct {
	CaptureID     string
	ManifestID    string
	Receipt       string
	Generation    int64
	Uploaded      int
	UploadedBytes int64
}

// Uploader drains generations from one durable checkpoint store.
type Uploader struct {
	store     *rawcheckpoint.Store
	transport Transport
	deviceID  string
	now       func() time.Time
}

// New constructs an uploader for one already-configured device.
func New(
	store *rawcheckpoint.Store,
	transport Transport,
	deviceID string,
) *Uploader {
	return &Uploader{
		store: store, transport: transport, deviceID: deviceID, now: time.Now,
	}
}

// UploadNext uploads and acknowledges the oldest ready generation. The bool
// is false when no generation is currently ready.
func (u *Uploader) UploadNext(
	ctx context.Context,
) (Result, bool, error) {
	manifest, found, err := u.store.FinalizeNextManifest(ctx, u.deviceID)
	if err != nil || !found {
		return Result{}, false, err
	}
	result := Result{CaptureID: manifest.CaptureID}
	commit, committed, err := u.store.FinalizedCommit(ctx, u.deviceID, manifest.CaptureID)
	if err != nil {
		return Result{}, true, u.recordFailure(ctx, manifest.CaptureID, err)
	}
	if !committed {
		objects := manifestObjects(manifest)
		missing, err := missingObjects(ctx, u.transport, manifest.Provider, objects)
		if err != nil {
			return Result{}, true, u.recordFailure(ctx, manifest.CaptureID, err)
		}
		for _, object := range missing {
			file, err := os.Open(u.store.ObjectPath(object))
			if err != nil {
				cause := fmt.Errorf("rawupload: open queued object: %w", err)
				return Result{}, true, u.recordFailure(ctx, manifest.CaptureID, cause)
			}
			uploadErr := u.transport.UploadObject(ctx, manifest.Provider, object, file)
			closeErr := file.Close()
			if uploadErr != nil {
				return Result{}, true, u.recordFailure(ctx, manifest.CaptureID, uploadErr)
			}
			if closeErr != nil {
				cause := fmt.Errorf("rawupload: close queued object: %w", closeErr)
				return Result{}, true, u.recordFailure(ctx, manifest.CaptureID, cause)
			}
			result.Uploaded++
			result.UploadedBytes += object.Length
		}
		commit, err = u.transport.CommitManifest(ctx, manifest)
		if err != nil {
			return Result{}, true, u.recordFailure(ctx, manifest.CaptureID, err)
		}
		if err := u.store.BindFinalizedCommit(
			ctx, u.deviceID, manifest.CaptureID, commit,
		); err != nil {
			return Result{}, true, u.recordFailure(ctx, manifest.CaptureID, err)
		}
	}
	if _, err := u.store.AcknowledgeGeneration(
		ctx, u.deviceID, manifest.CaptureID, commit,
	); err != nil {
		return Result{}, true, u.recordFailure(ctx, manifest.CaptureID, err)
	}
	result.ManifestID = commit.ManifestID
	result.Receipt = commit.Receipt
	result.Generation = commit.Generation
	return result, true, nil
}

func (u *Uploader) recordFailure(
	ctx context.Context,
	captureID string,
	cause error,
) error {
	class := rawcheckpoint.GenerationFailureTransient
	retryAt := time.Time{}
	var apiError rawclient.APIError
	if rawclient.AsAPIError(cause, &apiError) {
		switch {
		case apiError.Code == rawclient.CodeHeadConflict:
			class = rawcheckpoint.GenerationFailureParentReceiptConflict
			retryAt = time.Time{}
		case apiError.Status == http.StatusRequestTimeout,
			apiError.Status == http.StatusTooManyRequests,
			apiError.Status >= http.StatusInternalServerError,
			apiError.Code == rawclient.CodeMissingObject:
		case apiError.Code == rawclient.CodeInvalidRequest,
			apiError.Code == rawclient.CodeChecksumMismatch:
			class = rawcheckpoint.GenerationFailurePermanent
			retryAt = time.Time{}
		}
	}
	if class == rawcheckpoint.GenerationFailureParentReceiptConflict {
		reconciled := rawcheckpoint.SourceHead{
			ManifestID: apiError.CurrentManifestID,
			Receipt:    apiError.CurrentReceipt,
			Generation: apiError.CurrentGeneration,
		}
		if err := u.store.ResumeGeneration(
			ctx, u.deviceID, captureID, reconciled,
		); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	if class == rawcheckpoint.GenerationFailureTransient {
		attempts, err := u.store.GenerationAttempts(ctx, u.deviceID, captureID)
		if err != nil {
			return errors.Join(cause, err)
		}
		retryAt = u.now().UTC().Add(transientBackoff(attempts))
	}
	if class == rawcheckpoint.GenerationFailurePermanent {
		if err := u.store.RecordGenerationFailure(
			ctx, u.deviceID, captureID, class, time.Time{},
		); err != nil {
			return errors.Join(cause, err)
		}
		return errors.Join(ErrPermanentFailure, cause)
	}
	if err := u.store.RecordGenerationFailure(
		ctx, u.deviceID, captureID, class, retryAt,
	); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func transientBackoff(previousAttempts int) time.Duration {
	delay := transientRetryDelay
	for range max(previousAttempts, 0) {
		if delay >= maximumRetryDelay/2 {
			return maximumRetryDelay
		}
		delay *= 2
	}
	return delay
}

func manifestObjects(manifest rawsync.Manifest) []rawsync.ObjectRef {
	seen := make(map[rawsync.ObjectRef]struct{})
	var objects []rawsync.ObjectRef
	for _, entry := range manifest.Entries {
		for _, object := range entry.Objects {
			if _, exists := seen[object]; exists {
				continue
			}
			seen[object] = struct{}{}
			objects = append(objects, object)
		}
	}
	return objects
}

func missingObjects(
	ctx context.Context,
	transport Transport,
	provider parser.AgentType,
	objects []rawsync.ObjectRef,
) ([]rawsync.ObjectRef, error) {
	var missing []rawsync.ObjectRef
	for start := 0; start < len(objects); start += missingObjectBatchSize {
		end := min(start+missingObjectBatchSize, len(objects))
		batch, err := transport.MissingObjects(ctx, provider, objects[start:end])
		if err != nil {
			return nil, err
		}
		missing = append(missing, batch...)
	}
	return missing, nil
}
