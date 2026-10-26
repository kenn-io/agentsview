package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// pushSuffixBoundary compares the transcript in the snapshot used for copying.
// Result events belong to their call's ordinal.
func pushSuffixBoundary(ctx context.Context, snapshot *db.MessageSnapshot, sessionID string, remote db.MessageComparisonFields) (int, bool, error) {
	comparator := newPushSuffixRowComparator(remote)
	from := 0
	for {
		messages, err := snapshot.GetMessages(ctx, sessionID, from, db.MaxMessageLimit, true)
		if err != nil {
			return 0, false, fmt.Errorf("reading local suffix comparison: %w", err)
		}
		if len(messages) == 0 {
			break
		}
		for _, m := range messages {
			if boundary, changed := comparator.compare(m.Ordinal, db.MessageComparisonRow(m, pgPushTimestampFingerprintText)); changed {
				return boundary, true, nil
			}
		}
		from = messages[len(messages)-1].Ordinal + 1
	}
	return comparator.boundaryAtEnd(from), comparator.changedAtEnd(), nil
}

type pushSuffixRowComparator struct{ remaining [7]string }

func comparisonStreams(row db.MessageComparisonFields) [7]string {
	return [7]string{row.ContentHash, row.RoleTime, row.Flags, row.Dialogue, row.Token, row.ToolCalls, row.ToolResults}
}

func newPushSuffixRowComparator(remote db.MessageComparisonFields) *pushSuffixRowComparator {
	return &pushSuffixRowComparator{remaining: comparisonStreams(remote)}
}

// Each fragment starts with its message ordinal and a pipe. Consume complete
// local fragments, then detect remote children left at the same ordinal.
func (c *pushSuffixRowComparator) compare(ordinal int, row db.MessageComparisonFields) (int, bool) {
	for i, fragment := range comparisonStreams(row) {
		remote := c.remaining[i]
		if remote != "" {
			if next := comparisonStreamOrdinal(remote); next < ordinal {
				return next, true
			}
		}
		if !strings.HasPrefix(remote, fragment) {
			return ordinal, true
		}
		c.remaining[i] = remote[len(fragment):]
		if rest := c.remaining[i]; rest != "" && comparisonStreamOrdinal(rest) <= ordinal {
			return ordinal, true
		}
	}
	return 0, false
}

func comparisonStreamOrdinal(stream string) int {
	ordinal, _, _ := strings.Cut(stream, "|")
	value, _ := strconv.Atoi(ordinal)
	return value
}

func (c *pushSuffixRowComparator) boundaryAtEnd(localNext int) int {
	boundary := localNext
	for _, stream := range c.remaining {
		if stream != "" {
			boundary = min(boundary, comparisonStreamOrdinal(stream))
		}
	}
	return boundary
}

func (c *pushSuffixRowComparator) changedAtEnd() bool {
	return c.remaining != [7]string{}
}
