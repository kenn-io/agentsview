package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

const (
	RecallListCursorRecency = "recency"
	RecallListCursorRanked  = "ranked"
)

type RecallListCursor struct {
	Kind       string `json:"kind"`
	UpdatedAt  string `json:"updated_at"`
	ID         string `json:"id"`
	Offset     int    `json:"offset,omitempty"`
	Revision   string `json:"revision,omitempty"`
	FilterHash string `json:"filter_hash"`
}

// ErrStaleRecallPagination reports that the corpus changed between or during
// the pages of one walk. A cursor cannot say which entries were missed, so the
// caller starts over from the first page with the walk's original bound.
var ErrStaleRecallPagination = errors.New(
	"recall corpus changed during pagination; restart from the first page " +
		"with the same updated_since bound, and keep the saved checkpoint " +
		"until the last page returns",
)

// RecallQueryRevisionProvider reports the revision that changes with every
// Recall entry or evidence write. An empty revision marks a read-only archive.
type RecallQueryRevisionProvider interface {
	RecallQueryRevision(context.Context) (string, error)
}

// ListRecentRecallEntriesPage returns one recency page. Time-bounded pages
// carry the corpus revision in their cursor, because a write that shares the
// cursor's millisecond can sort behind it and be skipped.
func ListRecentRecallEntriesPage(
	ctx context.Context,
	store interface {
		ListRecallEntries(context.Context, db.RecallQuery) ([]db.RecallEntry, error)
	},
	query db.RecallQuery,
	cursor *RecallListCursor,
) ([]db.RecallResult, string, error) {
	pageLimit := db.RecallLimit(query.Limit)
	bounded := query.TimeBounded()
	revisions, tracked := store.(RecallQueryRevisionProvider)
	tracked = tracked && bounded
	var revision string
	if tracked {
		var err error
		revision, err = revisions.RecallQueryRevision(ctx)
		if err != nil {
			return nil, "", err
		}
	}
	if cursor != nil {
		if cursor.Kind != RecallListCursorRecency {
			return nil, "", db.ErrInvalidRecallQuery
		}
		if bounded && cursor.Revision != revision {
			return nil, "", ErrStaleRecallPagination
		}
		query.CursorUpdatedAt = cursor.UpdatedAt
		query.CursorID = cursor.ID
	}
	query.ProbeNext = true
	entries, err := store.ListRecallEntries(ctx, query)
	if err != nil {
		return nil, "", err
	}
	if tracked {
		current, err := revisions.RecallQueryRevision(ctx)
		if err != nil {
			return nil, "", err
		}
		if current != revision {
			return nil, "", ErrStaleRecallPagination
		}
	}
	if entries == nil {
		entries = []db.RecallEntry{}
	}
	hasMore := len(entries) > pageLimit
	if hasMore {
		entries = entries[:pageLimit]
	}
	results := make([]db.RecallResult, 0, len(entries))
	for _, entry := range entries {
		results = append(results, db.RecallResult{RecallEntry: entry})
	}
	if !hasMore {
		return results, "", nil
	}
	if bounded && !tracked {
		return nil, "", fmt.Errorf(
			"%w: this store cannot page a time-bounded list safely; "+
				"narrow the bounds or filters to fit one page",
			db.ErrInvalidRecallQuery,
		)
	}
	last := entries[len(entries)-1]
	nextCursor := EncodeRecallListCursor(RecallListCursor{
		Kind:       RecallListCursorRecency,
		UpdatedAt:  last.UpdatedAt,
		ID:         last.ID,
		Revision:   revision,
		FilterHash: RecallListFilterHash(query),
	})
	return results, nextCursor, nil
}

func RecallListFilterHash(query db.RecallQuery) string {
	query.CursorUpdatedAt = ""
	query.CursorID = ""
	query.ProbeNext = false
	data, _ := json.Marshal(query)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func EncodeRecallListCursor(cursor RecallListCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func DecodeRecallListCursor(raw string) (RecallListCursor, error) {
	var cursor RecallListCursor
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor, err
	}
	if err := json.Unmarshal(
		data, &cursor, json.RejectUnknownMembers(true),
	); err != nil {
		return cursor, err
	}
	if cursor.FilterHash == "" {
		return RecallListCursor{}, db.ErrInvalidRecallQuery
	}
	switch cursor.Kind {
	case RecallListCursorRecency:
		if cursor.UpdatedAt == "" || cursor.ID == "" || cursor.Offset != 0 {
			return RecallListCursor{}, db.ErrInvalidRecallQuery
		}
	case RecallListCursorRanked:
		if cursor.UpdatedAt != "" || cursor.ID != "" ||
			cursor.Offset <= 0 ||
			cursor.Offset >= db.MaxRecallEntryLimit ||
			cursor.Revision == "" {
			return RecallListCursor{}, db.ErrInvalidRecallQuery
		}
	default:
		return RecallListCursor{}, db.ErrInvalidRecallQuery
	}
	return cursor, nil
}
