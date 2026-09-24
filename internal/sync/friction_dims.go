package sync

import (
	"context"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/nanoclaw"
)

// friction_session_dims.dims_source values (spec §5.3).
const (
	frictionDimsNanoClaw    = "nanoclaw"
	frictionDimsSeatPattern = "seat_pattern"
)

// NewFrictionDims builds the FrictionDimsFunc from the configured fleet
// sources (spec §11). Nothing has a default (D33): with no resolver and no
// seat patterns it returns nil, and friction compute writes no dims row.
func NewFrictionDims(resolver *nanoclaw.Resolver, seats []friction.SeatPattern) FrictionDimsFunc {
	if resolver == nil && len(seats) == 0 {
		return nil
	}
	return func(ctx context.Context, s db.Session) (db.FrictionSessionDims, bool) {
		filePath := ""
		if s.FilePath != nil {
			filePath = *s.FilePath
		}
		d := resolveFleetDims(ctx, resolver, seats, filePath)
		return d, d.DimsSource != ""
	}
}

// resolveFleetDims answers for one session source path. Persona and channel
// are stored display-sanitized, so only those forms leave the host through
// push (spec §11.1). An excluded session carries no names.
func resolveFleetDims(
	ctx context.Context, resolver *nanoclaw.Resolver,
	seats []friction.SeatPattern, filePath string,
) db.FrictionSessionDims {
	var d db.FrictionSessionDims
	if filePath == "" {
		return d
	}
	var sources []string
	if resolver != nil {
		if persona, channel, excluded, ok := resolver.Resolve(ctx, filePath); ok {
			d.Persona = friction.SanitizeDisplay(persona)
			d.Channel = friction.SanitizeDisplay(channel)
			d.ReviewExcluded = excluded
			sources = append(sources, frictionDimsNanoClaw)
		}
	}
	if seat := friction.SeatFromPath(filePath, seats); seat != "" {
		d.Seat = seat
		sources = append(sources, frictionDimsSeatPattern)
	}
	d.DimsSource = strings.Join(sources, "+")
	return d
}
