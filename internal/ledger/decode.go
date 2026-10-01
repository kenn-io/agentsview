package ledger

import (
	"slices"

	"go.kenn.io/agentsview/internal/serdejson"
)

var segmentJSONFields = []string{"source", "source_seq", "checksum", "created_at", "events"}

var eventJSONFields = []string{
	"event_id", "zone", "source", "source_seq", "timestamp", "correlation_id",
	"causation_id", "actor_ref", "object_ref", "event_class", "payload_tier", "payload",
}

func decodedObject(v any) (map[string]any, []string, bool) {
	switch obj := v.(type) {
	case serdejson.ObjectWithDuplicateKeys:
		return obj.Fields, obj.DuplicateKeys, true
	case map[string]any:
		return obj, nil, true
	default:
		return nil, nil, false
	}
}

func firstDuplicateKnownField(duplicates, knownFields []string) string {
	for _, duplicate := range duplicates {
		if slices.Contains(knownFields, duplicate) {
			return duplicate
		}
	}
	return ""
}
