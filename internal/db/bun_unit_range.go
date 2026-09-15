package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
)

type bunUnitBoundsQuerier struct {
	store  bun.IDB
	parent *BunStore
}

var _ UnitBoundsQuerier = (*BunStore)(nil)

// NearestUserBoundaries resolves canonical message boundaries inside one
// guarded backend view; adapters only supply their search-prefix capability.
func (s *BunStore) NearestUserBoundaries(
	ctx context.Context, probes []UnitProbe,
) ([]UnitBounds, error) {
	var bounds []UnitBounds
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		bounds, err = (bunUnitBoundsQuerier{store: store, parent: s}).
			NearestUserBoundaries(ctx, probes)
		return err
	})
	return bounds, err
}

// RunExtents resolves canonical assistant runs inside one guarded view.
func (s *BunStore) RunExtents(
	ctx context.Context, probes []ExtentProbe,
) ([][2]int, error) {
	var extents [][2]int
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		extents, err = (bunUnitBoundsQuerier{store: store, parent: s}).
			RunExtents(ctx, probes)
		return err
	})
	return extents, err
}

func (q bunUnitBoundsQuerier) NearestUserBoundaries(
	ctx context.Context, probes []UnitProbe,
) ([]UnitBounds, error) {
	return ResolveUserBoundaries(ctx, probes, unitSessionChunk,
		func(ctx context.Context, sessions []string, out [][]int) error {
			values := make([]string, len(sessions))
			args := make([]any, 0, len(sessions)*2)
			for i, sessionID := range sessions {
				values[i] = "(?, ?)"
				args = append(args, i, sessionID)
			}
			predicate := fmt.Sprintf(
				"%[1]s.role = 'user' AND %[1]s.is_system = FALSE AND %[2]s",
				"message", q.parent.bunContentSystemPrefixSQL(
					"message.content", "message.role",
				),
			)
			query := `WITH spans(idx, session_id) AS (VALUES ` +
				strings.Join(values, ", ") + `)
				SELECT spans.idx, message.ordinal
				FROM spans JOIN messages AS message
					ON message.session_id = spans.session_id
				WHERE ` + predicate
			var rows []struct {
				Index   int `bun:"idx"`
				Ordinal int `bun:"ordinal"`
			}
			if err := q.store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
				return fmt.Errorf("querying Bun user boundaries: %w", err)
			}
			for _, row := range rows {
				if row.Index < 0 || row.Index >= len(out) {
					return fmt.Errorf("bun user boundary index %d out of range", row.Index)
				}
				out[row.Index] = append(out[row.Index], row.Ordinal)
			}
			return nil
		},
	)
}

func (q bunUnitBoundsQuerier) RunExtents(
	ctx context.Context, probes []ExtentProbe,
) ([][2]int, error) {
	return ResolveRunExtents(ctx, probes, unitExtentChunk,
		func(ctx context.Context, chunk []ExtentProbe, out [][2]int) error {
			values := make([]string, len(chunk))
			args := make([]any, 0, len(chunk)*6)
			for i, probe := range chunk {
				values[i] = "(?, ?, ?, ?, ?, ?)"
				args = append(args, i, probe.SessionID, probe.Ordinal,
					probe.Lo, probe.Hi, probe.Sidechain)
			}
			user := fmt.Sprintf(
				"%[1]s.role = 'user' AND %[1]s.is_system = FALSE AND %[2]s",
				"f", q.parent.bunContentSystemPrefixSQL("f.content", "f.role"),
			)
			stop := "((f.role = 'assistant' AND f.is_system = FALSE " +
				"AND f.is_sidechain <> p.sc) OR (" + user + "))"
			query := `WITH probes(idx, session_id, o, lo, hi, sc) AS (VALUES ` +
				strings.Join(values, ", ") + `)
				SELECT p.idx,
				  (SELECT m.ordinal FROM messages m
				   WHERE m.session_id = p.session_id AND m.ordinal <= p.o
				     AND m.ordinal > COALESCE((SELECT f.ordinal FROM messages f
				       WHERE f.session_id = p.session_id
				         AND f.ordinal > p.lo AND f.ordinal < p.o AND ` + stop + `
				       ORDER BY f.ordinal DESC LIMIT 1), p.lo)
				     AND m.role = 'assistant' AND m.is_system = FALSE
				     AND m.is_sidechain = p.sc
				   ORDER BY m.ordinal ASC LIMIT 1) AS first_ordinal,
				  (SELECT m.ordinal FROM messages m
				   WHERE m.session_id = p.session_id AND m.ordinal >= p.o
				     AND m.ordinal < COALESCE((SELECT f.ordinal FROM messages f
				       WHERE f.session_id = p.session_id
				         AND f.ordinal > p.o AND f.ordinal < p.hi AND ` + stop + `
				       ORDER BY f.ordinal ASC LIMIT 1), p.hi)
				     AND m.role = 'assistant' AND m.is_system = FALSE
				     AND m.is_sidechain = p.sc
				   ORDER BY m.ordinal DESC LIMIT 1) AS last_ordinal
				FROM probes p`
			var rows []struct {
				Index int  `bun:"idx"`
				First *int `bun:"first_ordinal"`
				Last  *int `bun:"last_ordinal"`
			}
			if err := q.store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
				return fmt.Errorf("querying Bun run extents: %w", err)
			}
			if len(rows) != len(chunk) {
				return fmt.Errorf("bun run extents returned %d rows for %d probes",
					len(rows), len(chunk))
			}
			for _, row := range rows {
				if row.Index < 0 || row.Index >= len(out) || row.First == nil || row.Last == nil {
					return fmt.Errorf("invalid Bun run extent at index %d", row.Index)
				}
				out[row.Index] = [2]int{*row.First, *row.Last}
			}
			return nil
		},
	)
}
