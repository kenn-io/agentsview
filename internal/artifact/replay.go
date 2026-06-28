package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

func replayMetadata(
	ctx context.Context,
	database *db.DB,
	clock *HLCClock,
	originRoot, origin, localOrigin string,
) (int, error) {
	events, err := readMetadataArtifacts(originRoot, origin)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, art := range events {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		// Advance the local HLC past this remote event before applying it, so a
		// later local edit is causally ahead of the peer. If the clock cannot be
		// advanced (the remote wall time is beyond the drift bound), defer the
		// event rather than dragging local state to a value a future local edit
		// could not out-order; a later run retries once wall time advances.
		if clock != nil {
			if stamp, perr := ParseHLCTimestamp(art.hlc); perr == nil {
				if _, oerr := clock.Observe(stamp); oerr != nil {
					continue
				}
			}
		}
		if err := validateMetadataArtifactEvent(art, origin); err != nil {
			if errors.Is(err, errFutureArtifactVersion) {
				continue
			}
			return changed, err
		}
		if err := validateMetadataOp(art.event.Op); err != nil {
			if err := database.MarkMetadataEventApplied(ctx, origin, art.orderKey, art.hash); err != nil {
				return changed, err
			}
			if err := database.SetSyncState(metaStateKey(origin), art.orderKey); err != nil {
				return changed, fmt.Errorf("writing metadata import state for %s: %w", origin, err)
			}
			continue
		}
		projection, err := metadataProjection(art, localOrigin)
		if err != nil {
			return changed, err
		}
		res, err := database.ApplyMetadataProjection(ctx, projection)
		if err != nil {
			// The event targets a session or message that is not durable
			// locally yet. Skip only this event, leaving it unapplied so a
			// later run retries it, and keep replaying the rest of the origin.
			if errors.Is(err, db.ErrMetadataTargetUnavailable) {
				continue
			}
			return changed, fmt.Errorf("replaying metadata event %s: %w", art.path, err)
		}
		if res.Applied || res.Conflict {
			changed++
		}
		if err := database.SetSyncState(metaStateKey(origin), art.orderKey); err != nil {
			return changed, fmt.Errorf("writing metadata import state for %s: %w", origin, err)
		}
	}
	return changed, nil
}

func readMetadataArtifacts(originRoot, origin string) ([]metadataArtifact, error) {
	paths, err := filepath.Glob(filepath.Join(originRoot, "meta", "*"+metadataEventExtension))
	if err != nil {
		return nil, err
	}
	events := make([]metadataArtifact, 0, len(paths))
	for _, path := range paths {
		art, err := readMetadataArtifact(path)
		if err != nil {
			return nil, fmt.Errorf("reading metadata artifact for %s: %w", origin, err)
		}
		events = append(events, art)
	}
	sort.Slice(events, func(i, j int) bool {
		return events[i].orderKey < events[j].orderKey
	})
	return events, nil
}

func readMetadataArtifact(path string) (metadataArtifact, error) {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, metadataEventExtension)
	if name == base {
		return metadataArtifact{}, fmt.Errorf("metadata artifact %s missing %s extension", base, metadataEventExtension)
	}
	idx := strings.LastIndex(name, "-")
	if idx < 0 {
		return metadataArtifact{}, fmt.Errorf("metadata artifact %s missing hash suffix", base)
	}
	hlc := name[:idx]
	hash := name[idx+1:]
	data, err := os.ReadFile(path)
	if err != nil {
		return metadataArtifact{}, err
	}
	if got := hashHex(data); got != hash {
		return metadataArtifact{}, fmt.Errorf("metadata artifact %s hash mismatch: got %s", base, got)
	}
	var event metadataEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return metadataArtifact{}, fmt.Errorf("decoding metadata artifact %s: %w", base, err)
	}
	return metadataArtifact{
		path:     path,
		orderKey: name,
		hash:     hash,
		hlc:      hlc,
		event:    event,
	}, nil
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
