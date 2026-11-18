package duckdb

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// Preserve non-nil empty optional strings in the private cache representation.
var analyticsMarshalers = json.MarshalToFunc(func(out *jsontext.Encoder, session db.TopSession) error {
	type topSession db.TopSession
	return json.MarshalEncode(out, struct {
		topSession
		DisplayName       *string `json:"display_name,omitzero"`
		StartedAt         *string `json:"started_at,omitzero"`
		EndedAt           *string `json:"ended_at,omitzero"`
		TerminationStatus *string `json:"termination_status,omitzero"`
	}{topSession(session), session.DisplayName, session.StartedAt, session.EndedAt, session.TerminationStatus})
})

func keptAnalytics[T any](ctx context.Context, s *Store, panel string, f db.AnalyticsFilter, variant string, load func(context.Context) (T, error)) (T, error) {
	version := s.keptVersion()
	if version == "" || f.ActiveSince != "" || db.TerminationUsesTime(f.Termination) {
		return load(ctx)
	}
	key, err := json.Marshal(struct {
		Panel   string
		Filter  db.AnalyticsFilter
		Variant string
	}{panel, f, variant})
	if err != nil {
		var zero T
		return zero, fmt.Errorf("encoding analytics selection: %w", err)
	}
	var savedVersion string
	var handle *sql.DB
	if db.SavedPanel(panel) {
		savedVersion, handle = s.dashboardDigest(ctx, panel, f)
	}
	if savedVersion != "" {
		savedVersion += "|" + db.TrendsTimezoneKey(f)
	}
	encoded, err := s.analyticsReads.DoSaved(ctx, &s.usageDisk, string(key), version, savedVersion, func(b []byte) ([]byte, error) {
		s.handleMu.RLock()
		current := s.duck == handle
		s.handleMu.RUnlock()
		if !current {
			return nil, errors.New("mirror changed during analytics read")
		}
		return b, nil
	}, func(b []byte) ([]byte, error) {
		var result T
		return b, json.Unmarshal(b, &result)
	}, func(ctx context.Context) ([]byte, error) {
		value, err := load(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true), json.WithMarshalers(analyticsMarshalers))
	})
	var result T
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, fmt.Errorf("decoding kept analytics: %w", err)
	}
	return result, nil
}

func (s *Store) dashboardDigest(ctx context.Context, panel string, f db.AnalyticsFilter) (string, *sql.DB) {
	s.handleMu.RLock()
	handle := s.duck
	s.handleMu.RUnlock()
	if handle == nil {
		return "", nil
	}
	type component struct {
		name, table string
		columns     []string
	}
	components := []component{{"sessions", "sessions", db.DashboardSessionInputs}}
	switch panel {
	case "tools":
		components = append(components,
			component{"tools_sessions", "sessions", db.DashboardToolsSessionInputs},
			component{"tools_messages", "messages", append([]string{"id"}, db.DashboardToolsMessageInputs...)},
			component{"tools_calls", "tool_calls", append([]string{"message_id"}, db.DashboardToolsCallInputs...)})
	case "velocity":
		components = append(components,
			component{"velocity_messages", "messages", append([]string{"id"}, db.DashboardVelocityMessageInputs...)},
			component{"velocity_calls", "tool_calls", append([]string{"message_id"}, db.DashboardVelocityCallInputs...)})
	case "signals":
		components = append(components, component{"signals_sessions", "sessions", append([]string{"frustration_marker_count"}, db.DashboardSignalsSessionInputs...)})
	}
	if db.DashboardUsesMessageScope(f) {
		components = append(components, component{"scope", "messages", db.DashboardScopeMessageInputs})
	}
	version := fmt.Sprintf("%p", handle)
	parts := make([]string, 0, len(components))
	for _, c := range components {
		digest, err := s.analyticsDigests.Do(ctx, c.name, version, func(ctx context.Context) (string, error) {
			var result string
			query := fmt.Sprintf("SELECT count(*)::VARCHAR || ':' || coalesce(sum(hash(%s)::HUGEINT), 0)::VARCHAR FROM %s", strings.Join(c.columns, ","), c.table)
			err := handle.QueryRowContext(ctx, query).Scan(&result)
			return result, err
		})
		if err != nil {
			return "", nil
		}
		parts = append(parts, digest)
	}
	return strings.Join(parts, "|"), handle
}
