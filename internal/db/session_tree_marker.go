package db

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// SessionTreeEntry is the version of one member of a watched session tree.
type SessionTreeEntry struct {
	ID      string            `json:"id"`
	Count   int               `json:"count"`
	Version int64             `json:"version"`
	State   *SessionTreeState `json:"state"`
}

type SessionTreeState struct {
	State           string   `json:"state"`
	PublicID        string   `json:"public_id,omitempty"`
	Variants        []string `json:"variants,omitempty"`
	ContentRevision string   `json:"content_revision,omitempty"`
}

// SessionTreeMarker encodes a parent and its direct children deterministically.
func SessionTreeMarker(entries []SessionTreeEntry) (string, error) {
	if len(entries) == 0 {
		return "", fmt.Errorf("session not found")
	}
	slices.SortFunc(entries[1:], func(a, b SessionTreeEntry) int { return strings.Compare(a.ID, b.ID) })
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v1:%x", sha256.Sum256(encoded)), nil
}
