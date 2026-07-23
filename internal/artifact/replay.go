package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type metadataArtifact struct {
	path     string
	orderKey string
	hash     string
	hlc      string
	event    metadataEvent
}

type metadataEventEnvelope struct {
	Version int             `json:"v"`
	HLC     json.RawMessage `json:"hlc"`
}

func observeMetadataStamp(clock *HLCClock, stamp HLCTimestamp, hlc string) error {
	if clock == nil {
		return nil
	}
	if _, err := clock.Observe(stamp); err != nil {
		return fmt.Errorf("observing metadata HLC %s: %w", hlc, err)
	}
	return nil
}

func markAppliedAndQuarantineMetadata(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	ref Ref,
	art metadataArtifact,
	reason string,
) error {
	if err := database.MarkMetadataEventApplied(ctx, ref.Origin, art.orderKey, art.hash); err != nil {
		return err
	}
	return store.Quarantine(ctx, ref, reason)
}

func metadataArtifactOrderKey(path string) (string, error) {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, metadataEventExtension)
	if name == base {
		return "", fmt.Errorf("metadata artifact %s missing %s extension", base, metadataEventExtension)
	}
	return name, nil
}

func validateMetadataArtifactEvent(art metadataArtifact, origin string) error {
	if art.event.HLC != art.hlc {
		return fmt.Errorf("metadata event %s HLC mismatch: got %q", art.path, art.event.HLC)
	}
	if art.event.Origin != origin {
		return fmt.Errorf(
			"metadata event %s origin mismatch for %s: got %q",
			art.path, origin, art.event.Origin,
		)
	}
	if art.event.SessionGID == "" {
		return fmt.Errorf("metadata event %s has empty session GID", art.path)
	}
	if art.event.Version > formatVersion {
		return fmt.Errorf(
			"%w: metadata event %s has artifact version %d",
			errFutureArtifactVersion, art.path, art.event.Version,
		)
	}
	if art.event.Version != formatVersion {
		return fmt.Errorf(
			"metadata event %s has unsupported artifact version %d",
			art.path, art.event.Version,
		)
	}
	// Checked after the version gate: a future format may change the HLC
	// shape, but a current-version event with an unparseable HLC would
	// poison raw order-key LWW comparison and must never be accepted.
	if _, err := ParseHLCTimestamp(art.hlc); err != nil {
		return fmt.Errorf("metadata event %s has invalid HLC: %v", art.path, err)
	}
	return nil
}

func metadataProjection(art metadataArtifact, localOrigin string) (db.MetadataProjection, error) {
	event := art.event
	field, value, displayName, pin, err := metadataProjectionFields(event)
	if err != nil {
		return db.MetadataProjection{}, err
	}
	return db.MetadataProjection{
		EventOrigin:    event.Origin,
		OrderKey:       art.orderKey,
		HLC:            event.HLC,
		ArtifactHash:   art.hash,
		SessionGID:     event.SessionGID,
		LocalSessionID: metadataLocalSessionID(localOrigin, event.SessionGID),
		Field:          field,
		Op:             event.Op,
		Value:          value,
		DisplayName:    displayName,
		Pin:            pin,
	}, nil
}

func metadataProjectionFields(
	event metadataEvent,
) (field string, value string, displayName *string, pin *db.MetadataPinProjection, err error) {
	switch event.Op {
	case MetadataOpRename:
		var payload struct {
			DisplayName *string `json:"display_name"`
		}
		if err := json.Unmarshal(event.Value, &payload); err != nil {
			return "", "", nil, nil, fmt.Errorf("decoding rename metadata value: %w", err)
		}
		value, err := metadataCanonicalValue(event.Value)
		return "display_name", value, payload.DisplayName, nil, err
	case MetadataOpSoftDelete, MetadataOpRestore:
		return "deleted_at", event.Op, nil, nil, nil
	case MetadataOpStar, MetadataOpUnstar:
		return "starred", event.Op, nil, nil, nil
	case MetadataOpPin, MetadataOpUnpin:
		if event.Pin == nil {
			return "", "", nil, nil, fmt.Errorf("%s metadata event missing pin payload", event.Op)
		}
		value, err := metadataCanonicalPin(*event.Pin)
		if err != nil {
			return "", "", nil, nil, err
		}
		return "pin:" + metadataPinAnchor(*event.Pin), value, nil, &db.MetadataPinProjection{
			SourceUUID: event.Pin.SourceUUID,
			Ordinal:    event.Pin.Ordinal,
			Note:       event.Pin.Note,
		}, nil
	case MetadataOpPurge:
		return "purge", event.Op, nil, nil, nil
	default:
		return "", "", nil, nil, fmt.Errorf("unsupported metadata event op %q", event.Op)
	}
}

func metadataLocalSessionID(localOrigin, gid string) string {
	prefix := localOrigin + "~"
	if after, ok := strings.CutPrefix(gid, prefix); ok {
		return after
	}
	return gid
}

func metadataPinAnchor(pin MetadataPin) string {
	if pin.SourceUUID != "" {
		return "source_uuid:" + pin.SourceUUID
	}
	return fmt.Sprintf("ordinal:%d", pin.Ordinal)
}

func metadataCanonicalValue(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	data, err := canonicalJSON(raw)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(data)), nil
}

func metadataCanonicalPin(pin MetadataPin) (string, error) {
	data, err := canonicalJSON(pin)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(data)), nil
}
