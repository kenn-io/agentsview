package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/agentsview/internal/db"
)

var ErrStaleRecallPagination = errors.New("recall corpus changed; restart pagination")

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

func ListRecentRecallEntriesPage(
	ctx context.Context,
	store interface {
		ListRecallEntries(context.Context, db.RecallQuery) ([]db.RecallEntry, error)
	},
	query db.RecallQuery,
	pageLimit int,
	cursor *RecallListCursor,
) ([]db.RecallResult, string, error) {
	pageLimit = db.RecallLimit(pageLimit)
	var revision string
	var revisionProvider interface {
		RecallQueryRevision(context.Context) (string, error)
	}
	if query.UpdatedSince != "" {
		revisionProvider, _ = store.(interface {
			RecallQueryRevision(context.Context) (string, error)
		})
		if revisionProvider != nil {
			var err error
			revision, err = revisionProvider.RecallQueryRevision(ctx)
			if err != nil {
				return nil, "", err
			}
		}
		if cursor != nil {
			if revision == "" {
				return nil, "", db.ErrInvalidRecallQuery
			}
			if cursor.Revision != revision {
				return nil, "", ErrStaleRecallPagination
			}
		}
	}
	if cursor != nil {
		if cursor.Kind != RecallListCursorRecency {
			return nil, "", db.ErrInvalidRecallQuery
		}
		query.CursorUpdatedAt = cursor.UpdatedAt
		query.CursorID = cursor.ID
	}
	query.ProbeNext = true
	entries, err := store.ListRecallEntries(ctx, query)
	if err != nil {
		return nil, "", err
	}
	if revision != "" {
		current, err := revisionProvider.RecallQueryRevision(ctx)
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
	if !hasMore || (query.UpdatedSince != "" && revision == "") {
		return results, "", nil
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
		if cursor.UpdatedAt == "" || cursor.ID == "" ||
			cursor.Offset != 0 {
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
