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
// for its stored_message_count messages. When the content fingerprint
// differs, a push hashes the same rows for that many leading local messages.
// A match proves those rows are already stored, byte for byte, so the push
// appends the rows of the later messages and rewrites the small tables that
// are not keyed by message ordinal. Anything else, such as an edited or
// renumbered earlier message, a tool result that arrived for an earlier call,
// a resync, or a parser change, changes the hash, and the push rewrites the
// whole session.
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
// tables, in one pass over the messages, and returns the hash after the
// first keep messages and the hash after all of them. Each message
// contributes its message row, its tool call and result event rows, then the
// image text rows anchored after the previous message's ordinal and at or
// before its own; image text after the last message is not hashed. The prefix
// hash of a grown session therefore equals the full hash a push stored before
// the session grew. Both hashes are "" when the messages are not in strictly
// increasing ordinal order, since an append then cannot tell kept rows from
// new ones. The prefix hash is "" when keep is 0.
func messageRowsFingerprints(messages []db.Message, images []db.ImageTextRow, keep int) (prefix, full string, err error) {
	if !ordinalsIncrease(messages) {
		return "", "", nil
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
	next := 0
	for i, m := range messages {
		if i == keep && keep > 0 {
			prefix = hex.EncodeToString(h.Sum(nil))
		}
		if err := write(messageRow(m, 0)); err != nil {
			return "", "", err
		}
		for j, tc := range m.ToolCalls {
			if err := write(toolCallRow(m, tc, j, 0)); err != nil {
				return "", "", err
			}
			for _, ev := range tc.ResultEvents {
				if err := write(toolResultEventRow(m, j, ev, 0)); err != nil {
					return "", "", err
				}
			}
		}
		for ; next < len(byOrdinal) && byOrdinal[next].MessageOrdinal <= m.Ordinal; next++ {
			if err := write(imageTextRow(byOrdinal[next], 0)); err != nil {
				return "", "", err
			}
		}
	}
	full = hex.EncodeToString(h.Sum(nil))
	if keep > 0 && keep == len(messages) {
		prefix = full
	}
	return prefix, full, nil
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
