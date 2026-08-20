package artifact

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type artifactCheckpointSequenceDB interface {
	GetArtifactCheckpointFloor(context.Context, string) (int, bool, error)
	ReserveArtifactCheckpointSequence(context.Context, string, int) (int, error)
}

type checkpointFloorStore interface {
	checkpointFloor(context.Context, string) (int, error)
}

func openStoreEntryIterator(
	ctx context.Context, store ArtifactStore, origin string, kind Kind,
) (EntryIterator, error) {
	return store.Entries(ctx, origin, kind)
}

// statRecordedCheckpoint trusts the store's catalog identity, which is
// established by verified immutable creation and checked again on normal
// reads. Periodic unchanged export must remain constant work; full physical
// verification belongs bootstrap and maintenance.
func statRecordedCheckpoint(
	ctx context.Context,
	store ArtifactStore,
	head db.ArtifactCheckpointHead,
) (bool, error) {
	ref, err := NewRef(head.Origin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", head.Sequence))
	if err != nil {
		return false, err
	}
	entry, err := store.Stat(ctx, ref)
	if errors.Is(err, ErrArtifactNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stating recorded artifact checkpoint: %w", err)
	}
	if entry.Identity.SHA256 != head.CheckpointSHA256 || entry.Identity.Size != head.CheckpointSize {
		quarantineErr := store.Quarantine(ctx, ref, "recorded checkpoint identity mismatch")
		return false, quarantineErr
	}
	return true, nil
}

func latestValidCheckpointHead(
	ctx context.Context,
	store ArtifactStore,
	origin string,
) (_ db.ArtifactCheckpointHead, _ bool, retErr error) {
	var head db.ArtifactCheckpointHead
	iterator, err := openStoreEntryIterator(ctx, store, origin, KindCheckpoints)
	if err != nil {
		return db.ArtifactCheckpointHead{}, false, fmt.Errorf("listing artifact checkpoints: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
	for {
		entries, nextErr := iterator.Next(ctx, checkpointFloorPageSize)
		if nextErr != nil && !errors.Is(nextErr, io.EOF) {
			return db.ArtifactCheckpointHead{}, false, fmt.Errorf("listing artifact checkpoints: %w", nextErr)
		}
		for _, entry := range entries {
			sequence, err := checkpointSequence(entry.Ref.Name)
			if err != nil || sequence <= head.Sequence {
				continue
			}
			candidate, valid, err := decodeCheckpointCandidate(
				ctx, store, origin, entry,
			)
			if err != nil {
				return db.ArtifactCheckpointHead{}, false, err
			}
			if valid {
				head = candidate
			}
		}
		if errors.Is(nextErr, io.EOF) {
			break
		}
	}
	return head, head.Sequence > 0, nil
}

func decodeCheckpointCandidate(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	entry Entry,
) (db.ArtifactCheckpointHead, bool, error) {
	if entry.Identity.Size > checkpointDecodedLimit {
		return db.ArtifactCheckpointHead{}, false, nil
	}
	_, reader, err := store.Open(ctx, entry.Ref)
	if errors.Is(err, ErrArtifactNotFound) || errors.Is(err, ErrArtifactCorrupt) {
		return db.ArtifactCheckpointHead{}, false, nil
	}
	if err != nil {
		return db.ArtifactCheckpointHead{}, false,
			fmt.Errorf("opening artifact checkpoint: %w", err)
	}
	candidate, decodeErr := decodeCanonicalCheckpointHead(
		reader, origin, entry.Ref.Name, entry.Identity,
	)
	// Verify drains any bytes left unread after an early semantic decode
	// failure before authenticating the complete stream.
	verifyErr := reader.Verify()
	closeErr := reader.Close()
	if closeErr != nil && !errors.Is(closeErr, ErrArtifactCorrupt) {
		return db.ArtifactCheckpointHead{}, false,
			fmt.Errorf("closing artifact checkpoint: %w", closeErr)
	}
	if verifyErr != nil && !errors.Is(verifyErr, ErrArtifactCorrupt) {
		return db.ArtifactCheckpointHead{}, false,
			fmt.Errorf("verifying artifact checkpoint: %w", verifyErr)
	}
	if verifyErr != nil || closeErr != nil {
		return db.ArtifactCheckpointHead{}, false, nil
	}
	if errors.Is(decodeErr, errFutureArtifactVersion) {
		return db.ArtifactCheckpointHead{}, false, decodeErr
	}
	if decodeErr != nil {
		return db.ArtifactCheckpointHead{}, false, nil
	}
	return candidate, true, nil
}

func validateRecordedCheckpointFormat(
	ctx context.Context,
	store ArtifactStore,
	head db.ArtifactCheckpointHead,
) error {
	if head.CheckpointSize > checkpointDecodedLimit {
		return fmt.Errorf(
			"%w: recorded checkpoint %d exceeds the decode limit",
			ErrArtifactUnsupported, head.Sequence,
		)
	}
	ref, err := NewRef(head.Origin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", head.Sequence))
	if err != nil {
		return err
	}
	_, _, err = decodeCheckpointCandidate(ctx, store, head.Origin, Entry{
		Ref: ref,
		Identity: Identity{
			SHA256: head.CheckpointSHA256,
			Size:   head.CheckpointSize,
		},
	})
	return err
}

func decodeCanonicalCheckpointHead(
	reader io.Reader,
	origin string,
	name string,
	identity Identity,
) (db.ArtifactCheckpointHead, error) {
	body, err := io.ReadAll(io.LimitReader(reader, checkpointDecodedLimit+1))
	if err != nil {
		return db.ArtifactCheckpointHead{}, err
	}
	if int64(len(body)) > checkpointDecodedLimit {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint exceeds the decode limit")
	}

	var metadata struct {
		Version  int    `json:"v"`
		Origin   string `json:"origin"`
		Sequence int    `json:"seq"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return db.ArtifactCheckpointHead{}, err
	}
	if metadata.Origin == "" {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint origin is missing")
	}
	if metadata.Origin != origin {
		return db.ArtifactCheckpointHead{}, fmt.Errorf(
			"checkpoint origin mismatch for %s: got %q", origin, metadata.Origin,
		)
	}
	if metadata.Sequence < 1 {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint sequence is invalid")
	}
	if metadata.Version < 1 {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint version is unsupported")
	}
	if fmt.Sprintf("cp-%010d.json", metadata.Sequence) != name {
		return db.ArtifactCheckpointHead{}, fmt.Errorf(
			"checkpoint sequence identity mismatch: got %s", name,
		)
	}

	canonical := jsontext.Value(body).Clone()
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return db.ArtifactCheckpointHead{}, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(body, canonical) {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint is not canonically encoded")
	}
	if hashHex(body) != identity.SHA256 || int64(len(body)) != identity.Size {
		return db.ArtifactCheckpointHead{}, errors.New(
			"checkpoint stored identity differs from canonical encoding",
		)
	}
	if metadata.Version > checkpointFormatVersion {
		return db.ArtifactCheckpointHead{}, fmt.Errorf(
			"%w: checkpoint version %d", errFutureArtifactVersion, metadata.Version,
		)
	}

	var current checkpoint
	if err := json.Unmarshal(body, &current, json.RejectUnknownMembers(true)); err != nil {
		return db.ArtifactCheckpointHead{}, err
	}
	if current.Sessions == nil {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint sessions is missing")
	}
	for gid, manifestHash := range current.Sessions {
		if gid == "" || !strings.HasPrefix(gid, origin+"~") {
			return db.ArtifactCheckpointHead{}, errors.New("checkpoint session identity is invalid")
		}
		if err := validateHashHex(manifestHash); err != nil {
			return db.ArtifactCheckpointHead{}, fmt.Errorf(
				"checkpoint manifest hash is invalid: %w", err,
			)
		}
	}
	mapJSON, err := canonicalJSON(current.Sessions)
	if err != nil {
		return db.ArtifactCheckpointHead{}, err
	}
	return db.ArtifactCheckpointHead{
		Origin: origin, Sequence: current.Sequence,
		SessionMapSHA256: hashHex(mapJSON), CheckpointSHA256: identity.SHA256,
		CheckpointSize: identity.Size,
	}, nil
}

func reserveCheckpointSequenceFromStore(
	ctx context.Context,
	database artifactCheckpointSequenceDB,
	store ArtifactStore,
	origin string,
) (_ int, retErr error) {
	_, bootstrapped, err := database.GetArtifactCheckpointFloor(ctx, origin)
	if err != nil {
		return 0, fmt.Errorf("reading checkpoint floor for %s: %w", origin, err)
	}
	if bootstrapped {
		sequence, err := database.ReserveArtifactCheckpointSequence(ctx, origin, 0)
		if err != nil {
			return 0, fmt.Errorf("reserving checkpoint sequence for %s: %w", origin, err)
		}
		return sequence, nil
	}
	observedFloor := 0
	if observer, ok := store.(checkpointFloorStore); ok {
		floor, err := observer.checkpointFloor(ctx, origin)
		if err != nil {
			return 0, fmt.Errorf("listing checkpoint floor for %s: %w", origin, err)
		}
		observedFloor = floor
	} else {
		iterator, err := openStoreEntryIterator(ctx, store, origin, KindCheckpoints)
		if err != nil {
			return 0, fmt.Errorf("listing checkpoint floor for %s: %w", origin, err)
		}
		defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
		for {
			entries, nextErr := iterator.Next(ctx, checkpointFloorPageSize)
			if nextErr != nil && !errors.Is(nextErr, io.EOF) {
				return 0, fmt.Errorf("listing checkpoint floor for %s: %w", origin, nextErr)
			}
			for _, entry := range entries {
				sequence, err := checkpointSequence(entry.Ref.Name)
				if err != nil {
					continue
				}
				observedFloor = max(observedFloor, sequence)
			}
			if errors.Is(nextErr, io.EOF) {
				break
			}
		}
	}
	sequence, err := database.ReserveArtifactCheckpointSequence(ctx, origin, observedFloor)
	if err != nil {
		return 0, fmt.Errorf("reserving checkpoint sequence for %s: %w", origin, err)
	}
	return sequence, nil
}

func normalizeManifestSessionLocalState(sess *manifestSession) {
	// Keep non-content, machine-local state out of the canonical manifest so a
	// source-only change to it does not alter the content hash and trigger a
	// re-import that clears the importer's local findings. secret_leak_count is
	// import-discarded secret state (see rewriteForImport); local_modified_at is
	// the local sync watermark, which import ignores (the importer stamps its
	// own) -- and a secret rescan bumps both even when no exported message
	// content changed. The file_* fields are source-file bookkeeping that
	// import clears (see clearImportedSessionSourceState); a touch, move, or
	// re-download of the source file changes them without changing any
	// exported content.
	sess.SecretLeakCount = 0
	sess.LocalModifiedAt = nil
	sess.FilePath = nil
	sess.FileSize = nil
	sess.FileMtime = nil
	sess.FileInode = nil
	sess.FileDevice = nil
	sess.FileHash = nil
}
