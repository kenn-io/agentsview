package friction

import (
	"encoding/json/v2"
	"fmt"
)

type snapshotFingerprintSignal struct {
	Kind     Kind   `json:"kind"`
	Subject  string `json:"subject_id"`
	ToolName string `json:"tool_name"`
	Text     string `json:"text"`
	Label    string `json:"label"`
}

type snapshotFingerprintPayload struct {
	Signals []snapshotFingerprintSignal `json:"signals"`
}

func fingerprintForSnapshotSignal(item snapshotFingerprintSignal) string {
	return (Signal{
		Kind: item.Kind, SubjectID: item.Subject, ToolName: item.ToolName, Text: item.Text, Label: item.Label,
	}).Fingerprint()
}

// SnapshotFingerprintList returns the unique fingerprints present in frozen
// digest signals, in snapshot order.
func SnapshotFingerprintList(snapshot []byte) ([]string, error) {
	var frozen snapshotFingerprintPayload
	if err := json.Unmarshal(snapshot, &frozen); err != nil {
		return nil, fmt.Errorf("decoding friction snapshot signals: %w", err)
	}
	seen := make(map[string]struct{}, len(frozen.Signals))
	out := make([]string, 0, len(frozen.Signals))
	for _, item := range frozen.Signals {
		fingerprint := fingerprintForSnapshotSignal(item)
		if _, ok := seen[fingerprint]; ok {
			continue
		}
		seen[fingerprint] = struct{}{}
		out = append(out, fingerprint)
	}
	return out, nil
}

// SnapshotFingerprints returns the requested fingerprints present in frozen
// digest signals without depending on the mutable findings table.
func SnapshotFingerprints(snapshot []byte, fingerprints []string) ([]string, error) {
	return snapshotFingerprints(snapshot, fingerprints, false)
}

// SnapshotContainsFingerprints checks frozen digest signals for any requested
// fingerprint without depending on the mutable findings table.
func SnapshotContainsFingerprints(snapshot []byte, fingerprints []string) (bool, error) {
	matches, err := snapshotFingerprints(snapshot, fingerprints, true)
	return len(matches) != 0, err
}

func snapshotFingerprints(snapshot []byte, fingerprints []string, firstOnly bool) ([]string, error) {
	if len(fingerprints) == 0 {
		return nil, nil
	}
	var frozen snapshotFingerprintPayload
	if err := json.Unmarshal(snapshot, &frozen); err != nil {
		return nil, fmt.Errorf("decoding friction snapshot signals: %w", err)
	}
	wanted := make(map[string]struct{}, len(fingerprints))
	for _, fingerprint := range fingerprints {
		wanted[fingerprint] = struct{}{}
	}
	matches := make([]string, 0, len(wanted))
	for _, item := range frozen.Signals {
		fingerprint := fingerprintForSnapshotSignal(item)
		if _, ok := wanted[fingerprint]; ok {
			matches = append(matches, fingerprint)
			delete(wanted, fingerprint)
			if firstOnly || len(wanted) == 0 {
				break
			}
		}
	}
	return matches, nil
}
