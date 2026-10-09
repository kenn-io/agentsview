package readbase

import (
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func ScanAnalyticsModelTimes(rows *sql.Rows, backend string, formatTime func(any) string, emit func(model, timestamp string)) error {
	for rows.Next() {
		var model string
		var ts any
		if err := rows.Scan(&model, &ts); err != nil {
			return fmt.Errorf("scanning %s filtered analytics model: %w", backend, err)
		}
		emit(model, formatTime(ts))
	}
	return rows.Err()
}

func ScanAnalyticsToolCounts(rows *sql.Rows, backend string, formatTime func(any) string, emit func(sessionID, model, timestamp string, count int)) error {
	for rows.Next() {
		var sessionID, model string
		var ts any
		var count int
		if err := rows.Scan(&sessionID, &model, &ts, &count); err != nil {
			return fmt.Errorf(
				"scanning %s filtered analytics tool calls: %w",
				backend, err,
			)
		}
		emit(sessionID, model, formatTime(ts), count)
	}
	return rows.Err()
}

func ScanAnalyticsAutonomy(rows *sql.Rows, backend string) (map[string]int, error) {
	counts := map[string]int{}
	for rows.Next() {
		var sessionID string
		var userCount, toolCount int
		if err := rows.Scan(&sessionID, &userCount, &toolCount); err != nil {
			return nil, fmt.Errorf("scanning %s autonomy: %w", backend, err)
		}
		if userCount > 0 {
			counts[db.AutonomyBucket(float64(toolCount)/float64(userCount))]++
		}
	}
	return counts, rows.Err()
}

func ScanAnalyticsVelocityMessages(rows *sql.Rows, backend string, formatTime func(any) string, loc *time.Location, out map[string][]db.TimingMessage) (map[string][]db.TimingMessage, error) {
	for rows.Next() {
		var sid, role string
		var ordinal int
		var ts any
		var contentLength int
		if err := rows.Scan(&sid, &ordinal, &role, &ts, &contentLength); err != nil {
			return nil, fmt.Errorf("scanning %s velocity message: %w", backend, err)
		}
		parsed, ok := AnalyticsLocalTime(formatTime(ts), loc)
		out[sid] = append(out[sid], db.TimingMessage{
			Role:          role,
			Time:          parsed,
			Valid:         ok,
			ContentLength: contentLength,
		})
	}
	return out, rows.Err()
}

func ScanAnalyticsVelocityToolCounts(rows *sql.Rows, backend string, out map[string]int) (map[string]int, error) {
	for rows.Next() {
		var sid string
		var count int
		if err := rows.Scan(&sid, &count); err != nil {
			return nil, fmt.Errorf("scanning %s velocity tool call count: %w", backend, err)
		}
		out[sid] = count
	}
	return out, rows.Err()
}

func ScanAnalyticsTools(rows *sql.Rows, formatTime func(any) string, emit func(sessionID, category, name, timestamp string, count int)) error {
	for rows.Next() {
		var sid, cat, toolName string
		var ts any
		var count int
		if err := rows.Scan(&sid, &cat, &toolName, &count, &ts); err != nil {
			return err
		}
		emit(sid, cat, toolName, formatTime(ts), count)
	}
	return rows.Err()
}

func ScanAnalyticsSkills(rows *sql.Rows, formatTime func(any) string, emit func(sessionID, name, timestamp string, count int)) error {
	for rows.Next() {
		var sid, skill string
		var count int
		var msgTS any
		if err := rows.Scan(&sid, &skill, &count, &msgTS); err != nil {
			return err
		}
		emit(sid, skill, formatTime(msgTS), count)
	}
	return rows.Err()
}

func ScanAnalyticsSignalMessages(rows *sql.Rows, backend string, formatTime func(any) string, emit func(db.SignalMessage)) error {
	for rows.Next() {
		var m db.SignalMessage
		var ts any
		if err := rows.Scan(
			&m.SessionID, &m.Ordinal, &m.Role,
			&m.Content, &ts,
			&m.IsSystem, &m.HasToolUse, &m.SourceSubtype,
		); err != nil {
			return fmt.Errorf("scanning %s signal message: %w", backend, err)
		}
		m.Timestamp = formatTime(ts)
		emit(m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating %s signal messages: %w", backend, err)
	}
	return nil
}

func ScanAnalyticsMessageScope(rows *sql.Rows, backend string, formatTime func(any) string, loc *time.Location, reducer *db.ScopeReducer) error {
	for rows.Next() {
		var (
			sessionID, role, sourceSubtype, model, content     string
			ordinal, outputTokens, contentLength               int
			isSystem, hasThinking, hasToolUse, hasOutputTokens bool
			ts                                                 any
		)
		if err := rows.Scan(
			&sessionID, &ordinal, &role, &sourceSubtype, &isSystem, &model,
			&hasThinking, &hasToolUse, &ts, &outputTokens,
			&hasOutputTokens, &contentLength, &content,
		); err != nil {
			return fmt.Errorf("scanning %s analytics candidate message: %w", backend, err)
		}
		tsStr := formatTime(ts)
		parsed, has := AnalyticsLocalTime(tsStr, loc)
		if err := reducer.Push(db.MessageInput{
			SessionID:       sessionID,
			Ordinal:         ordinal,
			Role:            role,
			SourceSubtype:   sourceSubtype,
			Model:           model,
			IsSystem:        isSystem,
			Timestamp:       tsStr,
			LocalTime:       parsed,
			HasLocalTime:    has,
			HasThinking:     hasThinking,
			HasToolUse:      hasToolUse,
			OutputTokens:    outputTokens,
			HasOutputTokens: hasOutputTokens,
			ContentLength:   contentLength,
			Content:         content,
		}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating %s analytics candidate messages: %w", backend, err)
	}
	return nil
}
