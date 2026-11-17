package clickhouse

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// A session that only grew keeps the dependent rows the mirror already holds
// for its earlier messages, and a push writes only the rows after them.
//
// The mirror's session row records, in agentsview_message_rows_fingerprint,
// a hash of the rows stored in the ordinal-keyed tables (messageOrdinalColumn)
// for its stored_message_count messages, and in
// agentsview_leading_message_rows_fingerprint the hash of the same rows
// without the last message's. When the content fingerprint differs, a push
// hashes the same rows for that many leading local messages. A match proves
// those rows are already stored, byte for byte, so the push appends the rows
// of the later messages and rewrites the small tables that are not keyed by
// message ordinal.
//
// An active session is often pushed while its last message waits on a tool
// result, which fills in that message's tool call row when it arrives. When
// only the leading hash still matches, the push keeps the rows of every
// stored message but the last, and writes that message again with the rest.
// Anything else, such as an edited or renumbered earlier message, a resync,
// or a parser change, changes both hashes, and the push rewrites the whole
// session.
//
// An append keeps the session's content_version, the push version of the
// oldest rows it keeps, and the appended rows carry the new push version.
// Reads that pair dependent rows with their session therefore accept rows at
// or above content_version.

// messageOrdinalColumn names, for each table whose rows belong to one
// message, the column holding that message's ordinal. An append keeps the
// rows at or below the last kept ordinal and replaces the rest.
var messageOrdinalColumn = map[string]string{
	"messages":           "ordinal",
	"tool_calls":         "message_ordinal",
	"tool_result_events": "tool_call_message_ordinal",
	"image_text":         "message_ordinal",
	"usage_messages":     "ordinal",
}

// messageRowsFingerprints hashes the rows a push stores in the ordinal-keyed
// tables, in one pass over the messages, and returns for each count in cuts
// the hash of the rows of that many leading messages. Each message
// contributes its message row, its tool call and result event rows, then the
// image text rows anchored after the previous message's ordinal and at or
// before its own; image text after the last message is not hashed. The hash
// of a grown session's leading messages therefore equals the hash a push
// stored before the session grew. A count of 0 or above len(messages) gets
// "", and every count gets "" when the messages are not in strictly
// increasing ordinal order, since an append then cannot tell kept rows from
// new ones.
func messageRowsFingerprints(messages []db.Message, images []db.ImageTextRow, cuts ...int) ([]string, error) {
	out := make([]string, len(cuts))
	if !ordinalsIncrease(messages) {
		return out, nil
	}
	byOrdinal := slices.Clone(images)
	slices.SortStableFunc(byOrdinal, func(a, b db.ImageTextRow) int {
		return cmp.Compare(a.MessageOrdinal, b.MessageOrdinal)
	})
	h := sha256.New()
	write := func(row []any) error {
		if err := json.MarshalWrite(h, row); err != nil {
			return fmt.Errorf("encoding message rows fingerprint: %w", err)
		}
		_, _ = h.Write([]byte{'\n'})
		return nil
	}
	record := func(n int) {
		var sum string
		for i, cut := range cuts {
			if cut == n && n > 0 {
				if sum == "" {
					sum = hex.EncodeToString(h.Sum(nil))
				}
				out[i] = sum
			}
		}
	}
	next := 0
	for i, m := range messages {
		record(i)
		if err := write(messageRow(m, 0)); err != nil {
			return nil, err
		}
		for j, tc := range m.ToolCalls {
			if err := write(toolCallRow(m, tc, j, 0)); err != nil {
				return nil, err
			}
			for _, ev := range tc.ResultEvents {
				if err := write(toolResultEventRow(m, j, ev, 0)); err != nil {
					return nil, err
				}
			}
		}
		for ; next < len(byOrdinal) && byOrdinal[next].MessageOrdinal <= m.Ordinal; next++ {
			if err := write(imageTextRow(byOrdinal[next], 0)); err != nil {
				return nil, err
			}
		}
	}
	record(len(messages))
	return out, nil
}

func ordinalsIncrease(messages []db.Message) bool {
	for i := 1; i < len(messages); i++ {
		if messages[i].Ordinal <= messages[i-1].Ordinal {
			return false
		}
	}
	return true
}

// keptOrdinalBound renders the condition that limits a vanished-rows delete
// on an ordinal-keyed table to rows after the last kept ordinal of each
// appended session. Sessions not in keptThrough are bounded by nothing. It
// returns the SQL and its arguments, or "" when no session keeps rows.
func keptOrdinalBound(column string, ids []string, keptThrough map[string]int) (string, []any) {
	var from []string
	var args, to []any
	for _, id := range ids {
		if last, ok := keptThrough[id]; ok {
			args = append(args, id)
			to = append(to, int64(last))
			from = append(from, "?")
		}
	}
	if len(from) == 0 {
		return "", nil
	}
	list := strings.Join(from, ",")
	return " AND " + column + " > transform(session_id, [" + list + "], CAST([" + list +
		"] AS Array(Int64)), toInt64(-9223372036854775808))", append(args, to...)
}
