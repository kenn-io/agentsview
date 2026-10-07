package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// PRLink is a pull or merge request associated with a session. Parsers
// populate it from structured source records, so a full reparse replaces
// the stored list.
type PRLink struct {
	URL        string `json:"url"`
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Number     int    `json:"number"`
}

// PRLinksFromParsed converts parser links into storage rows.
func PRLinksFromParsed(links []parser.PRLink) []PRLink {
	if len(links) == 0 {
		return nil
	}
	out := make([]PRLink, 0, len(links))
	for _, l := range links {
		link := PRLink{
			URL: l.URL, Host: l.Host, Repository: l.Repository,
			Number: l.Number,
		}
		out = append(out, link)
	}
	return out
}

// EncodePRLinks returns the stored text form of links: a JSON array, or
// the empty string when there are none.
func EncodePRLinks(links []PRLink) string {
	if len(links) == 0 {
		return ""
	}
	b, err := json.Marshal(links)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodePRLinks parses the stored text form. Empty or malformed text
// decodes to nil so one damaged row cannot break a session listing.
func DecodePRLinks(text string) []PRLink {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var links []PRLink
	if err := json.Unmarshal([]byte(text), &links); err != nil {
		return nil
	}
	if len(links) == 0 {
		return nil
	}
	return links
}

// prLinksColumn scans the stored pr_links text into a slice.
type prLinksColumn struct{ dst *[]PRLink }

// PRLinksScanner scans pr_links text written by EncodePRLinks into dst, for
// mirrors that store the same encoding.
func PRLinksScanner(dst *[]PRLink) sql.Scanner { return prLinksColumn{dst} }

func (c prLinksColumn) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*c.dst = nil
	case string:
		*c.dst = DecodePRLinks(v)
	case []byte:
		*c.dst = DecodePRLinks(string(v))
	default:
		return fmt.Errorf("scanning pr_links: unsupported type %T", src)
	}
	return nil
}

// PRFilter selects sessions linked to a repository, optionally narrowed
// to one pull request number. Repository matching ignores case.
type PRFilter struct {
	Repository string
	Number     int
}

// IsZero reports whether the filter selects nothing.
func (f PRFilter) IsZero() bool { return f.Repository == "" }

// ErrInvalidPRFilter identifies a pull request filter ParsePRFilter rejects.
var ErrInvalidPRFilter = errors.New("invalid pr filter")

// ParsePRFilter accepts "owner/repo" or "owner/repo#123". An empty value returns the zero filter.
func ParsePRFilter(value string) (PRFilter, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return PRFilter{}, nil
	}
	repo, num, hasNum := strings.Cut(value, "#")
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" || !strings.Contains(repo, "/") || strings.Contains(repo, "://") {
		return PRFilter{}, fmt.Errorf(
			"%w %q: want owner/repo, owner/repo#123", ErrInvalidPRFilter, value,
		)
	}
	f := PRFilter{Repository: strings.ToLower(repo)}
	if hasNum {
		n, err := strconv.Atoi(strings.TrimSpace(num))
		if err != nil || n <= 0 {
			return PRFilter{}, fmt.Errorf(
				"%w %q: invalid pull request number", ErrInvalidPRFilter, value,
			)
		}
		f.Number = n
	}
	return f, nil
}

// labelsColumn scans a JSON array of labels into a slice.
type labelsColumn struct{ dst *[]string }

// LabelsScanner scans a JSON array of labels into dst. An empty array scans
// to nil, the value an unlabeled session carries on every backend.
func LabelsScanner(dst *[]string) sql.Scanner { return labelsColumn{dst} }

// LabelsArg returns labels as a non-nil slice for mirrors whose labels
// column is a non-null array, so an unlabeled session writes an empty array
// and fingerprints the same as one whose labels were all removed.
func LabelsArg(labels []string) []string {
	if labels == nil {
		return []string{}
	}
	return labels
}

func (c labelsColumn) Scan(src any) error {
	var text string
	switch v := src.(type) {
	case nil:
		*c.dst = nil
		return nil
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return fmt.Errorf("scanning labels: unsupported type %T", src)
	}
	var labels []string
	if err := json.Unmarshal([]byte(text), &labels); err != nil {
		return fmt.Errorf("scanning labels: %w", err)
	}
	if len(labels) == 0 {
		labels = nil
	}
	*c.dst = labels
	return nil
}

// GetSessionPRLinkURLs returns the pull request URLs stored for a session.
// A missing session yields an empty map.
func (db *DB) GetSessionPRLinkURLs(
	ctx context.Context, sessionID string,
) (map[string]struct{}, error) {
	var text string
	err := db.getReader().QueryRowContext(ctx,
		"SELECT pr_links FROM sessions WHERE id = ?", sessionID,
	).Scan(&text)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reading pr links for %s: %w", sessionID, err)
	}
	seen := make(map[string]struct{})
	for _, link := range DecodePRLinks(text) {
		seen[link.URL] = struct{}{}
	}
	return seen, nil
}
