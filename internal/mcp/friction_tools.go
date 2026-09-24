package mcp

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/agentsview/internal/service"
)

const (
	defaultFrictionPatternLimit = 20
	maxFrictionPatternLimit     = 100
)

// --- get_friction_digest ---

type frictionDigestIn struct {
	Date   string `json:"date,omitempty" jsonschema:"Digest date (YYYY-MM-DD). Defaults to the latest digest."`
	Format string `json:"format,omitempty" jsonschema:"markdown (default) or summary (the counts-only JSON object)."`
}

type frictionDigestOut struct {
	Date     string         `json:"date"`
	Timezone string         `json:"timezone"`
	Revision int            `json:"revision"`
	Format   string         `json:"format"`
	Markdown string         `json:"markdown,omitempty"`
	Summary  map[string]any `json:"summary,omitempty"`
	WebURL   string         `json:"web_url,omitempty" jsonschema:"Browser URL for this digest; use this URL when linking to it."`
}

func (t *toolset) frictionDigest(
	ctx context.Context, _ *mcp.CallToolRequest, in frictionDigestIn,
) (*mcp.CallToolResult, frictionDigestOut, error) {
	fs, ok := t.svc.(service.FrictionService)
	if !ok {
		return nil, frictionDigestOut{}, errors.New("friction digests are not available from this backend")
	}
	format := cmp.Or(in.Format, "markdown")
	if format != "markdown" && format != "summary" {
		return nil, frictionDigestOut{}, errors.New("format must be markdown or summary")
	}
	view, err := fs.FrictionDigest(ctx, in.Date)
	if errors.Is(err, service.ErrFrictionDigestNotFound) {
		if in.Date == "" {
			return nil, frictionDigestOut{}, errors.New("no friction digests have been built yet")
		}
		return nil, frictionDigestOut{}, fmt.Errorf("no friction digest for %s", in.Date)
	}
	if err != nil {
		return nil, frictionDigestOut{}, err
	}
	out := frictionDigestOut{
		Date: view.Date, Timezone: view.Timezone, Revision: view.Revision,
		Format: format, WebURL: view.WebURL,
	}
	if format == "markdown" {
		out.Markdown = view.Markdown
		return nil, out, nil
	}
	if err := json.Unmarshal(view.Summary, &out.Summary); err != nil {
		return nil, frictionDigestOut{}, fmt.Errorf("decoding friction summary: %w", err)
	}
	return nil, out, nil
}

// --- list_friction_patterns ---

type listFrictionPatternsIn struct {
	Kind   string `json:"kind,omitempty" jsonschema:"correction, error, workaround, deferral, pattern, frustration or interruption."`
	Since  string `json:"since,omitempty" jsonschema:"Only patterns last seen on or after this date (YYYY-MM-DD)."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum patterns (default 20, max 100)."`
	Linked *bool  `json:"linked,omitempty" jsonschema:"true: only patterns linked to a Kata issue; false: only unlinked."`
}

type frictionPatternOut struct {
	Fingerprint     string `json:"fingerprint"`
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	FirstSeenDate   string `json:"first_seen_date"`
	LastSeenDate    string `json:"last_seen_date"`
	OccurrenceCount int    `json:"occurrence_count"`
	SessionCount    int    `json:"session_count"`
	LastSessionID   string `json:"last_session_id"`
	LastOrdinal     *int   `json:"last_ordinal,omitempty"`
	WebURL          string `json:"web_url,omitempty" jsonschema:"Browser URL for the most recent occurrence; use this URL when linking to it."`
	KataQualifiedID string `json:"kata_qualified_id,omitempty"`
	KataWebURL      string `json:"kata_web_url,omitempty"`
}

type listFrictionPatternsOut struct {
	Patterns []frictionPatternOut `json:"patterns"`
}

func (t *toolset) listFrictionPatterns(
	ctx context.Context, _ *mcp.CallToolRequest, in listFrictionPatternsIn,
) (*mcp.CallToolResult, listFrictionPatternsOut, error) {
	fs, ok := t.svc.(service.FrictionService)
	if !ok {
		return nil, listFrictionPatternsOut{}, errors.New("friction patterns are not available from this backend")
	}
	views, err := fs.FrictionPatterns(ctx, service.FrictionPatternFilter{
		Kind: in.Kind, Since: in.Since, Linked: in.Linked,
		Limit: clampLimit(in.Limit, defaultFrictionPatternLimit, maxFrictionPatternLimit),
	})
	if err != nil {
		return nil, listFrictionPatternsOut{}, err
	}
	out := listFrictionPatternsOut{Patterns: make([]frictionPatternOut, 0, len(views))}
	for _, v := range views {
		out.Patterns = append(out.Patterns, frictionPatternOut{
			Fingerprint: v.Fingerprint, Kind: v.Kind, Title: v.Title,
			FirstSeenDate: v.FirstSeenDate, LastSeenDate: v.LastSeenDate,
			OccurrenceCount: v.OccurrenceCount, SessionCount: v.SessionCount,
			LastSessionID: v.LastSubjectID, LastOrdinal: v.LastOrdinal,
			WebURL: v.LastSessionURL, KataQualifiedID: v.IssueQualifiedID,
			KataWebURL: v.IssueWebURL,
		})
	}
	return nil, out, nil
}
