package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"reflect"

	"go.kenn.io/agentsview/internal/db"
)

// Derived facts stay mutable without participating in immutable content identity.
type rawRecencyState struct {
	Outcome, OutcomeConfidence string
	SignalsPendingSince        *string
	HealthScore                *int
	HealthGrade                *string
	ToolObservations           []db.ToolObservation `json:",omitzero"`
}

func decodeRawRecency(data []byte) (rawRecencyState, error) {
	var state rawRecencyState
	err := json.Unmarshal(data, &state)
	return state, err
}

func (r rawRecencyState) apply(update *db.SessionSignalUpdate) {
	update.Outcome, update.OutcomeConfidence = r.Outcome, r.OutcomeConfidence
	update.SignalsPendingSince = r.SignalsPendingSince
	update.HealthScore, update.HealthGrade = r.HealthScore, r.HealthGrade
	if r.ToolObservations != nil {
		update.ToolObservations = r.ToolObservations
	}
}

// publishRawRecency runs under the logical group lock. It persists settling for
// later reactivation and reports only a change to a currently materialized row.
// Equal recent observations retain the original pending timestamp.
func publishRawRecency(ctx context.Context, tx *sql.Tx, id string, update db.SessionSignalUpdate) (bool, error) {
	next := rawRecencyState{update.Outcome, update.OutcomeConfidence, update.SignalsPendingSince, update.HealthScore, update.HealthGrade, update.ToolObservations}
	var data []byte
	if err := tx.QueryRowContext(ctx, `SELECT recency_state FROM raw_content_revisions WHERE session_id=$1`, id).Scan(&data); err != nil {
		return false, err
	}
	prior, err := decodeRawRecency(data)
	if err != nil {
		return false, err
	}
	if next.ToolObservations == nil {
		next.ToolObservations = prior.ToolObservations
	}
	if prior.SignalsPendingSince != nil && next.SignalsPendingSince != nil {
		next.SignalsPendingSince = prior.SignalsPendingSince
	}
	if reflect.DeepEqual(prior, next) {
		return false, nil
	}
	data, err = json.Marshal(next)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE raw_content_revisions SET recency_state=$2 WHERE session_id=$1`, id, data); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET outcome=$2,outcome_confidence=$3,signals_pending_since=$4,health_score=$5,health_grade=$6 WHERE id=$1 AND (outcome,outcome_confidence,signals_pending_since,health_score,health_grade) IS DISTINCT FROM ($2::text,$3::text,$4::text,$5::int,$6::text)`, id, next.Outcome, next.OutcomeConfidence, next.SignalsPendingSince, next.HealthScore, next.HealthGrade)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if next.ToolObservations != nil && !reflect.DeepEqual(prior.ToolObservations, next.ToolObservations) {
		observations, err := json.Marshal(next.ToolObservations)
		if err != nil {
			return false, err
		}
		result, err := tx.ExecContext(ctx, `WITH facts AS (
 SELECT tc.id, o."Outcome" AS outcome, o."Repeat" AS repeat, o."SequenceEnding" AS ending
 FROM tool_calls tc LEFT JOIN jsonb_to_recordset($2::jsonb) AS o("MessageOrdinal" int,"CallIndex" int,"Outcome" text,"Repeat" text,"SequenceEnding" text)
 ON tc.message_ordinal=o."MessageOrdinal" AND tc.call_index=o."CallIndex" WHERE tc.session_id=$1
) UPDATE tool_calls tc SET observed_outcome=f.outcome,observed_repeat=f.repeat,sequence_ending=f.ending FROM facts f WHERE tc.id=f.id AND (tc.observed_outcome,tc.observed_repeat,tc.sequence_ending) IS DISTINCT FROM (f.outcome,f.repeat,f.ending)`, id, observations)
		if err != nil {
			return false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		count += changed
	}
	return count > 0, nil
}
