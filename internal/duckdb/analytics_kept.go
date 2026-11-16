package duckdb

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

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
	version, savedVersion := s.keptVersions()
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
	if !db.SavedPanel(panel) {
		savedVersion = ""
	}
	if savedVersion != "" {
		savedVersion += "|" + db.TrendsTimezoneKey(f)
	}
	encoded, err := s.analyticsReads.DoSaved(ctx, &s.usageDisk, string(key), version, savedVersion, func(b []byte) ([]byte, error) { return b, nil }, func(b []byte) ([]byte, error) {
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
