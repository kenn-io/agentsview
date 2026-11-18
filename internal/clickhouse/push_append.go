package clickhouse

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
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

// keptOrdinalBound renders the condition that compares an ordinal-keyed
// table's column, with op, to the last kept ordinal of each appended session:
// ">" limits a vanished-rows delete to the rows after the kept ones, and "<="
// selects the kept rows. Sessions not in keptThrough are compared with the
// lowest Int64. It returns the SQL and its arguments, or "" when no session
// keeps rows.
func keptOrdinalBound(column, op string, ids []string, keptThrough map[string]int) (string, []any) {
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
	return " AND " + column + " " + op + " transform(session_id, [" + list + "], CAST([" + list +
		"] AS Array(Int64)), toInt64(-9223372036854775808))", append(args, to...)
}

// keptRows describes the stored rows an append keeps for one session: those
// of its messages through ordinal through, written from contentVersion
// through rowsVersion. counts holds how many rows each of keptRowsTables
// should have for them.
type keptRows struct {
	through        int
	contentVersion uint64
	rowsVersion    uint64
	counts         [len(keptRowsTables)]uint64
}

// keptRowsTables are the tables whose kept rows an append checks.
// usage_messages copies the messages rows, so checking messages covers it.
var keptRowsTables = [...]string{"messages", "tool_calls", "tool_result_events", "image_text"}

// newKeptRows describes the rows of the first keep messages of p, which the
// session row published from contentVersion through rowsVersion.
func newKeptRows(p sessionPayload, keep int, contentVersion, rowsVersion uint64) keptRows {
	k := keptRows{
		through: p.messages[keep-1].Ordinal, contentVersion: contentVersion, rowsVersion: rowsVersion,
	}
	k.counts[0] = uint64(keep)
	for _, m := range p.messages[:keep] {
		k.counts[1] += uint64(len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			k.counts[2] += uint64(len(tc.ResultEvents))
		}
	}
	for _, r := range p.imageText {
		if r.MessageOrdinal <= k.through {
			k.counts[3]++
		}
	}
	return k
}

// keptRowsPublished reports, for each session in kept, whether the mirror
// still holds exactly the rows the session row published for the kept
// messages. Another archive's push of the same session that stopped before
// publishing its session row leaves rows at a newer version than the session
// row's rows_version, which win on read over the rows an append would keep.
// Its delete, or its deletion of the session, can remove kept rows. Either way
// the append would publish a mix of two archives' rows, so the push rewrites
// the session instead. Pushes are versioned by start time, so this misses only
// a push that runs at the same time as this one.
func (s *Sync) keptRowsPublished(ctx context.Context, kept map[string]keptRows) (map[string]bool, error) {
	out := make(map[string]bool, len(kept))
	through := make(map[string]int, len(kept))
	for id, k := range kept {
		through[id] = k.through
	}
	for batch := range idBatches(slices.Sorted(maps.Keys(kept))) {
		placeholders, idArgs := inArgs(batch)
		parts := make([]string, 0, len(keptRowsTables))
		var args []any
		for i, table := range keptRowsTables {
			bound, boundArgs := keptOrdinalBound(messageOrdinalColumn[table], "<=", batch, through)
			parts = append(parts, fmt.Sprintf("SELECT %d AS t, session_id, push_version FROM %s WHERE session_id IN (%s)%s",
				i, table, placeholders, bound))
			args = slices.Concat(args, idArgs, boundArgs)
		}
		if err := func() error {
			rows, err := s.conn.QueryContext(ctx, `SELECT session_id,
					countIf(t = 0), countIf(t = 1), countIf(t = 2), countIf(t = 3),
					min(push_version), max(push_version)
				FROM (`+strings.Join(parts, " UNION ALL ")+`) GROUP BY session_id`, args...)
			if err != nil {
				return fmt.Errorf("reading clickhouse kept message row versions: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				var counts [len(keptRowsTables)]uint64
				var oldest, newest uint64
				if err := rows.Scan(&id, &counts[0], &counts[1], &counts[2], &counts[3], &oldest, &newest); err != nil {
					return fmt.Errorf("scanning clickhouse kept message row versions: %w", err)
				}
				k := kept[id]
				out[id] = counts == k.counts && oldest >= k.contentVersion && newest <= k.rowsVersion
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
