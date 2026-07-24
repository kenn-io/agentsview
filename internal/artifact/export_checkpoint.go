package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
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
			if entry.Identity.Size > checkpointDecodedLimit {
				continue
			}
			_, reader, err := store.Open(ctx, entry.Ref)
			if errors.Is(err, ErrArtifactNotFound) || errors.Is(err, ErrArtifactCorrupt) {
				continue
			}
			if err != nil {
				return db.ArtifactCheckpointHead{}, false,
					fmt.Errorf("opening artifact checkpoint: %w", err)
			}
			candidate, decodeErr := decodeCanonicalCheckpointHead(
				reader, origin, entry.Ref.Name, entry.Identity,
			)
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
			if errors.Is(decodeErr, errFutureArtifactVersion) {
				return db.ArtifactCheckpointHead{}, false, decodeErr
			}
			if decodeErr != nil || verifyErr != nil || closeErr != nil {
				continue
			}
			head = candidate
		}
		if errors.Is(nextErr, io.EOF) {
			break
		}
	}
	return head, head.Sequence > 0, nil
}

func decodeCanonicalCheckpointHead(
	reader io.Reader,
	origin string,
	name string,
	identity Identity,
) (db.ArtifactCheckpointHead, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint is not a JSON object")
	}
	expectedFields := []string{"origin", "seq", "sessions", "v"}
	var sequence int
	var mapDigest string
	for _, expected := range expectedFields {
		token, err := decoder.Token()
		if err != nil {
			return db.ArtifactCheckpointHead{}, err
		}
		field, ok := token.(string)
		if !ok || field != expected {
			return db.ArtifactCheckpointHead{}, fmt.Errorf(
				"checkpoint is not canonical: expected field %q", expected,
			)
		}
		switch field {
		case "origin":
			var got string
			if err := decoder.Decode(&got); err != nil {
				return db.ArtifactCheckpointHead{}, err
			}
			if got != origin {
				return db.ArtifactCheckpointHead{}, fmt.Errorf(
					"checkpoint origin mismatch for %s: got %q", origin, got,
				)
			}
		case "seq":
			var number json.Number
			if err := decoder.Decode(&number); err != nil {
				return db.ArtifactCheckpointHead{}, err
			}
			value, err := strconv.ParseInt(number.String(), 10, 32)
			if err != nil || value < 1 {
				return db.ArtifactCheckpointHead{}, errors.New("checkpoint sequence is invalid")
			}
			sequence = int(value)
		case "sessions":
			mapDigest, err = decodeCanonicalCheckpointSessionMap(decoder, origin)
			if err != nil {
				return db.ArtifactCheckpointHead{}, err
			}
		case "v":
			var number json.Number
			if err := decoder.Decode(&number); err != nil {
				return db.ArtifactCheckpointHead{}, err
			}
			version, err := strconv.Atoi(number.String())
			if err != nil || version < 1 {
				return db.ArtifactCheckpointHead{}, errors.New("checkpoint version is unsupported")
			}
			if version > formatVersion {
				return db.ArtifactCheckpointHead{}, fmt.Errorf(
					"%w: checkpoint version %d", errFutureArtifactVersion, version,
				)
			}
			if version != formatVersion {
				return db.ArtifactCheckpointHead{}, errors.New("checkpoint version is unsupported")
			}
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint object is incomplete")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return db.ArtifactCheckpointHead{}, errors.New("checkpoint has trailing JSON")
		}
		return db.ArtifactCheckpointHead{}, err
	}
	if fmt.Sprintf("cp-%010d.json", sequence) != name {
		return db.ArtifactCheckpointHead{}, fmt.Errorf(
			"checkpoint sequence identity mismatch: got %s", name,
		)
	}
	return db.ArtifactCheckpointHead{
		Origin: origin, Sequence: sequence,
		SessionMapSHA256: mapDigest, CheckpointSHA256: identity.SHA256,
		CheckpointSize: identity.Size,
	}, nil
}

func decodeCanonicalCheckpointSessionMap(
	decoder *json.Decoder,
	origin string,
) (string, error) {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", errors.New("checkpoint sessions is not an object")
	}
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, "{")
	first := true
	previous := ""
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		gid, ok := token.(string)
		if !ok || gid == "" || !strings.HasPrefix(gid, origin+"~") {
			return "", errors.New("checkpoint session identity is invalid")
		}
		if !first && gid <= previous {
			return "", errors.New("checkpoint sessions are not in canonical order")
		}
		var manifestHash string
		if err := decoder.Decode(&manifestHash); err != nil {
			return "", err
		}
		if err := validateHashHex(manifestHash); err != nil {
			return "", fmt.Errorf("checkpoint manifest hash is invalid: %w", err)
		}
		if !first {
			_, _ = io.WriteString(hasher, ",")
		}
		gidJSON, _ := json.Marshal(gid)
		hashJSON, _ := json.Marshal(manifestHash)
		_, _ = hasher.Write(gidJSON)
		_, _ = io.WriteString(hasher, ":")
		_, _ = hasher.Write(hashJSON)
		first = false
		previous = gid
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return "", errors.New("checkpoint sessions object is incomplete")
	}
	_, _ = io.WriteString(hasher, "}\n")
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// Export temporarily preserves the root-based API while canonical publication
// migrates to ArtifactStore. The reference filesystem store is isolated from
// the legacy wire tree, then encoded into that tree for existing transports.
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
