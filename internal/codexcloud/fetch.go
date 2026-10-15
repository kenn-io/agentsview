package codexcloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

// TurnView is the itemsView sent with every thread/turns/list call. The
// hosted default is "full", which returns every item of every turn, so it
// is never left out.
type TurnView string

const TurnViewNotLoaded TurnView = "notLoaded"

const (
	turnPageSize = 50
	itemPageSize = 200
)

// fetchResult is one thread's fetch: the records to append and the state
// to save once they are durable.
type fetchResult struct {
	Records []mirror.Record
	Next    ThreadState
	// Unfinished means the thread must be fetched again next pass.
	Unfinished bool
	// InProgress means its newest turns include one still running.
	InProgress bool
}

type turnsPage struct {
	Data       []jsontext.Value `json:"data"`
	NextCursor jsontext.Value   `json:"nextCursor"`
}

type itemsPage struct {
	Data       []jsontext.Value `json:"data"`
	NextCursor jsontext.Value   `json:"nextCursor"`
}

type turnsParams struct {
	ThreadID  string         `json:"threadId"`
	Limit     int            `json:"limit"`
	Cursor    jsontext.Value `json:"cursor,omitzero"`
	ItemsView TurnView       `json:"itemsView"`
}

type itemsParams struct {
	ThreadID string         `json:"threadId"`
	TurnID   string         `json:"turnId"`
	Limit    int            `json:"limit"`
	Cursor   jsontext.Value `json:"cursor,omitzero"`
}

type itemAnchor struct {
	Type   string `json:"type"`
	ItemID string `json:"itemId"`
}

// fetchThread reads what changed in one thread since st's resume point.
// Turn statuses come first, so a turn that ends between the two calls is
// not yet treated as ended and its items are fetched again next pass.
// listing is the thread's listing record, or nil. The caller owns
// redialing after ErrReplyTooLarge.
func fetchThread(
	ctx context.Context, c *Conn, threadID string, st ThreadState, listing jsontext.Value, now time.Time,
) (fetchResult, error) {
	return fetchThreadPaged(ctx, c, threadID, st, listing, now, itemPageSize)
}

// errTurnListTooLarge marks an oversized turn listing, which a smaller item
// page cannot fix.
var errTurnListTooLarge = fmt.Errorf("%w: turn listing", ErrReplyTooLarge)

// fetchThreadPaged is fetchThread with an item page size; the pass uses one
// item per page to retry after an oversized reply.
func fetchThreadPaged(
	ctx context.Context, c *Conn, threadID string, st ThreadState, listing jsontext.Value,
	now time.Time, itemLimit int,
) (fetchResult, error) {
	next := st
	var records []mirror.Record
	if len(listing) > 0 {
		records = append(records, mirror.Record{Kind: mirror.KindThread, FetchedAt: now, Data: listing})
		var meta mirror.Thread
		if err := json.Unmarshal(listing, &meta); err != nil {
			return fetchResult{}, fmt.Errorf("%w: unreadable thread record: %w", ErrProtocolChange, err)
		}
		if meta.UpdatedAt != nil {
			updated := int64(math.Floor(*meta.UpdatedAt))
			if updated != st.UpdatedAt {
				next.LastProgress = now
			}
			next.UpdatedAt = updated
		}
		if meta.CreatedAt != nil {
			next.CreatedAt = int64(math.Floor(*meta.CreatedAt))
		}
	}

	turnRecords, err := listTurns(ctx, c, threadID, st.ResumeTurnID)
	if errors.Is(err, ErrReplyTooLarge) {
		return fetchResult{}, errTurnListTooLarge
	}
	if err != nil {
		return fetchResult{}, err
	}
	previous := map[string]TurnState{}
	for _, turn := range st.Turns {
		previous[turn.ID] = turn
	}
	turns := make([]TurnState, 0, len(turnRecords))
	for _, raw := range turnRecords {
		var turn mirror.Turn
		if err := json.Unmarshal(raw, &turn); err != nil || turn.ID == "" {
			return fetchResult{}, fmt.Errorf("%w: unreadable turn record", ErrProtocolChange)
		}
		records = append(records, mirror.Record{Kind: mirror.KindTurn, FetchedAt: now, Data: raw})
		turns = append(turns, nextTurnState(previous[turn.ID], turn, now))
	}

	items := map[string][]mirror.ItemRecord{}
	for _, turn := range turns {
		var anchorID string
		if turn.ID == st.ResumeTurnID {
			anchorID = st.ResumeItemID
		}
		rows, err := listItems(ctx, c, threadID, turn.ID, anchorID, itemLimit)
		if err != nil {
			return fetchResult{}, err
		}
		for _, raw := range rows {
			var row mirror.ItemRecord
			if err := json.Unmarshal(raw, &row); err != nil {
				return fetchResult{}, fmt.Errorf("%w: unreadable item record: %w", ErrProtocolChange, err)
			}
			if row.TurnID == "" {
				row.TurnID = turn.ID
				if raw, err = json.Marshal(row); err != nil {
					return fetchResult{}, fmt.Errorf("codex-cloud: encode item record: %w", err)
				}
			}
			records = append(records, mirror.Record{Kind: mirror.KindItem, FetchedAt: now, Data: raw})
			items[turn.ID] = append(items[turn.ID], row)
		}
	}

	next.Turns = turns
	if len(turns) > 0 {
		next.ResumeTurnID, next.ResumeItemID = advanceResume(st, turns, items, now)
	}
	unfinished, inProgress := threadUnfinished(st, turns, items, now)
	return fetchResult{Records: records, Next: next, Unfinished: unfinished, InProgress: inProgress}, nil
}

func nextTurnState(old TurnState, turn mirror.Turn, now time.Time) TurnState {
	st := TurnState{ID: turn.ID, Status: turn.Status, FirstSeen: old.FirstSeen, Digest: old.Digest}
	if st.FirstSeen.IsZero() {
		st.FirstSeen = now
	}
	if turnEnded(turn.Status) {
		st.EndedFirstSeen = old.EndedFirstSeen
		if st.EndedFirstSeen.IsZero() {
			st.EndedFirstSeen = now
		}
	}
	if started := mirror.Seconds(turn.StartedAt); !started.IsZero() {
		st.StartedAt = &started
	}
	if completed := mirror.Seconds(turn.CompletedAt); !completed.IsZero() {
		st.CompletedAt = &completed
	}
	return st
}

// listTurns returns turns oldest first, from the resume turn on. It pages
// newest first and stops at the resume turn, so the cost follows the
// turns since the resume point, not the thread's length.
func listTurns(ctx context.Context, c *Conn, threadID, resumeTurnID string) ([]jsontext.Value, error) {
	params := turnsParams{ThreadID: threadID, Limit: turnPageSize, ItemsView: TurnViewNotLoaded}
	var turns []jsontext.Value
	for {
		var page turnsPage
		if err := c.Call(ctx, "thread/turns/list", params, &page); err != nil {
			return nil, err
		}
		for _, raw := range page.Data {
			turns = append(turns, raw)
			if resumeTurnID != "" {
				var turn mirror.Turn
				if json.Unmarshal(raw, &turn) == nil && turn.ID == resumeTurnID {
					slices.Reverse(turns)
					return turns, nil
				}
			}
		}
		if len(page.Data) == 0 || len(page.NextCursor) == 0 || page.NextCursor.Kind() == 'n' {
			slices.Reverse(turns)
			return turns, nil
		}
		params.Cursor = page.NextCursor
	}
}

// listItems reads one turn's items after anchorID, or all of them. An item
// anchor reads only within its turn. The anchor's own record is discarded,
// so this never depends on whether the server includes it. A rejected
// anchor falls back to reading the turn in full and skipping through the
// anchor.
func listItems(ctx context.Context, c *Conn, threadID, turnID, anchorID string, limit int) ([]jsontext.Value, error) {
	params := itemsParams{ThreadID: threadID, TurnID: turnID, Limit: limit}
	if anchorID != "" {
		anchor, err := json.Marshal(itemAnchor{Type: "item", ItemID: anchorID})
		if err != nil {
			return nil, fmt.Errorf("codex-cloud: encode item anchor: %w", err)
		}
		params.Cursor = anchor
		rows, err := pageItems(ctx, c, params)
		if rpcErr, ok := errors.AsType[*RPCError](err); ok &&
			Classify("thread/items/list", true, rpcErr) == CatAnchoredRead {
			params.Cursor = nil
			rows, err = pageItems(ctx, c, params)
			if err != nil {
				return nil, err
			}
			return afterItem(rows, anchorID), nil
		}
		if err != nil {
			return nil, err
		}
		return withoutItem(rows, anchorID), nil
	}
	return pageItems(ctx, c, params)
}

func pageItems(ctx context.Context, c *Conn, params itemsParams) ([]jsontext.Value, error) {
	var rows []jsontext.Value
	for {
		var page itemsPage
		if err := c.Call(ctx, "thread/items/list", params, &page); err != nil {
			return nil, err
		}
		rows = append(rows, page.Data...)
		if len(page.Data) == 0 || len(page.NextCursor) == 0 || page.NextCursor.Kind() == 'n' {
			return rows, nil
		}
		params.Cursor = page.NextCursor
	}
}

func rowItemID(raw jsontext.Value) string {
	var row mirror.ItemRecord
	var header mirror.ItemHeader
	if json.Unmarshal(raw, &row) != nil || json.Unmarshal(row.Item, &header) != nil {
		return ""
	}
	return header.ID
}

func withoutItem(rows []jsontext.Value, id string) []jsontext.Value {
	return slices.DeleteFunc(rows, func(raw jsontext.Value) bool { return rowItemID(raw) == id })
}

// afterItem keeps the rows after id. When id is missing every row is kept;
// a repeated record is harmless because the newest record wins.
func afterItem(rows []jsontext.Value, id string) []jsontext.Value {
	for i, raw := range rows {
		if rowItemID(raw) == id {
			return rows[i+1:]
		}
	}
	return rows
}

// digest identifies a record's content regardless of key order.
func digest(data jsontext.Value) string {
	canonical := slices.Clone(data)
	if err := canonical.Canonicalize(); err != nil {
		canonical = data
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// recordKey names the stored record a new record would replace.
func recordKey(record mirror.Record) string {
	switch record.Kind {
	case mirror.KindThread:
		return "thread"
	case mirror.KindTurn:
		var turn mirror.Turn
		_ = json.Unmarshal(record.Data, &turn)
		return "turn/" + turn.ID
	case mirror.KindItem:
		var row mirror.ItemRecord
		_ = json.Unmarshal(record.Data, &row)
		return "item/" + row.TurnID + "/" + rowItemID(record.Data)
	case mirror.KindTokenUsage, mirror.KindTurnCompleted:
		// A live record never replaces another: its join ID and observation
		// time make it unique, so its key is its content. The key exists
		// so a retried flush drops the records an earlier attempt appended
		// before its state save failed. pruneItemDigests drops these keys
		// before state is saved, so they last one commit.
		return liveKeyPrefix + digest(record.Data)
	default:
		return ""
	}
}

// liveKeyPrefix starts the digest keys of live monitor records.
const liveKeyPrefix = "live/"

// absorb records the digests of records already in the mirror, so dedup
// compares against what is really stored.
func absorb(st *ThreadState, records []mirror.Record) {
	for _, record := range records {
		switch record.Kind {
		case mirror.KindPlanUsageRow, mirror.KindPlanUsageOutcome:
			notePlanRecord(st, record)
		default:
			setDigest(st, recordKey(record), digest(record.Data))
		}
	}
}

func storedDigest(st *ThreadState, key string) string {
	if key == "thread" {
		return st.MetaDigest
	}
	if turnID, ok := strings.CutPrefix(key, "turn/"); ok {
		for _, turn := range st.Turns {
			if turn.ID == turnID {
				return turn.Digest
			}
		}
		return ""
	}
	return st.ItemDigests[key]
}

func setDigest(st *ThreadState, key, sum string) {
	if key == "" {
		return
	}
	if key == "thread" {
		st.MetaDigest = sum
		return
	}
	if turnID, ok := strings.CutPrefix(key, "turn/"); ok {
		for i := range st.Turns {
			if st.Turns[i].ID == turnID {
				st.Turns[i].Digest = sum
			}
		}
		return
	}
	if st.ItemDigests == nil {
		st.ItemDigests = map[string]string{}
	}
	st.ItemDigests[key] = sum
}

// dedup drops records equal to the newest stored record for the same key
// and records the digests of the rest, so an unchanged re-fetch appends
// nothing and causes no parse.
func dedup(st *ThreadState, records []mirror.Record) []mirror.Record {
	var kept []mirror.Record
	for _, record := range records {
		if record.Kind == mirror.KindPlanUsageRow || record.Kind == mirror.KindPlanUsageOutcome {
			// A plan usage record is kept when it differs from the newest
			// plan usage record of any kind, so a row returning after an
			// outcome is stored again.
			if digest(record.Data) == st.LastPlanRecordDigest {
				continue
			}
			notePlanRecord(st, record)
			kept = append(kept, record)
			continue
		}
		key, sum := recordKey(record), digest(record.Data)
		if key != "" && storedDigest(st, key) == sum {
			continue
		}
		setDigest(st, key, sum)
		kept = append(kept, record)
	}
	return kept
}

func notePlanRecord(st *ThreadState, record mirror.Record) {
	st.LastPlanRecordDigest, st.LastPlanRecordKind = digest(record.Data), string(record.Kind)
	if record.Kind == mirror.KindPlanUsageRow {
		st.LastPlanRow = record.Data
	}
}

// pruneItemDigests keeps digests only for items in turns that can still be
// fetched again: the resume turn and later ones. Live record digests are
// dropped: only the commit that recovered their frames needs them.
func pruneItemDigests(st *ThreadState) {
	// Turns run oldest first. Items of turns before the resume turn are
	// never listed again, so their digests are dropped.
	turns := st.Turns
	if i := slices.IndexFunc(turns, func(t TurnState) bool { return t.ID == st.ResumeTurnID }); i >= 0 {
		turns = turns[i:]
	}
	live := map[string]bool{}
	for _, turn := range turns {
		live[turn.ID] = true
	}
	for key := range st.ItemDigests {
		if strings.HasPrefix(key, liveKeyPrefix) {
			delete(st.ItemDigests, key)
			continue
		}
		rest, _ := strings.CutPrefix(key, "item/")
		turnID, _, _ := strings.Cut(rest, "/")
		if !live[turnID] {
			delete(st.ItemDigests, key)
		}
	}
}

// commitThread makes records durable and only then saves st. It first
// recovers frames an earlier append wrote without saving state, so dedup
// never compares against digests a crash left stale. A crash before the
// save repeats the fetch; it never skips records. appended reports whether
// any record was new.
func commitThread(
	store *Store, key AccountKey, threadID string, st ThreadState, records []mirror.Record, now time.Time,
) (next ThreadState, appended bool, err error) {
	path, err := store.MirrorPath(key, threadID)
	if err != nil {
		return st, false, err
	}
	info, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		st = forgetMirror(st)
	case statErr != nil:
		return st, false, fmt.Errorf("codex-cloud: stat mirror: %w", statErr)
	default:
		if info.Size() < st.Offset {
			// The file was replaced or cut short; the stored digests no
			// longer describe it.
			st = forgetMirror(st)
		}
		recovered, err := mirror.Append(path, st.Offset, nil)
		if err != nil {
			return st, false, err
		}
		st.Offset = recovered.Offset
		absorb(&st, recovered.Recovered)
	}
	kept := dedup(&st, records)
	if len(kept) > 0 {
		result, err := mirror.Append(path, st.Offset, kept)
		if err != nil {
			return st, false, err
		}
		st.Offset = result.Offset
		if slices.ContainsFunc(kept, func(r mirror.Record) bool { return r.Kind == mirror.KindItem }) {
			st.LastProgress = now
		}
	}
	pruneItemDigests(&st)
	if err := store.SaveThread(key, threadID, st); err != nil {
		return st, false, err
	}
	return st, len(kept) > 0, nil
}

// forgetHistory resets a thread whose mirror file was lost after its
// resume point moved: the next fetch reads every turn and item again, and
// dedup drops what the remaining file already holds.
func forgetHistory(st ThreadState) ThreadState {
	st = forgetMirror(st)
	st.ResumeTurnID, st.ResumeItemID = "", ""
	st.Turns = nil
	return st
}

func forgetMirror(st ThreadState) ThreadState {
	st.Offset = 0
	st.MetaDigest = ""
	st.ItemDigests = nil
	st.LastPlanRecordDigest, st.LastPlanRecordKind, st.LastPlanRow = "", "", nil
	for i := range st.Turns {
		st.Turns[i].Digest = ""
	}
	return st
}
