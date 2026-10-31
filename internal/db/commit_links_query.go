package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"sort"
	"strings"
	"time"
)

// ValidateCommitSHAs rejects the entire batch before any database operation.
func ValidateCommitSHAs(shas []string) error {
	if len(shas) > MaxCommitSHAs {
		return ErrInvalidCommitQuery
	}
	for _, sha := range shas {
		if len(sha) < 7 || len(sha) > 40 {
			return ErrInvalidCommitQuery
		}
		for _, c := range sha {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return ErrInvalidCommitQuery
			}
		}
	}
	return nil
}

// CommitSHAMatches compares retained evidence without claiming Git-wide uniqueness.
func CommitSHAMatches(link CommitLink, query string) bool {
	q := strings.ToLower(query)
	s := strings.ToLower(link.CommitSHA)
	return strings.HasPrefix(s, q) || !link.Resolved && strings.HasPrefix(q, s)
}

// BuildCommitLinksPredicate is shared by archive and replica readers.
func BuildCommitLinksPredicate(b *QueryBuilder, shas []string, sessionID string) string {
	if sessionID != "" {
		return "session_id = " + b.Add(sessionID)
	}
	if len(shas) == 0 {
		return "1=0"
	}
	var terms []string
	seen := map[string]bool{}
	prefixes := map[string]bool{}
	for _, raw := range shas {
		sha := strings.ToLower(raw)
		if seen[sha] {
			continue
		}
		seen[sha] = true
		for n := 7; n < len(sha); n++ {
			prefixes[sha[:n]] = true
		}
		terms = append(terms, "(commit_sha >= "+b.Add(sha)+" AND commit_sha < "+b.Add(sha+"g")+")")
	}
	if len(prefixes) > 0 {
		keys := make([]string, 0, len(prefixes))
		for key := range prefixes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ph := make([]string, 0, len(keys))
		for _, key := range keys {
			ph = append(ph, b.Add(key))
		}
		terms = append(terms, "commit_sha IN ("+strings.Join(ph, ",")+")")
	}
	return "(" + strings.Join(terms, " OR ") + ")"
}

// ScanCommitLinkPayloads bounds bytes and rows before decoding candidate payloads.
// Rows contain one JSON wire card each. A server may not silently truncate evidence.
func ScanCommitLinkPayloads(rows *sql.Rows) ([]CommitLink, error) {
	out := []CommitLink{}
	bytes := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if raw == "index-not-ready" {
			return nil, ErrCommitIndexUnavailable
		}
		bytes += len(raw)
		if raw == "" || len(out) >= MaxCommitLinks || bytes > MaxCommitResponseBytes || len(raw) > 64<<10 {
			return nil, ErrCommitLookupTooLarge
		}
		var row CommitLink
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SortCommitLinks gives every backend the same total ordering, including missing times.
func SortCommitLinks(rows []CommitLink) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		ta, ea := time.Parse(time.RFC3339Nano, a.CommittedAt)
		tb, eb := time.Parse(time.RFC3339Nano, b.CommittedAt)
		if (ea == nil) != (eb == nil) {
			return ea != nil
		}
		if ea == nil {
			if !ta.Equal(tb) {
				return ta.Before(tb)
			}
		} else if a.CommittedAt != b.CommittedAt {
			return a.CommittedAt < b.CommittedAt
		}
		for _, pair := range [][2]string{{a.CommitSHA, b.CommitSHA}, {a.SessionID, b.SessionID}} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		if a.Source != b.Source {
			return a.Source == "transcript"
		}
		for _, pair := range [][2]*int{{a.MessageOrdinal, b.MessageOrdinal}, {a.CallIndex, b.CallIndex}} {
			x, y := -1, -1
			if pair[0] != nil {
				x = *pair[0]
			}
			if pair[1] != nil {
				y = *pair[1]
			}
			if x != y {
				return x < y
			}
		}
		return a.RepositoryPath < b.RepositoryPath
	})
}

// MarkCommitAmbiguity flags unresolved evidence and known full-SHA collisions.
func MarkCommitAmbiguity(rows []CommitLink, shas []string) {
	for i := range rows {
		if !rows[i].Resolved {
			rows[i].Ambiguous = true
		}
	}
	for _, sha := range shas {
		full := map[string]bool{}
		for _, row := range rows {
			if row.Resolved && CommitSHAMatches(row, sha) {
				full[row.CommitSHA] = true
			}
		}
		if len(full) > 1 {
			for i := range rows {
				if CommitSHAMatches(rows[i], sha) {
					rows[i].Ambiguous = true
				}
			}
		}
	}
}

type commitByteBudget struct{ remaining int }

func (b *commitByteBudget) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		return 0, ErrCommitLookupTooLarge
	}
	b.remaining -= len(p)
	return len(p), nil
}

// CheckCommitResponseSize counts encoded bytes without buffering the response.
func CheckCommitResponseSize(value any) error {
	err := json.MarshalWrite(&commitByteBudget{remaining: MaxCommitResponseBytes}, value)
	if errors.Is(err, ErrCommitLookupTooLarge) {
		return ErrCommitLookupTooLarge
	}
	return err
}

// ReadCommitLinkPayloads supplies shared, bounded replica reads. Publication guards
// belong in relation; it must be a trusted constant supplied by the backend.
func ReadCommitLinkPayloads(ctx context.Context, query func(context.Context, string, ...any) (*sql.Rows, error), dialect QueryDialect, relation string, shas []string, id string) ([]CommitLink, error) {
	if err := ValidateCommitSHAs(shas); err != nil {
		return nil, err
	}
	if id == "" && len(shas) == 0 {
		return []CommitLink{}, nil
	}
	b := NewQueryBuilder(dialect, 0)
	pred := BuildCommitLinksPredicate(b, shas, id)
	rows, err := query(ctx, "SELECT CASE WHEN length(payload)>65536 THEN '' ELSE payload END FROM "+relation+" WHERE "+pred+" LIMIT 4097", b.Args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links, err := ScanCommitLinkPayloads(rows)
	if err != nil {
		return nil, err
	}
	if len(shas) == 1 {
		MarkCommitAmbiguity(links, shas)
	} else {
		MarkCommitAmbiguity(links, nil)
	}
	SortCommitLinks(links)
	return links, nil
}

var _ io.Writer = (*commitByteBudget)(nil)
