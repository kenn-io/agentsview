package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"sort"

	"go.kenn.io/agentsview/internal/db"
)

type artifactExportStore interface {
	ListOwnedSessionIDsForExport(context.Context) ([]string, error)
	PendingArtifactExports(context.Context, int) ([]db.ArtifactExportQueueItem, error)
	ArtifactExportClaims(context.Context, []string) ([]db.ArtifactExportQueueItem, error)
	GetSessionFull(context.Context, string) (*db.Session, error)
	GetAllMessages(context.Context, string) ([]db.Message, error)
	GetUsageEvents(context.Context, string) ([]db.UsageEvent, error)
	ApplyArtifactPublicationChanges(context.Context, string, []db.ArtifactPublicationChange) (int64, bool, error)
	AcknowledgeArtifactExports(context.Context, []db.ArtifactExportQueueItem) error
	GetArtifactCheckpointHead(context.Context, string) (db.ArtifactCheckpointHead, bool, error)
	StreamArtifactPublications(context.Context, string, func(db.ArtifactPublication) error) (int64, error)
	RecordArtifactCheckpointHead(context.Context, db.ArtifactCheckpointHead, []db.ArtifactExportQueueItem) error
	GetArtifactCheckpointFloor(context.Context, string) (int, bool, error)
	ReserveArtifactCheckpointSequence(context.Context, string, int) (int, error)
}

const artifactExportBatchSize = 128

// ExportOptions selects artifact publication work. The default and explicit-ID
// modes process one bounded batch. Full drains bounded claim pages before
// recreating every currently-owned session's immutable dependencies one body
// at a time; publication authority still changes only through guarded claims.
type ExportOptions struct {
	Origin     string
	SessionIDs []string
	Full       bool
}

// ExportResult summarizes one canonical store export.
type ExportResult struct {
	ExportedSessions   int
	CheckpointCreated  bool
	CheckpointSequence int
}

// ExportToStore publishes generation-guarded work into the canonical artifact
// store. Immutable dependencies are created before their manifest, and each
// bounded page's checkpoint is created last. Full mode may publish several
// bounded pages before its dependency-recovery pass completes.
func ExportToStore(
	ctx context.Context,
	database artifactExportStore,
	store ArtifactStore,
	opts ExportOptions,
) (_ ExportResult, retErr error) {
	if database == nil {
		return ExportResult{}, errors.New("artifact export database is required")
	}
	if store == nil {
		return ExportResult{}, errors.New("artifact export store is required")
	}
	if err := validateOriginID(opts.Origin); err != nil {
		return ExportResult{}, err
	}
	if len(opts.SessionIDs) > 1024 {
		return ExportResult{}, errors.New("artifact export session batch exceeds 1024 rows")
	}
	if opts.Full && len(opts.SessionIDs) > 0 {
		return ExportResult{}, errors.New("full artifact export cannot select session IDs")
	}
	if opts.Full {
		return exportFullToStore(ctx, database, store, opts.Origin)
	}

	var claims []db.ArtifactExportQueueItem
	var err error
	if len(opts.SessionIDs) > 0 {
		claims, err = database.ArtifactExportClaims(ctx, opts.SessionIDs)
	} else {
		queueLimit := artifactExportBatchSize
		if opts.Full {
			queueLimit = 1024
		}
		claims, err = database.PendingArtifactExports(ctx, queueLimit)
	}
	if err != nil {
		return ExportResult{}, fmt.Errorf("reading artifact export queue: %w", err)
	}
	claimByID := make(map[string]db.ArtifactExportQueueItem, len(claims))
	for _, claim := range claims {
		claimByID[claim.SessionID] = claim
	}

	selected, err := selectArtifactExportSessionIDs(ctx, database, opts, claims, claimByID)
	if err != nil {
		return ExportResult{}, err
	}
	changes := make([]db.ArtifactPublicationChange, 0, len(claims))
	acknowledged := make([]db.ArtifactExportQueueItem, 0, len(claims))
	result := ExportResult{}
	for _, sessionID := range selected {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		claim, claimed := claimByID[sessionID]
		sess, err := database.GetSessionFull(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading artifact export session %s: %w", sessionID, err)
		}
		if sess == nil || sess.Machine != "local" || sess.DeletedAt != nil {
			if claimed {
				changes = append(changes, db.ArtifactPublicationChange{
					SessionID: sessionID, Generation: claim.Generation, Delete: true,
				})
				acknowledged = append(acknowledged, claim)
			}
			continue
		}
		messages, err := database.GetAllMessages(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading artifact export messages %s: %w", sessionID, err)
		}
		usageEvents, err := database.GetUsageEvents(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading artifact export usage %s: %w", sessionID, err)
		}
		manifestHash, created, err := exportLoadedSessionToStore(
			ctx, store, opts.Origin, sess, messages, usageEvents,
			productionArtifactLimits(),
		)
		if err != nil {
			return result, err
		}
		if created && claimed {
			result.ExportedSessions++
		}
		if claimed {
			changes = append(changes, db.ArtifactPublicationChange{
				SessionID: sessionID, Generation: claim.Generation,
				ManifestHash: manifestHash, SourceFingerprint: manifestHash,
			})
			acknowledged = append(acknowledged, claim)
		}
	}

	head, hadHead, err := database.GetArtifactCheckpointHead(ctx, opts.Origin)
	if err != nil {
		return result, fmt.Errorf("reading artifact checkpoint head: %w", err)
	}
	publicationRevision, changed, err := database.ApplyArtifactPublicationChanges(
		ctx, opts.Origin, changes,
	)
	if err != nil {
		return result, err
	}
	if !changed && hadHead && head.PublicationRevision == publicationRevision {
		verified, err := statRecordedCheckpoint(ctx, store, head)
		if err != nil {
			return result, err
		}
		if verified {
			if err := database.AcknowledgeArtifactExports(ctx, acknowledged); err != nil {
				return result, err
			}
			return result, nil
		}
	}

	comparableHead := !changed && hadHead
	if !hadHead {
		head, comparableHead, err = latestValidCheckpointHead(ctx, store, opts.Origin)
		if err != nil {
			return result, err
		}
	}
	mapSpool, mapDigest, mapRevision, err := spoolArtifactPublicationMap(ctx, database, opts.Origin)
	if err != nil {
		return result, err
	}
	defer func() {
		if mapSpool != nil {
			retErr = errors.Join(retErr, closeAndRemoveExportSpool(mapSpool))
		}
	}()
	if comparableHead && head.SessionMapSHA256 == mapDigest {
		checkpointSpool, checkpointIdentity, err := spoolArtifactCheckpoint(
			ctx, mapSpool, opts.Origin, head.Sequence,
		)
		if err != nil {
			return result, err
		}
		defer func() {
			if checkpointSpool != nil {
				retErr = errors.Join(retErr, closeAndRemoveExportSpool(checkpointSpool))
			}
		}()
		if checkpointIdentity.SHA256 != head.CheckpointSHA256 {
			return result, fmt.Errorf(
				"%w: recorded checkpoint %d hash differs from canonical publications",
				ErrArtifactCorrupt, head.Sequence,
			)
		}
		head.PublicationRevision = mapRevision
		head.CheckpointSize = checkpointIdentity.Size
		checkpointRef, err := NewRef(opts.Origin, KindCheckpoints,
			fmt.Sprintf("cp-%010d.json", head.Sequence))
		if err != nil {
			return result, err
		}
		if err := closeAndRemoveExportSpool(mapSpool); err != nil {
			return result, fmt.Errorf("cleaning artifact session map spool: %w", err)
		}
		mapSpool = nil
		create, err := store.Create(ctx, checkpointRef, checkpointIdentity,
			canonicalArtifactMediaType(KindCheckpoints), checkpointSpool)
		if err != nil {
			return result, fmt.Errorf("recreating artifact checkpoint: %w", err)
		}
		if err := closeAndRemoveExportSpool(checkpointSpool); err != nil {
			return result, fmt.Errorf("cleaning artifact checkpoint spool: %w", err)
		}
		checkpointSpool = nil
		if err := database.RecordArtifactCheckpointHead(ctx, head, acknowledged); err != nil {
			return result, err
		}
		result.CheckpointCreated = create.Created
		result.CheckpointSequence = head.Sequence
		return result, nil
	}

	sequence, err := reserveCheckpointSequenceFromStore(ctx, database, store, opts.Origin)
	if err != nil {
		return result, err
	}
	checkpointSpool, checkpointIdentity, err := spoolArtifactCheckpoint(
		ctx, mapSpool, opts.Origin, sequence,
	)
	if err != nil {
		return result, err
	}
	defer func() {
		if checkpointSpool != nil {
			retErr = errors.Join(retErr, closeAndRemoveExportSpool(checkpointSpool))
		}
	}()
	checkpointRef, err := NewRef(opts.Origin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", sequence))
	if err != nil {
		return result, err
	}
	if err := closeAndRemoveExportSpool(mapSpool); err != nil {
		return result, fmt.Errorf("cleaning artifact session map spool: %w", err)
	}
	mapSpool = nil
	if _, err := store.Create(ctx, checkpointRef, checkpointIdentity,
		canonicalArtifactMediaType(KindCheckpoints), checkpointSpool,
	); err != nil {
		return result, fmt.Errorf("creating artifact checkpoint: %w", err)
	}
	if err := closeAndRemoveExportSpool(checkpointSpool); err != nil {
		return result, fmt.Errorf("cleaning artifact checkpoint spool: %w", err)
	}
	checkpointSpool = nil
	if err := database.RecordArtifactCheckpointHead(ctx, db.ArtifactCheckpointHead{
		Origin: opts.Origin, Sequence: sequence, PublicationRevision: mapRevision,
		SessionMapSHA256: mapDigest, CheckpointSHA256: checkpointIdentity.SHA256,
		CheckpointSize: checkpointIdentity.Size,
	}, acknowledged); err != nil {
		return result, err
	}
	result.CheckpointCreated = true
	result.CheckpointSequence = sequence
	return result, nil
}

// maxExportDrainRounds bounds the terminal drain loop below. Full export must
// terminate even when the queue is kept perpetually non-empty by concurrent
// writers; after the cap is reached the accumulated result is still returned,
// paired with an error describing the unsettled queue.
const maxExportDrainRounds = 32

func exportFullToStore(
	ctx context.Context,
	database artifactExportStore,
	store ArtifactStore,
	origin string,
) (ExportResult, error) {
	result := ExportResult{}
	processed := make(map[string]struct{})
	drain := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			claims, err := database.PendingArtifactExports(ctx, maxArtifactExportBatchSize)
			if err != nil {
				return fmt.Errorf("reading full artifact export queue: %w", err)
			}
			if len(claims) == 0 {
				return nil
			}
			ids := make([]string, len(claims))
			for i, claim := range claims {
				ids[i] = claim.SessionID
				processed[claim.SessionID] = struct{}{}
			}
			page, err := ExportToStore(ctx, database, store, ExportOptions{
				Origin: origin, SessionIDs: ids,
			})
			if err != nil {
				return err
			}
			mergeArtifactExportResult(&result, page)
		}
	}
	if err := drain(); err != nil {
		return result, err
	}
	ids, err := database.ListOwnedSessionIDsForExport(ctx)
	if err != nil {
		return result, fmt.Errorf("listing sessions for full artifact export: %w", err)
	}
	for _, sessionID := range ids {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if _, ok := processed[sessionID]; ok {
			continue
		}
		sess, err := database.GetSessionFull(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading full artifact export session %s: %w", sessionID, err)
		}
		if sess == nil || sess.Machine != "local" || sess.DeletedAt != nil {
			continue
		}
		messages, err := database.GetAllMessages(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading full artifact export messages %s: %w", sessionID, err)
		}
		usageEvents, err := database.GetUsageEvents(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading full artifact export usage %s: %w", sessionID, err)
		}
		if _, _, err := exportLoadedSessionToStore(
			ctx, store, origin, sess, messages, usageEvents, productionArtifactLimits(),
		); err != nil {
			return result, err
		}
	}
	if err := drain(); err != nil {
		return result, err
	}
	for range maxExportDrainRounds {
		final, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin})
		if err != nil {
			return result, err
		}
		mergeArtifactExportResult(&result, final)
		pending, err := database.PendingArtifactExports(ctx, 1)
		if err != nil {
			return result, fmt.Errorf("checking concurrent full artifact work: %w", err)
		}
		if len(pending) == 0 {
			return result, nil
		}
		if err := drain(); err != nil {
			return result, err
		}
	}
	return result, fmt.Errorf(
		"artifact export queue did not settle after %d drain rounds", maxExportDrainRounds,
	)
}

const maxArtifactExportBatchSize = 1024

func mergeArtifactExportResult(total *ExportResult, page ExportResult) {
	total.ExportedSessions += page.ExportedSessions
	if page.CheckpointCreated {
		total.CheckpointCreated = true
	}
	if page.CheckpointSequence > total.CheckpointSequence {
		total.CheckpointSequence = page.CheckpointSequence
	}
}

func selectArtifactExportSessionIDs(
	ctx context.Context,
	database artifactExportStore,
	opts ExportOptions,
	claims []db.ArtifactExportQueueItem,
	claimByID map[string]db.ArtifactExportQueueItem,
) ([]string, error) {
	selected := make(map[string]struct{})
	switch {
	case opts.Full:
		ids, err := database.ListOwnedSessionIDsForExport(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing sessions for full artifact export: %w", err)
		}
		for _, id := range ids {
			selected[id] = struct{}{}
		}
		for _, claim := range claims {
			selected[claim.SessionID] = struct{}{}
		}
	case len(opts.SessionIDs) > 0:
		for _, id := range opts.SessionIDs {
			if _, ok := claimByID[id]; ok {
				selected[id] = struct{}{}
			}
		}
	default:
		for _, claim := range claims {
			selected[claim.SessionID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func exportLoadedSessionToStore(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	sess *db.Session,
	messages []db.Message,
	usageEvents []db.UsageEvent,
	limits artifactLimits,
) (string, bool, error) {
	if len(messages) > limits.sessionMessages {
		return "", false, fmt.Errorf(
			"session message limit exceeded for %s: got %d, limit %d",
			sess.ID, len(messages), limits.sessionMessages,
		)
	}
	if len(usageEvents) > limits.manifestUsageEvents {
		return "", false, fmt.Errorf(
			"manifest usage event limit exceeded for %s: got %d, limit %d",
			sess.ID, len(usageEvents), limits.manifestUsageEvents,
		)
	}
	if err := validateExportNestedCollections(messages, limits); err != nil {
		return "", false, fmt.Errorf("validating nested collections for %s: %w", sess.ID, err)
	}

	segmentHashes, err := exportMessageSegmentsToStore(
		ctx, store, origin, sess.ID, messages, limits,
	)
	if err != nil {
		return "", false, err
	}

	wireSession := manifestSessionFromDB(*sess)
	wireSession.Machine = origin
	normalizeManifestSessionLocalState(&wireSession)
	m := manifest{
		Version: formatVersion, Origin: origin, NativeSessionID: sess.ID,
		Session: wireSession, SessionName: sess.SessionName,
		Segments: segmentHashes, UsageEvents: canonicalUsageEvents(usageEvents),
		DataVersion: sess.DataVersion, Generation: 1,
		SessionHasToolCalls:   sess.HasToolCalls,
		SessionHasContextData: sess.HasContextData,
		SessionQualitySignals: manifestQualitySignalsFromDB(sess.StoredQualitySignals()),
	}
	data, err := canonicalJSON(m)
	if err != nil {
		return "", false, err
	}
	if int64(len(data)) > manifestDecodedLimit {
		return "", false, fmt.Errorf(
			"generated manifest exceeds %d-byte readable limit: got %d bytes",
			manifestDecodedLimit, len(data),
		)
	}
	hash := hashHex(data)
	identity, err := NewIdentity(hash, int64(len(data)))
	if err != nil {
		return "", false, err
	}
	ref, err := NewRef(origin, KindManifests, hash+".json")
	if err != nil {
		return "", false, err
	}
	created, err := store.Create(ctx, ref, identity,
		canonicalArtifactMediaType(KindManifests), bytes.NewReader(data))
	if err != nil {
		return "", false, fmt.Errorf("creating manifest for %s: %w", sess.ID, err)
	}
	return hash, created.Created, nil
}

func exportMessageSegmentsToStore(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	sessionID string,
	messages []db.Message,
	limits artifactLimits,
) (_ []string, retErr error) {
	segmentHashes := make([]string, 0, 1)
	seen := make(map[string]struct{})
	var decodedBytes int64
	var spool *os.File
	var hasher hash.Hash
	var segmentBytes int64
	segmentMessages := 0
	segmentNested := nestedCollectionCounts{}
	defer func() {
		if spool != nil {
			retErr = errors.Join(retErr, closeAndRemoveExportSpool(spool))
		}
	}()

	start := func() error {
		var err error
		spool, err = os.CreateTemp("", "agentsview-artifact-segment-*")
		if err != nil {
			return fmt.Errorf("creating artifact segment spool: %w", err)
		}
		if err := spool.Chmod(0o600); err != nil {
			return fmt.Errorf("securing artifact segment spool: %w", err)
		}
		hasher = sha256.New()
		segmentBytes = 0
		segmentMessages = 0
		segmentNested = nestedCollectionCounts{}
		return nil
	}
	flush := func() error {
		if len(segmentHashes) >= limits.manifestSegments {
			return fmt.Errorf("manifest segment reference limit exceeded for %s: limit %d",
				sessionID, limits.manifestSegments)
		}
		digest := hex.EncodeToString(hasher.Sum(nil))
		if _, duplicate := seen[digest]; duplicate {
			return fmt.Errorf("generated duplicate segment reference %s", digest)
		}
		identity, err := NewIdentity(digest, segmentBytes)
		if err != nil {
			return err
		}
		ref, err := NewRef(origin, KindSegments, digest+".ndjson")
		if err != nil {
			return err
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewinding artifact segment spool: %w", err)
		}
		if _, err := store.Create(ctx, ref, identity,
			canonicalArtifactMediaType(KindSegments), spool); err != nil {
			return fmt.Errorf("creating segment for %s: %w", sessionID, err)
		}
		cleanupErr := closeAndRemoveExportSpool(spool)
		spool = nil
		if cleanupErr != nil {
			return fmt.Errorf("cleaning artifact segment spool: %w", cleanupErr)
		}
		seen[digest] = struct{}{}
		segmentHashes = append(segmentHashes, digest)
		return nil
	}

	if err := start(); err != nil {
		return nil, err
	}
	for _, message := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		messageNested, err := dbMessageNestedCounts(message, limits)
		if err != nil {
			return nil, err
		}
		if err := validateMessageFitsSegment(message.Ordinal, messageNested, limits); err != nil {
			return nil, err
		}
		data, err := canonicalJSON(segmentMessageFromDB(message))
		if err != nil {
			return nil, fmt.Errorf("encoding message segment: %w", err)
		}
		if int64(len(data)) > segmentDecodedLimit {
			return nil, fmt.Errorf(
				"encoded message record at ordinal %d exceeds %d-byte readable limit",
				message.Ordinal, segmentDecodedLimit,
			)
		}
		if segmentBytes > 0 && (segmentBytes+int64(len(data)) > segmentTargetSize ||
			segmentMessages >= limits.segmentMessages ||
			exceedsCollectionLimit(segmentNested.toolCalls,
				messageNested.toolCalls, limits.segmentToolCalls) ||
			exceedsCollectionLimit(segmentNested.resultEvents,
				messageNested.resultEvents, limits.segmentResultEvents)) {
			if err := flush(); err != nil {
				return nil, err
			}
			if err := start(); err != nil {
				return nil, err
			}
		}
		if int64(len(data)) > limits.sessionDecodedBytes-decodedBytes {
			return nil, fmt.Errorf("session decoded byte limit exceeded for %s: limit %d",
				sessionID, limits.sessionDecodedBytes)
		}
		if _, err := io.MultiWriter(spool, hasher).Write(data); err != nil {
			return nil, fmt.Errorf("writing artifact segment spool: %w", err)
		}
		segmentBytes += int64(len(data))
		decodedBytes += int64(len(data))
		segmentMessages++
		segmentNested.toolCalls += messageNested.toolCalls
		segmentNested.resultEvents += messageNested.resultEvents
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return segmentHashes, nil
}

func spoolArtifactPublicationMap(
	ctx context.Context,
	database artifactExportStore,
	origin string,
) (_ *os.File, _ string, _ int64, retErr error) {
	spool, err := os.CreateTemp("", "agentsview-artifact-map-*")
	if err != nil {
		return nil, "", 0, fmt.Errorf("creating artifact session map spool: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			retErr = errors.Join(retErr, exportSpoolCleanup(spool))
		}
	}()
	if err := exportSpoolChmod(spool); err != nil {
		return nil, "", 0, fmt.Errorf("securing artifact session map spool: %w", err)
	}
	hasher := sha256.New()
	writer := io.MultiWriter(spool, hasher)
	if _, err := io.WriteString(writer, "{"); err != nil {
		return nil, "", 0, err
	}
	first := true
	revision, err := database.StreamArtifactPublications(ctx, origin, func(publication db.ArtifactPublication) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if publication.Origin != origin {
			return fmt.Errorf("artifact publication origin mismatch: got %q", publication.Origin)
		}
		gid, err := json.Marshal(origin + "~" + publication.SessionID)
		if err != nil {
			return err
		}
		hash, err := json.Marshal(publication.ManifestHash)
		if err != nil {
			return err
		}
		if !first {
			if _, err := io.WriteString(writer, ","); err != nil {
				return err
			}
		}
		first = false
		_, err = writer.Write(append(append(gid, ':'), hash...))
		return err
	})
	if err != nil {
		return nil, "", 0, fmt.Errorf("streaming artifact session map: %w", err)
	}
	if _, err := io.WriteString(writer, "}"); err != nil {
		return nil, "", 0, err
	}
	if _, err := hasher.Write([]byte{'\n'}); err != nil {
		return nil, "", 0, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, "", 0, fmt.Errorf("rewinding artifact session map spool: %w", err)
	}
	failed = false
	return spool, hex.EncodeToString(hasher.Sum(nil)), revision, nil
}

func spoolArtifactCheckpoint(
	ctx context.Context,
	mapSpool *os.File,
	origin string,
	sequence int,
) (_ *os.File, _ Identity, retErr error) {
	if _, err := mapSpool.Seek(0, io.SeekStart); err != nil {
		return nil, Identity{}, fmt.Errorf("rewinding artifact session map: %w", err)
	}
	spool, err := os.CreateTemp("", "agentsview-artifact-checkpoint-*")
	if err != nil {
		return nil, Identity{}, fmt.Errorf("creating artifact checkpoint spool: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			retErr = errors.Join(retErr, exportSpoolCleanup(spool))
		}
	}()
	if err := exportSpoolChmod(spool); err != nil {
		return nil, Identity{}, fmt.Errorf("securing artifact checkpoint spool: %w", err)
	}
	hasher := sha256.New()
	writer := io.MultiWriter(spool, hasher)
	originJSON, err := json.Marshal(origin)
	if err != nil {
		return nil, Identity{}, err
	}
	if _, err := fmt.Fprintf(writer, `{"origin":%s,"seq":%d,"sessions":`, originJSON, sequence); err != nil {
		return nil, Identity{}, err
	}
	if _, err := io.Copy(writer, &contextArtifactReader{ctx: ctx, reader: mapSpool}); err != nil {
		return nil, Identity{}, fmt.Errorf("copying artifact session map: %w", err)
	}
	if _, err := io.WriteString(writer, `,"v":1}`+"\n"); err != nil {
		return nil, Identity{}, err
	}
	info, err := spool.Stat()
	if err != nil {
		return nil, Identity{}, fmt.Errorf("stating artifact checkpoint spool: %w", err)
	}
	identity, err := NewIdentity(hex.EncodeToString(hasher.Sum(nil)), info.Size())
	if err != nil {
		return nil, Identity{}, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, Identity{}, fmt.Errorf("rewinding artifact checkpoint spool: %w", err)
	}
	failed = false
	return spool, identity, nil
}

func closeAndRemoveExportSpool(file *os.File) error {
	if file == nil {
		return nil
	}
	name := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(name)
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

var (
	exportSpoolChmod   = func(file *os.File) error { return file.Chmod(0o600) }
	exportSpoolCleanup = closeAndRemoveExportSpool
)

func canonicalUsageEvents(events []db.UsageEvent) []artifactUsageEvent {
	out := make([]artifactUsageEvent, len(events))
	for i, ev := range events {
		out[i] = artifactUsageEvent{
			MessageOrdinal:           ev.MessageOrdinal,
			Source:                   ev.Source,
			Model:                    ev.Model,
			InputTokens:              ev.InputTokens,
			OutputTokens:             ev.OutputTokens,
			CacheCreationInputTokens: ev.CacheCreationInputTokens,
			CacheReadInputTokens:     ev.CacheReadInputTokens,
			ReasoningTokens:          ev.ReasoningTokens,
			CostUSD:                  ev.CostUSD,
			CostStatus:               ev.CostStatus,
			CostSource:               ev.CostSource,
			OccurredAt:               ev.OccurredAt,
			DedupKey:                 ev.DedupKey,
		}
	}
	return out
}

func validateExportNestedCollections(msgs []db.Message, limits artifactLimits) error {
	total := nestedCollectionCounts{}
	for _, msg := range msgs {
		messageNested, err := dbMessageNestedCounts(msg, limits)
		if err != nil {
			return err
		}
		if err := validateMessageFitsSegment(msg.Ordinal, messageNested, limits); err != nil {
			return err
		}
		if exceedsCollectionLimit(
			total.toolCalls, messageNested.toolCalls, limits.sessionToolCalls,
		) {
			return fmt.Errorf(
				"session tool call limit exceeded at message ordinal %d: limit %d",
				msg.Ordinal, limits.sessionToolCalls,
			)
		}
		if exceedsCollectionLimit(
			total.resultEvents, messageNested.resultEvents, limits.sessionResultEvents,
		) {
			return fmt.Errorf(
				"session result event limit exceeded at message ordinal %d: limit %d",
				msg.Ordinal, limits.sessionResultEvents,
			)
		}
		total.toolCalls += messageNested.toolCalls
		total.resultEvents += messageNested.resultEvents
	}
	return nil
}

func dbMessageNestedCounts(
	msg db.Message,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	if len(msg.ToolCalls) > limits.messageToolCalls {
		return nestedCollectionCounts{}, fmt.Errorf(
			"tool call limit exceeded for message ordinal %d: got %d, limit %d",
			msg.Ordinal, len(msg.ToolCalls), limits.messageToolCalls,
		)
	}
	counts := nestedCollectionCounts{toolCalls: len(msg.ToolCalls)}
	for toolIndex, call := range msg.ToolCalls {
		if len(call.ResultEvents) > limits.toolResultEvents {
			return nestedCollectionCounts{}, fmt.Errorf(
				"result event limit exceeded for tool call %d in message ordinal %d: got %d, limit %d",
				toolIndex, msg.Ordinal, len(call.ResultEvents), limits.toolResultEvents,
			)
		}
		counts.resultEvents += len(call.ResultEvents)
	}
	return counts, nil
}

func validateMessageFitsSegment(
	ordinal int,
	counts nestedCollectionCounts,
	limits artifactLimits,
) error {
	if counts.toolCalls > limits.segmentToolCalls {
		return fmt.Errorf(
			"message ordinal %d cannot fit in one segment: got %d tool calls, segment limit %d",
			ordinal, counts.toolCalls, limits.segmentToolCalls,
		)
	}
	if counts.resultEvents > limits.segmentResultEvents {
		return fmt.Errorf(
			"message ordinal %d cannot fit in one segment: got %d result events, segment limit %d",
			ordinal, counts.resultEvents, limits.segmentResultEvents,
		)
	}
	return nil
}
