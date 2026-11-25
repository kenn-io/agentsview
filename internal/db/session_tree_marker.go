package db

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
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

// SessionTreeSnapshot includes the parent version from the same read as the marker.
type SessionTreeSnapshot struct {
	Marker        string
	ParentCount   int
	ParentVersion int64
}

// NewSessionTreeSnapshot builds a marker and retains the parent version.
func NewSessionTreeSnapshot(entries []SessionTreeEntry) (SessionTreeSnapshot, error) {
	marker, err := SessionTreeMarker(entries)
	if err != nil {
		return SessionTreeSnapshot{}, err
	}
	return SessionTreeSnapshot{Marker: marker, ParentCount: entries[0].Count, ParentVersion: entries[0].Version}, nil
}

// SessionTreeMarker encodes a parent and its direct children deterministically.
func SessionTreeMarker(entries []SessionTreeEntry) (string, error) {
	if len(entries) == 0 {
		return "", errors.New("session not found")
	}
	slices.SortFunc(entries[1:], func(a, b SessionTreeEntry) int { return strings.Compare(a.ID, b.ID) })
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v1:%x", sha256.Sum256(encoded)), nil
}
