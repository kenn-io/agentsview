package parser

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// errCursorStoreFormat identifies readable stores whose structure cannot be
// used for enrichment. Database I/O errors remain retryable source errors.
var errCursorStoreFormat = errors.New("unsupported Cursor store format")

type cursorStoreTurn struct {
	UserDecoded      bool
	AssistantDecoded bool
	UserText         string
	AssistantText    string
	UserTime         time.Time
	AssistantTime    time.Time
	ReasoningText    string
	ReasoningTime    time.Time
}

type cursorStoreMetaJSON struct {
	AgentID          string `json:"agentId"`
	LatestRootBlobID string `json:"latestRootBlobId"`
}

type cursorStoreIndex struct {
	mu          sync.Mutex
	roots       map[string]map[string]string
	transcripts map[string]map[string]string
}

func newCursorStoreIndex() *cursorStoreIndex {
	return &cursorStoreIndex{
		roots:       make(map[string]map[string]string),
		transcripts: make(map[string]map[string]string),
	}
}

func (i *cursorStoreIndex) rememberTranscript(root, path string) {
	if i == nil || root == "" || path == "" {
		return
	}
	agentID := cursorRawIDFromTranscriptPath(path)
	if !IsValidSessionID(agentID) {
		return
	}
	key := filepath.Clean(root)
	i.mu.Lock()
	defer i.mu.Unlock()
	paths := i.transcripts[key]
	if paths == nil {
		paths = make(map[string]string)
		i.transcripts[key] = paths
	}
	paths[agentID] = filepath.Clean(path)
}

func (i *cursorStoreIndex) transcriptPath(root, agentID string) string {
	if i == nil || root == "" || !IsValidSessionID(agentID) {
		return ""
	}
	key := filepath.Clean(root)
	i.mu.Lock()
	defer i.mu.Unlock()
	path := i.transcripts[key][agentID]
	if path == "" {
		return ""
	}
	if _, ok := cursorRawSessionIDFromPath(key, path); !ok || !IsRegularFile(path) {
		delete(i.transcripts[key], agentID)
		return ""
	}
	return path
}

func (i *cursorStoreIndex) path(chatsRoot, agentID string) string {
	if i == nil {
		return ""
	}
	key := filepath.Clean(chatsRoot)
	i.mu.Lock()
	defer i.mu.Unlock()
	paths, ok := i.roots[key]
	if !ok {
		return ""
	}
	return i.validPath(key, paths, agentID)
}

func (i *cursorStoreIndex) initialized(chatsRoot string) bool {
	if i == nil || chatsRoot == "" {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.roots[filepath.Clean(chatsRoot)]
	return ok
}

// refresh keeps the last complete index on scan failure. An unsuccessful first
// scan records an empty index so each transcript does not repeat the same scan;
// the next discovery pass retries, and store events can populate it meanwhile.
func (i *cursorStoreIndex) refresh(chatsRoot string) {
	if i == nil || chatsRoot == "" {
		return
	}
	key := filepath.Clean(chatsRoot)
	paths, err := cursorStorePathsUnderChats(key)
	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil {
		if _, ok := i.roots[key]; !ok {
			i.roots[key] = nil
		}
		log.Printf("warning: Cursor store index: %v; continuing with transcripts and cached stores", err)
		return
	}
	i.roots[key] = paths
}

func (i *cursorStoreIndex) validPath(
	chatsRoot string, paths map[string]string, agentID string,
) string {
	path := paths[agentID]
	if path != "" && !cursorStorePathIsValid(chatsRoot, path) {
		delete(paths, agentID)
		return ""
	}
	return path
}

func (i *cursorStoreIndex) remember(chatsRoot, agentID, storePath string) {
	if i == nil || chatsRoot == "" || !IsValidSessionID(agentID) {
		return
	}
	key := filepath.Clean(chatsRoot)
	i.mu.Lock()
	defer i.mu.Unlock()
	paths := i.roots[key]
	if paths == nil {
		paths = make(map[string]string)
		i.roots[key] = paths
	}
	if storePath == "" || !cursorStorePathIsValid(key, storePath) {
		delete(paths, agentID)
		return
	}
	paths[agentID] = storePath
}

// enrichCursorSessionFromStore overlays field-8 turn data onto an existing
// transcript parse. The store must exist; callers skip this when absent.
// Message count, roles and order stay with the transcript.
func enrichCursorSessionFromStore(
	ctx context.Context,
	storePath, agentID string,
	sess *ParsedSession,
	msgs []ParsedMessage,
) error {
	turns, err := readCursorStoreTurns(ctx, storePath, agentID)
	if err != nil {
		return err
	}
	applyCursorStoreTurns(sess, msgs, turns)
	return nil
}

func readCursorStoreTurns(
	ctx context.Context, storePath, agentID string,
) ([]cursorStoreTurn, error) {
	if storePath == "" || !IsValidSessionID(agentID) {
		return nil, fmt.Errorf("%w: missing agent id", errCursorStoreFormat)
	}
	conn, err := openCursorIDEDB(storePath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	tx, err := beginCursorIDESnapshot(ctx, conn, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	meta, err := loadCursorStoreMeta(ctx, tx, agentID)
	if err != nil {
		return nil, err
	}

	loader := newCursorStoreBlobLoader(ctx, tx)
	rootData, ok := loader.load(meta.LatestRootBlobID)
	if loader.err != nil {
		return nil, fmt.Errorf(
			"cursor store %s: reading selected root (latestRootBlobId=%s): %w",
			storePath, meta.LatestRootBlobID, loader.err,
		)
	}
	if !ok {
		return nil, fmt.Errorf(
			"%w: missing selected root (latestRootBlobId=%s)",
			errCursorStoreFormat, meta.LatestRootBlobID,
		)
	}

	rootFields, err := agProtoParse(rootData)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: decoding root (latestRootBlobId=%s): %w",
			errCursorStoreFormat, meta.LatestRootBlobID, err,
		)
	}
	turnIndexField, ok := agProtoFind(rootFields, 8)
	if !ok {
		return nil, fmt.Errorf(
			"%w: root missing turn index (latestRootBlobId=%s)",
			errCursorStoreFormat, meta.LatestRootBlobID,
		)
	}
	turnIndexFields, ok := cursorStoreResolveMessage(turnIndexField, loader)
	if loader.err != nil {
		return nil, fmt.Errorf(
			"cursor store %s: reading turn index (latestRootBlobId=%s): %w",
			storePath, meta.LatestRootBlobID, loader.err,
		)
	}
	if !ok {
		return nil, fmt.Errorf(
			"%w: undecodable turn index (latestRootBlobId=%s)",
			errCursorStoreFormat, meta.LatestRootBlobID,
		)
	}

	var turns []cursorStoreTurn
	decodedAny := false
	for _, f := range turnIndexFields {
		if f.Number != 1 {
			continue
		}
		turnFields, ok := cursorStoreResolveMessage(f, loader)
		if loader.err != nil {
			return nil, fmt.Errorf(
				"cursor store %s: reading turn (latestRootBlobId=%s): %w",
				storePath, meta.LatestRootBlobID, loader.err,
			)
		}
		if !ok {
			turns = append(turns, cursorStoreTurn{})
			continue
		}
		turn, decoded := decodeCursorStoreTurn(turnFields, loader)
		if loader.err != nil {
			return nil, fmt.Errorf(
				"cursor store %s: reading turn payload (latestRootBlobId=%s): %w",
				storePath, meta.LatestRootBlobID, loader.err,
			)
		}
		decodedAny = decodedAny || decoded
		turns = append(turns, turn)
	}
	if !decodedAny {
		return nil, fmt.Errorf(
			"%w: no decodable turn (latestRootBlobId=%s)",
			errCursorStoreFormat, meta.LatestRootBlobID,
		)
	}
	return turns, nil
}

func loadCursorStoreMeta(
	ctx context.Context, q cursorIDEQuerier, agentID string,
) (cursorStoreMetaJSON, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, "0").Scan(&raw)
	if err == sql.ErrNoRows {
		return cursorStoreMetaJSON{}, fmt.Errorf(
			"%w: missing metadata key 0 for %s", errCursorStoreFormat, agentID,
		)
	}
	if err != nil {
		return cursorStoreMetaJSON{}, fmt.Errorf(
			"cursor store: reading metadata for %s: %w", agentID, err,
		)
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return cursorStoreMetaJSON{}, fmt.Errorf(
			"%w: metadata key 0 is not hex for %s: %w", errCursorStoreFormat, agentID, err,
		)
	}
	var meta cursorStoreMetaJSON
	if err := json.Unmarshal(decoded, &meta); err != nil {
		return cursorStoreMetaJSON{}, fmt.Errorf(
			"%w: metadata key 0 is not JSON for %s: %w", errCursorStoreFormat, agentID, err,
		)
	}
	if meta.AgentID == "" || meta.LatestRootBlobID == "" {
		return cursorStoreMetaJSON{}, fmt.Errorf(
			"%w: metadata key 0 missing agentId or latestRootBlobId for %s",
			errCursorStoreFormat, agentID,
		)
	}
	if meta.AgentID != agentID {
		return cursorStoreMetaJSON{}, fmt.Errorf(
			"%w: metadata agentId %q does not match %s",
			errCursorStoreFormat, meta.AgentID, agentID,
		)
	}
	return meta, nil
}

type cursorStoreBlobLoader struct {
	ctx   context.Context
	q     cursorIDEQuerier
	cache map[string][]byte
	err   error
}

func newCursorStoreBlobLoader(
	ctx context.Context, q cursorIDEQuerier,
) *cursorStoreBlobLoader {
	return &cursorStoreBlobLoader{
		ctx:   ctx,
		q:     q,
		cache: make(map[string][]byte),
	}
}

func (l *cursorStoreBlobLoader) load(id string) ([]byte, bool) {
	if id == "" {
		return nil, false
	}
	if data, ok := l.cache[id]; ok {
		return data, true
	}
	var data []byte
	err := l.q.QueryRowContext(
		l.ctx, `SELECT data FROM blobs WHERE id = ?`, id,
	).Scan(&data)
	if err != nil {
		if err != sql.ErrNoRows && l.err == nil {
			l.err = err
		}
		return nil, false
	}
	l.cache[id] = data
	return data, true
}

func cursorStoreResolveMessage(
	f agProtoField, loader *cursorStoreBlobLoader,
) ([]agProtoField, bool) {
	if f.Wire != pbWireBytes {
		return nil, false
	}
	if len(f.Bytes) == 32 {
		id := hex.EncodeToString(f.Bytes)
		if data, ok := loader.load(id); ok {
			if fields, err := agProtoParse(data); err == nil {
				return fields, true
			}
		}
	}
	if f.Nested != nil {
		return f.Nested, true
	}
	fields, err := agProtoParse(f.Bytes)
	if err != nil {
		return nil, false
	}
	return fields, true
}

func decodeCursorStoreTurn(
	fields []agProtoField, loader *cursorStoreBlobLoader,
) (cursorStoreTurn, bool) {
	var turn cursorStoreTurn
	decoded := false
	for _, f := range fields {
		switch f.Number {
		case 1:
			msgFields, ok := cursorStoreResolveMessage(f, loader)
			if !ok {
				continue
			}
			if text, ts, ok := decodeCursorStoreUserMessage(msgFields); ok {
				turn.UserDecoded = true
				turn.UserText = text
				turn.UserTime = ts
				decoded = true
			}
		case 2:
			msgFields, ok := cursorStoreResolveMessage(f, loader)
			if !ok {
				continue
			}
			if text, ts, ok := decodeCursorStoreReasoning(msgFields); ok {
				if turn.ReasoningText != "" {
					turn.ReasoningText += "\n\n"
				}
				turn.ReasoningText += text
				if turn.ReasoningTime.IsZero() || ts.After(turn.ReasoningTime) {
					turn.ReasoningTime = ts
				}
				decoded = true
				continue
			}
			if text, ts, ok := decodeCursorStoreAssistantMessage(msgFields); ok {
				turn.AssistantDecoded = true
				turn.AssistantText = text
				turn.AssistantTime = ts
				decoded = true
			}
		}
	}
	return turn, decoded
}

func decodeCursorStoreUserMessage(fields []agProtoField) (string, time.Time, bool) {
	textField, ok := agProtoFind(fields, 1)
	if !ok {
		return "", time.Time{}, false
	}
	text, ok := agProtoString(textField)
	if !ok || strings.TrimSpace(text) == "" {
		return "", time.Time{}, false
	}
	var ts time.Time
	if f, ok := agProtoFind(fields, 25); ok {
		if t, ok := cursorStoreTimeMS(f.Varint); ok {
			ts = t
		}
	}
	if ts.IsZero() {
		if f, ok := agProtoFind(fields, 26); ok {
			if t, ok := cursorStoreTimeMS(f.Varint); ok {
				ts = t
			}
		}
	}
	return text, ts, true
}

func decodeCursorStoreReasoning(fields []agProtoField) (string, time.Time, bool) {
	block, ok := agProtoFind(fields, 3)
	if !ok {
		return "", time.Time{}, false
	}
	inner := block.Nested
	if inner == nil && len(block.Bytes) > 0 {
		var err error
		inner, err = agProtoParse(block.Bytes)
		if err != nil {
			return "", time.Time{}, false
		}
	}
	if inner == nil {
		return "", time.Time{}, false
	}
	textField, ok := agProtoFind(inner, 1)
	if !ok {
		return "", time.Time{}, false
	}
	text, ok := agProtoString(textField)
	if !ok || strings.TrimSpace(text) == "" {
		return "", time.Time{}, false
	}
	var ts time.Time
	if f, ok := agProtoFind(inner, 4); ok {
		if t, ok := cursorStoreTimeMS(f.Varint); ok {
			ts = t
		}
	}
	if ts.IsZero() {
		if f, ok := agProtoFind(inner, 3); ok {
			if t, ok := cursorStoreTimeMS(f.Varint); ok {
				ts = t
			}
		}
	}
	if ts.IsZero() {
		return "", time.Time{}, false
	}
	return text, ts, true
}

func decodeCursorStoreAssistantMessage(fields []agProtoField) (string, time.Time, bool) {
	block, ok := agProtoFind(fields, 1)
	if !ok {
		return "", time.Time{}, false
	}
	inner := block.Nested
	if inner == nil && len(block.Bytes) > 0 {
		var err error
		inner, err = agProtoParse(block.Bytes)
		if err != nil {
			return "", time.Time{}, false
		}
	}
	if inner == nil {
		return "", time.Time{}, false
	}
	textField, ok := agProtoFind(inner, 1)
	if !ok {
		return "", time.Time{}, false
	}
	text, ok := agProtoString(textField)
	if !ok || strings.TrimSpace(text) == "" {
		return "", time.Time{}, false
	}
	var ts time.Time
	if f, ok := agProtoFind(inner, 2); ok {
		if t, ok := cursorStoreTimeMS(f.Varint); ok {
			ts = t
		}
	}
	return text, ts, true
}

func cursorStoreTimeMS(v uint64) (time.Time, bool) {
	// Producer millisecond stamps in the captured Cursor CLI store sit near
	// 1.7e12 (2026). Reject second-scale and absurd values so inferred times
	// are not presented as exact producer stamps.
	if v < 1_000_000_000_000 || v > 99_999_999_999_999 {
		return time.Time{}, false
	}
	if v > uint64(^uint64(0)>>1) {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(v)).UTC(), true
}

func applyCursorStoreTurns(
	sess *ParsedSession, msgs []ParsedMessage, turns []cursorStoreTurn,
) {
	if sess == nil || len(msgs) == 0 {
		return
	}
	mi := 0
	mappedPairs := 0
	expectedUsers := 0
	expectedAssistants := 0
	for _, msg := range msgs {
		switch msg.Role {
		case RoleUser:
			expectedUsers++
		case RoleAssistant:
			expectedAssistants++
		}
	}
	expectedPairs := max(expectedUsers, expectedAssistants)
	var started, ended time.Time
	note := func(ts time.Time) {
		if ts.IsZero() {
			return
		}
		if started.IsZero() || ts.Before(started) {
			started = ts
		}
		if ended.IsZero() || ts.After(ended) {
			ended = ts
		}
	}
	for _, turn := range turns {
		if !turn.UserDecoded || !turn.AssistantDecoded {
			knownIndex, known := cursorStoreKnownMessage(msgs, mi, turn)
			if !known {
				break
			}
			mi = knownIndex + 1
			continue
		}
		userIndex, assistantIndex, ok := cursorStoreTranscriptPair(
			msgs, mi, turn,
		)
		if !ok {
			break
		}
		mi = assistantIndex + 1
		if !turn.UserTime.IsZero() {
			msgs[userIndex].Timestamp = turn.UserTime
			note(turn.UserTime)
		}
		if turn.ReasoningText != "" {
			msgs[assistantIndex].ThinkingText = turn.ReasoningText
			msgs[assistantIndex].HasThinking = true
		}
		switch {
		case !turn.AssistantTime.IsZero():
			msgs[assistantIndex].Timestamp = turn.AssistantTime
			note(turn.AssistantTime)
		case !turn.ReasoningTime.IsZero():
			msgs[assistantIndex].Timestamp = turn.ReasoningTime
			note(turn.ReasoningTime)
		}
		note(turn.ReasoningTime)
		if !turn.UserTime.IsZero() && !turn.AssistantTime.IsZero() {
			mappedPairs++
		}
	}
	if !started.IsZero() {
		if mappedPairs == expectedPairs {
			sess.StartedAt = started
			sess.EndedAt = ended
		} else {
			if sess.StartedAt.IsZero() || started.Before(sess.StartedAt) {
				sess.StartedAt = started
			}
			if sess.EndedAt.IsZero() || ended.After(sess.EndedAt) {
				sess.EndedAt = ended
			}
		}
	}
}

func cursorStoreKnownMessage(
	msgs []ParsedMessage, start int, turn cursorStoreTurn,
) (int, bool) {
	if turn.UserText != "" {
		want := strings.TrimSpace(turn.UserText)
		for i := start; i < len(msgs); i++ {
			if msgs[i].Role == RoleUser &&
				strings.TrimSpace(msgs[i].Content) == want {
				return i, true
			}
		}
	}
	if turn.AssistantText != "" {
		want := strings.TrimSpace(turn.AssistantText)
		for i := start; i < len(msgs); i++ {
			if msgs[i].Role == RoleAssistant &&
				strings.TrimSpace(msgs[i].Content) == want {
				return i, true
			}
		}
	}
	return 0, false
}

func cursorStoreTranscriptPair(
	msgs []ParsedMessage, start int, turn cursorStoreTurn,
) (int, int, bool) {
	for userIndex := start; userIndex < len(msgs); userIndex++ {
		if msgs[userIndex].Role != RoleUser {
			continue
		}
		if turn.UserText != "" &&
			strings.TrimSpace(msgs[userIndex].Content) != strings.TrimSpace(turn.UserText) {
			continue
		}
		for assistantIndex := userIndex + 1; assistantIndex < len(msgs); assistantIndex++ {
			if msgs[assistantIndex].Role == RoleUser {
				break
			}
			if msgs[assistantIndex].Role != RoleAssistant {
				continue
			}
			if turn.AssistantText != "" &&
				strings.TrimSpace(msgs[assistantIndex].Content) != strings.TrimSpace(turn.AssistantText) {
				continue
			}
			return userIndex, assistantIndex, true
		}
	}
	return 0, 0, false
}

func cursorStorePathsUnderChats(chatsRoot string) (map[string]string, error) {
	paths := make(map[string]string)
	if chatsRoot == "" {
		return paths, nil
	}
	entries, err := os.ReadDir(chatsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return paths, nil
		}
		return nil, fmt.Errorf("read Cursor chats root %s: %w", chatsRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		agentDirs, readErr := os.ReadDir(
			filepath.Join(chatsRoot, entry.Name()),
		)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf(
				"read Cursor workspace %s: %w",
				filepath.Join(chatsRoot, entry.Name()), readErr,
			)
		}
		for _, agentEntry := range agentDirs {
			if !agentEntry.IsDir() || !IsValidSessionID(agentEntry.Name()) {
				continue
			}
			candidate := filepath.Join(
				chatsRoot, entry.Name(), agentEntry.Name(), "store.db",
			)
			if cursorStorePathIsValid(chatsRoot, candidate) {
				paths[agentEntry.Name()] = candidate
			}
		}
	}
	return paths, nil
}

func cursorStorePathIsValid(chatsRoot, storePath string) bool {
	info, err := os.Stat(storePath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(chatsRoot)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(storePath)
	return err == nil && isContainedIn(resolved, resolvedRoot)
}

func cursorStoreAgentIDFromPath(path string) (string, bool) {
	base := filepath.Base(path)
	switch base {
	case "store.db", "store.db-wal":
		agentID := filepath.Base(filepath.Dir(path))
		if IsValidSessionID(agentID) {
			return agentID, true
		}
	}
	return "", false
}

func cursorRawIDFromTranscriptPath(path string) string {
	id := CursorSessionID(path)
	return strings.TrimPrefix(id, "cursor:")
}
