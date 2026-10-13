package codexcloud

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
)

// listPageSize is the thread/list page size the probes used.
const listPageSize = 50

// subAgentSourceKinds selects sub-agent threads, which the default listing
// may leave out.
var subAgentSourceKinds = []string{
	"subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther",
}

// Listings are the four listings each pass reads.
var Listings = []ListingID{
	ListDefaultActive, ListDefaultArchived, ListSubAgentActive, ListSubAgentArchived,
}

// ListedThread is one thread/list row. Raw is the server's whole thread
// object, stored as the thread metadata record.
type ListedThread struct {
	ID        string
	UpdatedAt int64
	Status    string
	Raw       jsontext.Value
}

// ListingResult is what one listing's pass found.
type ListingResult struct {
	// Changed holds threads whose updatedAt differs from the stored value.
	Changed []ListedThread
	// Active holds every walked thread the listing shows as active.
	Active []string
	// Newest is the newest updatedAt the listing returned.
	Newest int64
}

type listParams struct {
	Limit         int            `json:"limit"`
	Cursor        jsontext.Value `json:"cursor,omitzero"`
	SortKey       string         `json:"sortKey"`
	SortDirection string         `json:"sortDirection"`
	SourceKinds   []string       `json:"sourceKinds,omitzero"`
	Archived      bool           `json:"archived"`
}

type listPage struct {
	Data       []jsontext.Value `json:"data"`
	NextCursor jsontext.Value   `json:"nextCursor"`
}

type listRow struct {
	ID        string   `json:"id"`
	UpdatedAt *float64 `json:"updatedAt"`
	Status    struct {
		Type string `json:"type"`
	} `json:"status"`
}

// listChanged walks one listing newest first. With a watermark it stops
// after the first page holding a thread strictly older than it; updatedAt
// has whole-second resolution, so finishing that page covers every thread
// sharing the watermark's second. stored returns a thread's stored
// updatedAt. Rows out of updatedAt order are a protocol change, since the
// early stop is safe only if the server really sorts that way.
func listChanged(
	ctx context.Context, c *Conn, id ListingID, watermark int64,
	stored func(threadID string) (int64, bool, error),
) (ListingResult, error) {
	params := listParams{
		Limit: listPageSize, SortKey: "updated_at", SortDirection: "desc",
		// The hosted server rejects modelProviders, so the listing is
		// whatever its default provider filter returns.
		Archived: id == ListDefaultArchived || id == ListSubAgentArchived,
	}
	if id == ListSubAgentActive || id == ListSubAgentArchived {
		params.SourceKinds = subAgentSourceKinds
	}
	var result ListingResult
	previous := int64(math.MaxInt64)
	for {
		var page listPage
		if err := c.Call(ctx, "thread/list", params, &page); err != nil {
			return ListingResult{}, err
		}
		older := false
		for _, raw := range page.Data {
			var row listRow
			if err := json.Unmarshal(raw, &row); err != nil {
				return ListingResult{}, fmt.Errorf("%w: unreadable thread/list row: %w", ErrProtocolChange, err)
			}
			if row.ID == "" || row.UpdatedAt == nil {
				return ListingResult{}, fmt.Errorf("%w: thread/list row without id or updatedAt", ErrProtocolChange)
			}
			updated := int64(math.Floor(*row.UpdatedAt))
			if updated > previous {
				return ListingResult{}, fmt.Errorf("%w: thread/list %s is not sorted by updatedAt", ErrProtocolChange, id)
			}
			previous = updated
			result.Newest = max(result.Newest, updated)
			if watermark > 0 && updated < watermark {
				older = true
			}
			if row.Status.Type == "active" {
				result.Active = append(result.Active, row.ID)
			}
			have, ok, err := stored(row.ID)
			if err != nil {
				return ListingResult{}, err
			}
			if !ok || have != updated {
				result.Changed = append(result.Changed, ListedThread{
					ID: row.ID, UpdatedAt: updated, Status: row.Status.Type, Raw: raw,
				})
			}
		}
		if older || len(page.Data) == 0 || len(page.NextCursor) == 0 || page.NextCursor.Kind() == 'n' {
			return result, nil
		}
		params.Cursor = page.NextCursor
	}
}
