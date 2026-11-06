package db

import (
	"context"
	"fmt"
	"strings"
)

// searchRankWalkMaxSessions bounds the sessions a walk binds into the page
// query, four parameters each, well under SQLite's 32,766-parameter limit.
// Deeper pages rank every match inside the query instead.
const searchRankWalkMaxSessions = 4096

// searchRankWalkRowsPerSession bounds the matches a walk reads for each
// session the page needs. A 25-session page of a common word measured about
// 35 rows. Matches that tie on rank across many sessions, such as identical
// short messages, would make it read nearly every match; past the bound it
// hands the page to the query that ranks every match.
const searchRankWalkRowsPerSession = 64

// searchBestMatch is a session's best message match: the lowest rank, then
// the lowest ordinal, then the lowest FTS rowid.
type searchBestMatch struct {
	sessionID string
	rowid     int64
	ordinal   int
	rank      float64
}

func (m searchBestMatch) before(o searchBestMatch) bool {
	if m.rank != o.rank {
		return m.rank < o.rank
	}
	if m.ordinal != o.ordinal {
		return m.ordinal < o.ordinal
	}
	return m.rowid < o.rowid
}

// searchRankWalk holds the best match of every session the walk reached.
// complete reports that the walk read every match.
type searchRankWalk struct {
	best     []searchBestMatch
	complete bool
}

// walkSearchRanks reads the eligible message matches in rank order and
// stops once it has seen need sessions and every match tied with the rank
// at which it saw the last of them. Every session whose best match ranks
// at or below that boundary then has its best match among the rows read,
// and every other session ranks after them, so the first need sessions of
// the full ordering are among the walk's sessions. FTS5 sorts all matches
// by rank before the first row, but joins each match to its message and
// session only as the walk pulls it. The walk returns nil when it reaches
// more sessions than the page query can bind or reads too many matches.
func (db *DB) walkSearchRanks(
	ctx context.Context, table, innerWhereSQL string, ftsArgs []any, need int,
) (*searchRankWalk, error) {
	if need > searchRankWalkMaxSessions {
		return nil, nil
	}
	query := strings.ReplaceAll(`SELECT m2.session_id, messages_fts.rowid, m2.ordinal, rank
		FROM messages_fts
		JOIN messages m2 ON messages_fts.rowid = m2.id
		JOIN sessions s2 ON m2.session_id = s2.id
		WHERE `+innerWhereSQL+`
		ORDER BY rank`, "messages_fts", table)
	rows, err := db.getReader().QueryContext(ctx, query, ftsArgs...)
	if err != nil {
		return nil, fmt.Errorf("ranking search matches: %w", err)
	}
	defer rows.Close()
	walk := &searchRankWalk{complete: true}
	index := make(map[string]int, need)
	var boundary float64
	maxRows := need * searchRankWalkRowsPerSession
	for read := 0; rows.Next(); read++ {
		if read == maxRows {
			return nil, nil
		}
		var m searchBestMatch
		if err := rows.Scan(&m.sessionID, &m.rowid, &m.ordinal, &m.rank); err != nil {
			return nil, fmt.Errorf("scanning ranked search match: %w", err)
		}
		if len(walk.best) >= need && m.rank > boundary {
			walk.complete = false
			break
		}
		i, ok := index[m.sessionID]
		if !ok {
			if len(walk.best) == searchRankWalkMaxSessions {
				return nil, nil
			}
			index[m.sessionID] = len(walk.best)
			walk.best = append(walk.best, m)
			if len(walk.best) == need {
				boundary = m.rank
			}
			continue
		}
		if m.before(walk.best[i]) {
			walk.best[i] = m
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading ranked search matches: %w", err)
	}
	return walk, nil
}

// bestRowsSQL renders the walk's best matches as the best_rows CTE, opening
// the query's WITH clause, with its bind arguments.
func (w *searchRankWalk) bestRowsSQL() (string, []any) {
	if len(w.best) == 0 {
		return `WITH best_rows(session_id, best_rowid, best_ordinal, best_rank) AS (
			SELECT '', 0, 0, 0.0 WHERE 0
		),
		`, nil
	}
	values := make([]string, len(w.best))
	args := make([]any, 0, 4*len(w.best))
	for i, m := range w.best {
		values[i] = "(?, ?, ?, ?)"
		args = append(args, m.sessionID, m.rowid, m.ordinal, m.rank)
	}
	return "WITH best_rows(session_id, best_rowid, best_ordinal, best_rank) AS (VALUES " +
		strings.Join(values, ", ") + "),\n\t\t", args
}
