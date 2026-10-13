package parser

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

const codexCloudSessionPrefix = "codex-cloud:"

// codexCloudThread collects one mirror file's newest record of each kind.
type codexCloudThread struct {
	listing *mirror.Thread // newest thread record from a listing
	resume  *mirror.Thread // newest thread record from a resume reply
	// environmentCwd is the first environments[].cwd of the newest listing
	// that had one. A listing of a thread that is no longer loaded carries
	// null environments, which must not discard the folder.
	environmentCwd string
	turns          map[string]mirror.Turn
	// turnOrder lists turn IDs in first-seen order.
	turnOrder []string
	items     map[codexCloudItemKey]mirror.ItemRecord
	// itemOrder lists item keys in first-seen order, so a re-fetched item
	// keeps its place while its newest record wins.
	itemOrder []codexCloudItemKey
	newestAt  time.Time
}

type codexCloudItemKey struct{ turnID, itemID string }

// ParseCodexCloudFile parses one Codex cloud mirror file into a session.
// It returns ok false for a file with no thread metadata record.
func ParseCodexCloudFile(ctx context.Context, path, machine string) (ParseResult, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ParseResult{}, false, fmt.Errorf("stat codex-cloud mirror: %w", err)
	}
	records, err := mirror.ReadAll(ctx, path)
	if err != nil {
		return ParseResult{}, false, err
	}
	thread, err := collectCodexCloudRecords(records)
	if err != nil {
		return ParseResult{}, false, err
	}
	meta := thread.listing
	if meta == nil {
		meta = thread.resume
	}
	if meta == nil {
		return ParseResult{}, false, nil
	}

	messages := thread.messages(meta)
	firstMessage, userCount := firstMessageAndUserCount(messages)
	cwd := thread.cwd()
	project := ExtractProjectFromCwd(cwd)
	if project == "" {
		project = "codex_cloud"
	}
	inode, device := sourceFileIdentity(info)
	session := ParsedSession{
		ID:                 codexCloudSessionPrefix + meta.ID,
		Project:            project,
		Machine:            machine,
		Agent:              AgentCodexCloud,
		Entrypoint:         codexCloudEntrypoint(meta),
		Cwd:                cwd,
		SourceSessionID:    meta.ID,
		FirstMessage:       firstMessage,
		SessionName:        firstNonEmpty(meta.Name, meta.Preview),
		SessionNamePresent: meta.Name != "" || meta.Preview != "",
		MessageCount:       len(messages),
		UserMessageCount:   userCount,
		File: FileInfo{
			Path: path, Size: info.Size(), Mtime: info.ModTime().UnixNano(),
			Inode: int64(inode), Device: int64(device),
		},
	}
	session.StartedAt, session.EndedAt = thread.times()
	if parent := codexCloudResolvedParent(meta, messages); parent != "" {
		session.ParentSessionID = codexCloudSessionPrefix + parent
		session.RelationshipType = RelSubagent
	} else if meta.ForkedFromID != "" {
		session.ParentSessionID = codexCloudSessionPrefix + meta.ForkedFromID
		session.RelationshipType = RelFork
	}
	return ParseResult{Session: session, Messages: messages}, true, nil
}

func collectCodexCloudRecords(records []mirror.Record) (*codexCloudThread, error) {
	thread := &codexCloudThread{
		turns: map[string]mirror.Turn{},
		items: map[codexCloudItemKey]mirror.ItemRecord{},
	}
	for index, record := range records {
		thread.newestAt = codexCloudLaterTime(thread.newestAt, record.FetchedAt)
		switch record.Kind {
		case mirror.KindThread, mirror.KindThreadResume:
			var meta mirror.Thread
			if err := json.Unmarshal(record.Data, &meta); err != nil {
				return nil, fmt.Errorf("codex-cloud record %d: decode thread: %w", index, err)
			}
			if record.Kind == mirror.KindThread {
				thread.listing = &meta
				if cwd := codexCloudEnvironmentCwd(meta.Environments); cwd != "" {
					thread.environmentCwd = cwd
				}
			} else {
				thread.resume = &meta
			}
		case mirror.KindTurn:
			var turn mirror.Turn
			if err := json.Unmarshal(record.Data, &turn); err != nil {
				return nil, fmt.Errorf("codex-cloud record %d: decode turn: %w", index, err)
			}
			if _, seen := thread.turns[turn.ID]; !seen {
				thread.turnOrder = append(thread.turnOrder, turn.ID)
			}
			thread.turns[turn.ID] = turn
		case mirror.KindItem:
			var item mirror.ItemRecord
			if err := json.Unmarshal(record.Data, &item); err != nil {
				return nil, fmt.Errorf("codex-cloud record %d: decode item: %w", index, err)
			}
			var header mirror.ItemHeader
			if err := json.Unmarshal(item.Item, &header); err != nil {
				return nil, fmt.Errorf("codex-cloud record %d: decode item header: %w", index, err)
			}
			key := codexCloudItemKey{turnID: item.TurnID, itemID: header.ID}
			if _, seen := thread.items[key]; !seen {
				thread.itemOrder = append(thread.itemOrder, key)
			}
			thread.items[key] = item
			if _, seen := thread.turns[item.TurnID]; !seen && item.TurnID != "" {
				thread.turns[item.TurnID] = mirror.Turn{ID: item.TurnID}
				thread.turnOrder = append(thread.turnOrder, item.TurnID)
			}
		default:
			// Token, turn-completion, and plan usage records are read by
			// their own steps.
		}
	}
	return thread, nil
}

// orderedItems returns items grouped by turn, turns ordered by start time
// and then first appearance, items in first-seen order within a turn.
func (t *codexCloudThread) orderedItems() []mirror.ItemRecord {
	turnRank := make(map[string]int, len(t.turnOrder))
	order := slices.Clone(t.turnOrder)
	// Turns with a start time come first, oldest first; the stable sort
	// keeps first-seen order among turns without one.
	slices.SortStableFunc(order, func(a, b string) int {
		ta, tb := mirror.Seconds(t.turns[a].StartedAt), mirror.Seconds(t.turns[b].StartedAt)
		if c := cmp.Compare(codexCloudRank(ta.IsZero()), codexCloudRank(tb.IsZero())); c != 0 {
			return c
		}
		return ta.Compare(tb)
	})
	for rank, id := range order {
		turnRank[id] = rank
	}
	keys := slices.Clone(t.itemOrder)
	slices.SortStableFunc(keys, func(a, b codexCloudItemKey) int {
		return cmp.Compare(turnRank[a.turnID], turnRank[b.turnID])
	})
	items := make([]mirror.ItemRecord, len(keys))
	for i, key := range keys {
		items[i] = t.items[key]
	}
	return items
}

func (t *codexCloudThread) messages(meta *mirror.Thread) []ParsedMessage {
	var messages []ParsedMessage
	for _, record := range t.orderedItems() {
		ts := mirror.Millis(record.StartedAtMs)
		if ts.IsZero() {
			ts = mirror.Seconds(t.turns[record.TurnID].StartedAt)
		}
		for _, msg := range codexCloudItemMessages(record, ts, meta) {
			msg.Ordinal = len(messages)
			messages = append(messages, msg)
		}
	}
	return messages
}

// times returns the session's start and end. Listing records are the only
// source of thread times; a file holding only a resume record ends at its
// newest item.
func (t *codexCloudThread) times() (time.Time, time.Time) {
	if t.listing != nil {
		started := mirror.Seconds(t.listing.CreatedAt)
		ended := mirror.Seconds(t.listing.UpdatedAt)
		if ended.IsZero() {
			ended = started
		}
		return started, ended
	}
	var started, ended time.Time
	for _, item := range t.items {
		if s := mirror.Millis(item.StartedAtMs); !s.IsZero() && (started.IsZero() || s.Before(started)) {
			started = s
		}
		ended = codexCloudLaterTime(ended, mirror.Millis(item.CompletedAtMs))
	}
	if t.resume != nil && t.resume.CreatedAt != nil {
		started = mirror.Seconds(t.resume.CreatedAt)
	}
	if ended.IsZero() {
		ended = started
	}
	return started, ended
}

// cwd uses only listing records: a resume reply abbreviates the home
// directory as "~". The listing's cwd is used only when no listing ever
// named an environment folder.
func (t *codexCloudThread) cwd() string {
	if t.listing == nil {
		return ""
	}
	return firstNonEmpty(t.environmentCwd, t.listing.Cwd)
}

func codexCloudEnvironmentCwd(environments []mirror.Environment) string {
	for _, env := range environments {
		if strings.TrimSpace(env.Cwd) != "" {
			return env.Cwd
		}
	}
	return ""
}

func codexCloudEntrypoint(meta *mirror.Thread) string {
	switch {
	case meta.Originator == "codex_work_cca" || meta.ThreadSource == "aeon_child":
		return "dot"
	case meta.Originator == "codex_work_web":
		return "codex-web"
	case meta.Originator == "codex_cloud":
		return "codex-cloud"
	default:
		return meta.Originator
	}
}

// codexCloudResolvedParent is parentThreadId, else the parent a spawned
// sub-agent's source names, else the thread a delegated thread's first user
// message names in its <codex_delegation> block.
func codexCloudResolvedParent(meta *mirror.Thread, messages []ParsedMessage) string {
	if meta.ParentThreadID != "" {
		return meta.ParentThreadID
	}
	if parent := codexCloudSpawnParent(meta.Source); parent != "" {
		return parent
	}
	return codexCloudDelegationParent(meta.ID, messages)
}

// codexCloudDelegationRe matches the block that opens a delegated thread's
// first user message. The tag must start the text and hold a UUID.
var codexCloudDelegationRe = regexp.MustCompile(
	`^<codex_delegation><source_thread_id>([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-` +
		`[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})</source_thread_id>`,
)

func codexCloudDelegationParent(threadID string, messages []ParsedMessage) string {
	for _, msg := range messages {
		if msg.Role != RoleUser || len(msg.ToolResults) > 0 {
			continue
		}
		match := codexCloudDelegationRe.FindStringSubmatch(msg.Content)
		if match == nil || match[1] == threadID {
			return ""
		}
		return match[1]
	}
	return ""
}

func codexCloudSpawnParent(raw jsontext.Value) string {
	if len(raw) == 0 {
		return ""
	}
	var source struct {
		SubAgent struct {
			ThreadSpawn struct {
				ParentThreadID string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subAgent"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return ""
	}
	return source.SubAgent.ThreadSpawn.ParentThreadID
}

func codexCloudRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func codexCloudLaterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// rawString decodes a JSON string, returning "" for anything else.
func codexCloudString(v jsontext.Value) string {
	var s string
	if len(v) == 0 || json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}
