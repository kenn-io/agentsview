package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const metadataEventExtension = ".json"

// errOriginNotAdopted reports that this machine has no artifact origin, i.e.
// it never opted into artifact sync via `sync --init`, a sync run, or a peer
// exchange. Recording paths treat it as "stay local"; explicit sync paths
// treat it as a hard error.
var errOriginNotAdopted = errors.New("artifact origin not adopted")

type metadataSuppressionKey struct{}

// Metadata operation names written into the metadata event ledger.
const (
	MetadataOpRename     = "rename"
	MetadataOpSoftDelete = "soft_delete"
	MetadataOpRestore    = "restore"
	MetadataOpStar       = "star"
	MetadataOpUnstar     = "unstar"
	MetadataOpPin        = "pin"
	MetadataOpUnpin      = "unpin"
	MetadataOpPurge      = "purge"
)

// MetadataPin identifies a pinned message with stable source coordinates.
type MetadataPin struct {
	SourceUUID string  `json:"source_uuid,omitempty"`
	Ordinal    int     `json:"ordinal"`
	Note       *string `json:"note,omitempty"`
}

// MetadataEventInput describes a local user metadata mutation to append.
type MetadataEventInput struct {
	SessionID string
	Op        string
	Value     json.RawMessage
	Pin       *MetadataPin
}

// MetadataRecord describes a metadata artifact written to disk.
type MetadataRecord struct {
	HLC        string
	Origin     string
	SessionGID string
	Op         string
	Hash       string
	Path       string
	Ref        Ref
}

// MetadataPublishedError reports that the artifact file was durably written,
// but local replay bookkeeping failed afterward.
type MetadataPublishedError struct {
	Record MetadataRecord
	Err    error
}

func (e *MetadataPublishedError) Error() string {
	return fmt.Sprintf("metadata event published but local replay state was not recorded: %v", e.Err)
}

func (e *MetadataPublishedError) Unwrap() error {
	return e.Err
}

// MetadataRecorderOptions configures metadata event artifact writes.
type MetadataRecorderOptions struct {
	Origin   string
	Store    ArtifactStore
	Now      func() time.Time
	MaxDrift time.Duration
}

// MetadataRecorder appends canonical metadata event artifacts.
type MetadataRecorder struct {
	mu       sync.Mutex
	database *db.DB
	origin   string
	store    ArtifactStore
	clock    *HLCClock
}

// NewMetadataRecorder creates a metadata event recorder for the local artifact store.
func NewMetadataRecorder(database *db.DB, opts MetadataRecorderOptions) *MetadataRecorder {
	return &MetadataRecorder{
		database: database,
		origin:   strings.TrimSpace(opts.Origin),
		store:    opts.Store,
		clock: NewHLCClock(database, HLCClockOptions{
			Now:      opts.Now,
			MaxDrift: opts.MaxDrift,
		}),
	}
}

// WithMetadataEventSuppression marks a context as replaying metadata events.
func WithMetadataEventSuppression(ctx context.Context) context.Context {
	return context.WithValue(ctx, metadataSuppressionKey{}, true)
}

// MetadataEventsSuppressed reports whether local metadata event writes are disabled.
func MetadataEventsSuppressed(ctx context.Context) bool {
	suppressed, _ := ctx.Value(metadataSuppressionKey{}).(bool)
	return suppressed
}

// Append writes one metadata event artifact unless ctx is replay-suppressed.
func (r *MetadataRecorder) Append(ctx context.Context, input MetadataEventInput) (MetadataRecord, error) {
	if MetadataEventsSuppressed(ctx) {
		return MetadataRecord{}, nil
	}
	if r == nil {
		return MetadataRecord{}, nil
	}
	if r.database == nil {
		return MetadataRecord{}, errors.New("metadata recorder database is required")
	}
	if r.store == nil {
		return MetadataRecord{}, errors.New("metadata recorder artifact store is required")
	}
	if input.SessionID == "" {
		return MetadataRecord{}, errors.New("metadata event session id is required")
	}
	if err := validateMetadataOp(input.Op); err != nil {
		return MetadataRecord{}, err
	}
	origin, err := r.resolveOrigin()
	if errors.Is(err, errOriginNotAdopted) {
		// The machine never opted into artifact sync: curation stays local.
		// If it joins a fleet later, the `sync --init` baseline snapshot
		// publishes the accumulated local curation state.
		return MetadataRecord{}, nil
	}
	if err != nil {
		return MetadataRecord{}, err
	}
	stamp, err := r.clock.Next()
	if err != nil {
		return MetadataRecord{}, err
	}
	event := metadataEvent{
		Version:    formatVersion,
		HLC:        stamp.String(),
		Origin:     origin,
		SessionGID: MetadataSessionGID(origin, input.SessionID),
		Op:         input.Op,
		Value:      input.Value,
		Pin:        input.Pin,
	}
	data, err := canonicalJSON(event)
	if err != nil {
		return MetadataRecord{}, err
	}
	hash := hashHex(data)
	orderKey := stamp.OrderingKey(hash)
	projection, err := metadataProjection(metadataArtifact{
		orderKey: orderKey,
		hash:     hash,
		hlc:      event.HLC,
		event:    event,
	}, origin)
	if err != nil {
		return MetadataRecord{}, err
	}
	ref, err := NewRef(origin, KindMeta, orderKey+metadataEventExtension)
	if err != nil {
		return MetadataRecord{}, err
	}
	record := MetadataRecord{
		HLC:        event.HLC,
		Origin:     origin,
		SessionGID: event.SessionGID,
		Op:         event.Op,
		Hash:       hash,
		Ref:        ref,
	}
	identity, err := NewIdentity(hash, int64(len(data)))
	if err != nil {
		return MetadataRecord{}, err
	}
	if _, err := r.store.Create(ctx, ref, identity,
		canonicalArtifactMediaType(KindMeta), bytes.NewReader(data)); err != nil {
		return MetadataRecord{}, fmt.Errorf("creating metadata event: %w", err)
	}
	if err := r.database.RecordMetadataArtifactProvenance(ctx, db.MetadataArtifactProvenance{
		Origin: origin, OrderKey: orderKey, ArtifactHash: hash,
		SessionGID: event.SessionGID, Op: event.Op,
	}); err != nil {
		return record, &MetadataPublishedError{
			Record: record,
			Err:    fmt.Errorf("recording metadata artifact provenance: %w", err),
		}
	}
	// Record the local event in the LWW replay register only after the artifact
	// exists. Otherwise a failed publish can leave hidden local state that wins
	// future LWW comparisons for an event no peer can import.
	if _, err := r.database.RecordLocalMetadataProjection(ctx, projection); err != nil {
		return record, &MetadataPublishedError{
			Record: record,
			Err:    fmt.Errorf("recording local metadata replay state: %w", err),
		}
	}
	return record, nil
}

// RepairLocalSessionMetadata rebuilds local replay bookkeeping for already
// published local metadata artifacts without re-applying their visible
// mutations.
func (r *MetadataRecorder) RepairLocalSessionMetadata(
	ctx context.Context,
	sessionID string,
	ops ...string,
) (int, error) {
	if r == nil {
		return 0, nil
	}
	if r.database == nil {
		return 0, errors.New("metadata recorder database is required")
	}
	if r.store == nil {
		return 0, errors.New("metadata recorder artifact store is required")
	}
	if sessionID == "" {
		return 0, errors.New("metadata event session id is required")
	}
	opSet := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		if err := validateMetadataOp(op); err != nil {
			return 0, err
		}
		opSet[op] = struct{}{}
	}
	origin, err := r.resolveOrigin()
	if errors.Is(err, errOriginNotAdopted) {
		// No origin means no published local artifacts to repair against.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	sessionGID := MetadataSessionGID(origin, sessionID)
	provenance, queryErr := r.database.MetadataArtifactProvenanceForSession(
		ctx, origin, sessionGID, ops...,
	)
	if queryErr != nil {
		return 0, queryErr
	}
	events, err := readMetadataArtifactsFromProvenance(
		ctx, r.database, r.store, provenance,
	)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, art := range events {
		if err := ctx.Err(); err != nil {
			return repaired, err
		}
		if art.event.SessionGID != sessionGID {
			continue
		}
		if len(opSet) > 0 {
			if _, ok := opSet[art.event.Op]; !ok {
				continue
			}
		}
		if err := validateMetadataArtifactEvent(art, origin); err != nil {
			if errors.Is(err, errFutureArtifactVersion) {
				continue
			}
			return repaired, err
		}
		if err := validateMetadataOp(art.event.Op); err != nil {
			return repaired, err
		}
		projection, err := metadataProjection(art, origin)
		if err != nil {
			return repaired, err
		}
		if _, err := r.database.RecordLocalMetadataProjection(ctx, projection); err != nil {
			return repaired, fmt.Errorf("repairing local metadata replay state: %w", err)
		}
		repaired++
	}
	return repaired, nil
}

func readMetadataArtifactsFromProvenance(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	provenance []db.MetadataArtifactProvenance,
) (events []metadataArtifact, retErr error) {
	for _, indexed := range provenance {
		ref, err := NewRef(indexed.Origin, KindMeta, indexed.OrderKey+metadataEventExtension)
		if err != nil {
			return nil, err
		}
		entry, err := store.Stat(ctx, ref)
		if errors.Is(err, ErrArtifactNotFound) {
			// Provenance survives deliberate removal or vault recovery. A
			// missing immutable event cannot repair replay state; callers may
			// publish a replacement event with a later ordering key.
			continue
		}
		if err != nil {
			return nil, err
		}
		if entry.Identity.SHA256 != indexed.ArtifactHash {
			return nil, fmt.Errorf("%w: metadata provenance identity changed", ErrArtifactCorrupt)
		}
		data, err := readVerifiedStoreArtifact(
			ctx, database, store, entry, manifestDecodedLimit,
		)
		if err != nil {
			return nil, err
		}
		var event metadataEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, fmt.Errorf("decoding metadata artifact %s: %w", ref.Name, err)
		}
		idx := strings.LastIndex(indexed.OrderKey, "-")
		if idx < 0 {
			return nil, fmt.Errorf("metadata artifact %s missing hash suffix", ref.Name)
		}
		events = append(events, metadataArtifact{
			path: ref.Name, orderKey: indexed.OrderKey, hash: indexed.ArtifactHash,
			hlc: indexed.OrderKey[:idx], event: event,
		})
	}
	return events, nil
}

// AppendBaseline writes metadata events for existing local curation that
// predates artifact metadata recording.
func (r *MetadataRecorder) AppendBaseline(ctx context.Context) (int, error) {
	if r == nil {
		return 0, nil
	}
	if r.database == nil {
		return 0, errors.New("metadata recorder database is required")
	}
	snap, err := r.database.MetadataBaselineSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	return r.AppendBaselineSnapshot(ctx, snap)
}

// AppendBaselineSnapshot writes metadata events from a previously captured
// curation snapshot. Callers that import peer artifacts before initialization
// should capture the snapshot before that import so newly imported rows cannot
// be re-published as local baseline metadata.
func (r *MetadataRecorder) AppendBaselineSnapshot(
	ctx context.Context,
	snap db.MetadataBaselineSnapshot,
) (int, error) {
	if r == nil {
		return 0, nil
	}
	if r.database == nil {
		return 0, errors.New("metadata recorder database is required")
	}
	origin, err := r.resolveOrigin()
	if err != nil {
		return 0, err
	}
	written := 0
	for _, rename := range snap.Renames {
		covered, err := r.baselineFieldCovered(ctx, origin, rename.SessionID, "display_name")
		if err != nil {
			return written, err
		}
		if covered {
			continue
		}
		value, err := metadataRenameValue(rename.DisplayName)
		if err != nil {
			return written, err
		}
		if _, err := r.Append(ctx, MetadataEventInput{
			SessionID: rename.SessionID,
			Op:        MetadataOpRename,
			Value:     value,
		}); err != nil {
			return written, fmt.Errorf("writing baseline rename metadata: %w", err)
		}
		written++
	}
	for _, sessionID := range snap.StarredSessionIDs {
		covered, err := r.baselineFieldCovered(ctx, origin, sessionID, "starred")
		if err != nil {
			return written, err
		}
		if covered {
			continue
		}
		if _, err := r.Append(ctx, MetadataEventInput{
			SessionID: sessionID,
			Op:        MetadataOpStar,
		}); err != nil {
			return written, fmt.Errorf("writing baseline star metadata: %w", err)
		}
		written++
	}
	for _, sessionID := range snap.SoftDeletedIDs {
		covered, err := r.baselineFieldCovered(ctx, origin, sessionID, "deleted_at")
		if err != nil {
			return written, err
		}
		if covered {
			continue
		}
		if _, err := r.Append(ctx, MetadataEventInput{
			SessionID: sessionID,
			Op:        MetadataOpSoftDelete,
		}); err != nil {
			return written, fmt.Errorf("writing baseline soft-delete metadata: %w", err)
		}
		written++
	}
	for _, pin := range snap.Pins {
		metadataPin := MetadataPin{
			SourceUUID: pin.SourceUUID,
			Ordinal:    pin.Ordinal,
			Note:       pin.Note,
		}
		covered, err := r.baselineFieldCovered(
			ctx, origin, pin.SessionID, "pin:"+metadataPinAnchor(metadataPin),
		)
		if err != nil {
			return written, err
		}
		if covered {
			continue
		}
		if _, err := r.Append(ctx, MetadataEventInput{
			SessionID: pin.SessionID,
			Op:        MetadataOpPin,
			Pin:       &metadataPin,
		}); err != nil {
			return written, fmt.Errorf("writing baseline pin metadata: %w", err)
		}
		written++
	}
	return written, nil
}

// materializeCurrentState rebuilds the local origin's canonical metadata
// publication into an empty replacement store. It republishes only replay
// winners authored by this origin, including negative/default operations, then
// baselines positive curation fields that have no replay winner. Foreign
// winners remain authoritative in SQLite and are never reattributed locally.
func (r *MetadataRecorder) materializeCurrentState(ctx context.Context) (int, error) {
	if r == nil {
		return 0, nil
	}
	if r.database == nil {
		return 0, errors.New("metadata recorder database is required")
	}
	origin, err := r.resolveOrigin()
	if err != nil {
		return 0, err
	}
	written := 0
	err = r.database.VisitMetadataReplayWinnersAuthoredBy(ctx, origin, func(
		winner db.MetadataProjection,
	) error {
		input := MetadataEventInput{
			SessionID: winner.SessionGID,
			Op:        winner.Op,
		}
		switch winner.Op {
		case MetadataOpRename:
			input.Value = json.RawMessage(winner.Value)
		case MetadataOpPin, MetadataOpUnpin:
			if winner.Pin == nil {
				return fmt.Errorf("materializing %s metadata without pin payload", winner.Op)
			}
			input.Pin = &MetadataPin{
				SourceUUID: winner.Pin.SourceUUID,
				Ordinal:    winner.Pin.Ordinal,
				Note:       winner.Pin.Note,
			}
		}
		if _, err := r.Append(ctx, input); err != nil {
			return fmt.Errorf("materializing current %s metadata: %w", winner.Op, err)
		}
		written++
		return nil
	})
	if err != nil {
		return written, err
	}
	baselined, err := r.AppendBaseline(ctx)
	return written + baselined, err
}

// materializeCurrentStateAtHLC reconstructs reset metadata idempotently. Replay
// winners retain their original immutable identity; positive baseline fields
// without a replay winner use one pre-persisted HLC so a crash after Create but
// before SQLite bookkeeping retries the same bytes and reference.
func (r *MetadataRecorder) materializeCurrentStateAtHLC(
	ctx context.Context, baselineHLC string,
) (int, error) {
	if r == nil {
		return 0, nil
	}
	if r.database == nil {
		return 0, errors.New("metadata recorder database is required")
	}
	if r.store == nil {
		return 0, errors.New("metadata reset materialization requires an artifact store")
	}
	if _, err := ParseHLCTimestamp(baselineHLC); err != nil {
		return 0, fmt.Errorf("parsing reset baseline HLC: %w", err)
	}
	origin, err := r.resolveOrigin()
	if err != nil {
		return 0, err
	}
	written := 0
	err = r.database.VisitMetadataReplayWinnersAuthoredBy(ctx, origin, func(
		winner db.MetadataProjection,
	) error {
		event, err := metadataEventFromProjection(winner)
		if err != nil {
			return err
		}
		if err := r.createExactMetadataEvent(
			ctx, event, winner.OrderKey, winner.ArtifactHash, false,
		); err != nil {
			return fmt.Errorf("materializing current %s metadata: %w", winner.Op, err)
		}
		written++
		return nil
	})
	if err != nil {
		return written, err
	}
	baselined := 0
	err = r.database.VisitMetadataBaselinePages(ctx, func(page db.MetadataBaselineSnapshot) error {
		pageWritten, err := r.materializeBaselineSnapshotAtHLC(
			ctx, origin, baselineHLC, page,
		)
		baselined += pageWritten
		return err
	})
	return written + baselined, err
}

func metadataEventFromProjection(winner db.MetadataProjection) (metadataEvent, error) {
	event := metadataEvent{
		Version:    formatVersion,
		HLC:        winner.HLC,
		Origin:     winner.EventOrigin,
		SessionGID: winner.SessionGID,
		Op:         winner.Op,
	}
	switch winner.Op {
	case MetadataOpRename:
		event.Value = json.RawMessage(winner.Value)
	case MetadataOpPin, MetadataOpUnpin:
		if winner.Pin == nil {
			return metadataEvent{}, fmt.Errorf("materializing %s metadata without pin payload", winner.Op)
		}
		event.Pin = &MetadataPin{
			SourceUUID: winner.Pin.SourceUUID,
			Ordinal:    winner.Pin.Ordinal,
			Note:       winner.Pin.Note,
		}
	}
	return event, nil
}

func (r *MetadataRecorder) createExactMetadataEvent(
	ctx context.Context,
	event metadataEvent,
	expectedOrderKey string,
	expectedHash string,
	recordProjection bool,
) error {
	if err := validateMetadataOp(event.Op); err != nil {
		return err
	}
	stamp, err := ParseHLCTimestamp(event.HLC)
	if err != nil {
		return err
	}
	data, err := canonicalJSON(event)
	if err != nil {
		return err
	}
	hash := hashHex(data)
	orderKey := stamp.OrderingKey(hash)
	if expectedHash != "" && hash != expectedHash {
		return fmt.Errorf("metadata projection hash mismatch: reconstructed %s, expected %s",
			hash, expectedHash)
	}
	if expectedOrderKey != "" && orderKey != expectedOrderKey {
		return fmt.Errorf("metadata projection order key mismatch: reconstructed %s, expected %s",
			orderKey, expectedOrderKey)
	}
	ref, err := NewRef(event.Origin, KindMeta, orderKey+metadataEventExtension)
	if err != nil {
		return err
	}
	identity, err := NewIdentity(hash, int64(len(data)))
	if err != nil {
		return err
	}
	if _, err := r.store.Create(ctx, ref, identity,
		canonicalArtifactMediaType(KindMeta), bytes.NewReader(data)); err != nil {
		return fmt.Errorf("creating exact metadata event: %w", err)
	}
	if !recordProjection {
		return nil
	}
	projection, err := metadataProjection(metadataArtifact{
		orderKey: orderKey,
		hash:     hash,
		hlc:      event.HLC,
		event:    event,
	}, event.Origin)
	if err != nil {
		return err
	}
	if err := r.database.RecordMetadataArtifactProvenance(ctx, db.MetadataArtifactProvenance{
		Origin: event.Origin, OrderKey: orderKey, ArtifactHash: hash,
		SessionGID: event.SessionGID, Op: event.Op,
	}); err != nil {
		return fmt.Errorf("recording exact metadata artifact provenance: %w", err)
	}
	if _, err := r.database.RecordLocalMetadataProjection(ctx, projection); err != nil {
		return fmt.Errorf("recording exact local metadata replay state: %w", err)
	}
	return nil
}

func (r *MetadataRecorder) materializeBaselineSnapshotAtHLC(
	ctx context.Context,
	origin string,
	hlc string,
	snapshot db.MetadataBaselineSnapshot,
) (int, error) {
	written := 0
	create := func(sessionID, field string, event metadataEvent) error {
		covered, err := r.baselineFieldCovered(ctx, origin, sessionID, field)
		if err != nil {
			return err
		}
		if covered {
			return nil
		}
		event.Version = formatVersion
		event.HLC = hlc
		event.Origin = origin
		event.SessionGID = MetadataSessionGID(origin, sessionID)
		if err := r.createExactMetadataEvent(ctx, event, "", "", true); err != nil {
			return err
		}
		written++
		return nil
	}
	for _, rename := range snapshot.Renames {
		value, err := metadataRenameValue(rename.DisplayName)
		if err != nil {
			return written, err
		}
		if err := create(rename.SessionID, "display_name", metadataEvent{
			Op: MetadataOpRename, Value: value,
		}); err != nil {
			return written, fmt.Errorf("materializing baseline rename metadata: %w", err)
		}
	}
	for _, sessionID := range snapshot.StarredSessionIDs {
		if err := create(sessionID, "starred", metadataEvent{Op: MetadataOpStar}); err != nil {
			return written, fmt.Errorf("materializing baseline star metadata: %w", err)
		}
	}
	for _, sessionID := range snapshot.SoftDeletedIDs {
		if err := create(sessionID, "deleted_at", metadataEvent{Op: MetadataOpSoftDelete}); err != nil {
			return written, fmt.Errorf("materializing baseline soft-delete metadata: %w", err)
		}
	}
	for _, pin := range snapshot.Pins {
		metadataPin := &MetadataPin{
			SourceUUID: pin.SourceUUID,
			Ordinal:    pin.Ordinal,
			Note:       pin.Note,
		}
		if err := create(
			pin.SessionID,
			"pin:"+metadataPinAnchor(*metadataPin),
			metadataEvent{Op: MetadataOpPin, Pin: metadataPin},
		); err != nil {
			return written, fmt.Errorf("materializing baseline pin metadata: %w", err)
		}
	}
	return written, nil
}

func (r *MetadataRecorder) baselineFieldCovered(
	ctx context.Context,
	origin string,
	sessionID string,
	field string,
) (bool, error) {
	_, ok, err := r.database.MetadataReplayStateOp(
		ctx, MetadataSessionGID(origin, sessionID), field,
	)
	if err != nil {
		return false, fmt.Errorf("checking baseline metadata field %s: %w", field, err)
	}
	return ok, nil
}

// MetadataSessionGID returns the global metadata target ID for a session.
func MetadataSessionGID(origin, sessionID string) string {
	if host, _ := parser.StripHostPrefix(sessionID); host != "" {
		return sessionID
	}
	return origin + "~" + sessionID
}

// resolveOrigin returns the recorder's origin without ever creating one: the
// explicit option wins, then the origin persisted in DB sync state. A machine
// with no origin anywhere has not opted into artifact sync and gets
// errOriginNotAdopted. The empty result is not cached, so a recorder built
// before opt-in starts resolving the origin as soon as it is adopted.
func (r *MetadataRecorder) resolveOrigin() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.origin != "" {
		if err := validateOriginID(r.origin); err != nil {
			return "", fmt.Errorf("metadata recorder origin: %w", err)
		}
		return r.origin, nil
	}
	origin, err := StoredOrigin(r.database)
	if err != nil {
		return "", err
	}
	if origin == "" {
		return "", errOriginNotAdopted
	}
	r.origin = origin
	return origin, nil
}

func metadataRenameValue(displayName *string) (json.RawMessage, error) {
	data, err := json.Marshal(struct {
		DisplayName *string `json:"display_name"`
	}{DisplayName: displayName})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func validateMetadataOp(op string) error {
	switch op {
	case MetadataOpRename,
		MetadataOpSoftDelete,
		MetadataOpRestore,
		MetadataOpStar,
		MetadataOpUnstar,
		MetadataOpPin,
		MetadataOpUnpin,
		MetadataOpPurge:
		return nil
	default:
		return fmt.Errorf("unsupported metadata event op %q", op)
	}
}
