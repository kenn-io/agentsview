package parser

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
)

const (
	codexCursorCacheMaxEntries = 256
	codexCursorCacheMaxBytes   = 2 << 20
	codexCursorMaxPendingCalls = 8
	// codexCursorCheckpointVersion is the wire version for the persisted
	// cursor encoding. Bump when the encoding changes; decode failures fall
	// back to a full parse.
	// The fork replay gate is process-only state: it is re-armed from the
	// transcript on every parse and is not part of the persisted cursor.
	codexCursorCheckpointVersion   = 2
	codexCursorCheckpointMaxString = 1 << 20

	// Account for the map bucket, list element, pointers, string headers, and
	// allocator overhead that are not represented by the variable-length path
	// and cursor strings below. The fixed pending-call array contributes two
	// string headers, occurrence coordinates, and flags per slot. The cache is
	// intentionally an estimate rather
	// than a heap profiler, but this conservative allowance keeps its retained
	// memory bounded near the configured byte limit.
	codexCursorEntryOverheadBytes = 256 + codexCursorMaxPendingCalls*64
)

type codexPendingToolCall struct {
	id             string
	name           string
	messageOrdinal int
	callIndex      int
	positionKnown  bool
}

// codexCursorState is the compact state needed to make a tail parse behave as
// though the already-persisted prefix had just been scanned. It deliberately
// excludes parsed messages, raw transcript data, tool maps, and open files.
type codexCursorState struct {
	model                    string
	cwd                      string
	agentPath                string
	firstUserDigest          [sha256.Size]byte
	firstUserSeen            bool
	sawUserTurnAfterFirst    bool
	mayReplayFirstUserPrompt bool
	lastTokenUsageDigest     [sha256.Size]byte
	lastTokenUsageSeen       bool
	forkGate                 codexForkGate
	lastTaskEvent            string
	pendingCalls             [codexCursorMaxPendingCalls]codexPendingToolCall
	pendingCallCount         uint8
	pendingCallsOverflow     bool
}

// MarshalBinary encodes the compact continuation state for persistence.
// The encoding is versioned and intentionally bounded: strings are
// length-prefixed and capped on decode, and the pending-call array is
// fixed-size.
func (s *codexCursorState) MarshalBinary() ([]byte, error) {
	if s.pendingCallsOverflow {
		return nil, fmt.Errorf(
			"codex cursor has more than %d unresolved tool calls",
			codexCursorMaxPendingCalls,
		)
	}
	var buf bytes.Buffer
	write := func(v any) error {
		return binary.Write(&buf, binary.LittleEndian, v)
	}
	writeStr := func(str string) error {
		if err := write(uint32(len(str))); err != nil {
			return err
		}
		_, err := buf.WriteString(str)
		return err
	}
	if err := write(uint8(codexCursorCheckpointVersion)); err != nil {
		return nil, err
	}
	for _, str := range []string{s.model, s.cwd, s.agentPath} {
		if err := writeStr(str); err != nil {
			return nil, err
		}
	}
	if err := write(s.firstUserDigest); err != nil {
		return nil, err
	}
	flags := uint8(0)
	if s.firstUserSeen {
		flags |= 1 << 0
	}
	if s.sawUserTurnAfterFirst {
		flags |= 1 << 1
	}
	if s.mayReplayFirstUserPrompt {
		flags |= 1 << 2
	}
	if s.lastTokenUsageSeen {
		flags |= 1 << 3
	}
	if err := write(flags); err != nil {
		return nil, err
	}
	if err := write(s.lastTokenUsageDigest); err != nil {
		return nil, err
	}
	if err := writeStr(s.lastTaskEvent); err != nil {
		return nil, err
	}
	if err := write(uint8(s.pendingCallCount)); err != nil {
		return nil, err
	}
	for i := 0; i < int(s.pendingCallCount); i++ {
		pending := s.pendingCalls[i]
		if err := writeStr(pending.id); err != nil {
			return nil, err
		}
		if err := writeStr(pending.name); err != nil {
			return nil, err
		}
		positionKnown := uint8(0)
		if pending.positionKnown {
			positionKnown = 1
		}
		if err := write(positionKnown); err != nil {
			return nil, err
		}
		if err := write(int64(pending.messageOrdinal)); err != nil {
			return nil, err
		}
		if err := write(int32(pending.callIndex)); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// UnmarshalBinary restores the state written by MarshalBinary. Any version,
// length, or structure mismatch returns an error so the caller falls back
// to an authoritative full parse.
func (s *codexCursorState) UnmarshalBinary(data []byte) error {
	r := bytes.NewReader(data)
	read := func(v any) error {
		return binary.Read(r, binary.LittleEndian, v)
	}
	readStr := func() (string, error) {
		var n uint32
		if err := read(&n); err != nil {
			return "", err
		}
		if n > codexCursorCheckpointMaxString {
			return "", fmt.Errorf(
				"codex cursor string length %d exceeds bound %d",
				n, codexCursorCheckpointMaxString,
			)
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return string(b), nil
	}

	var version uint8
	if err := read(&version); err != nil {
		return err
	}
	if version != codexCursorCheckpointVersion {
		return fmt.Errorf(
			"unsupported codex cursor version %d", version,
		)
	}
	*s = codexCursorState{}
	var err error
	if s.model, err = readStr(); err != nil {
		return err
	}
	if s.cwd, err = readStr(); err != nil {
		return err
	}
	if s.agentPath, err = readStr(); err != nil {
		return err
	}
	if err := read(&s.firstUserDigest); err != nil {
		return err
	}
	var flags uint8
	if err := read(&flags); err != nil {
		return err
	}
	s.firstUserSeen = flags&(1<<0) != 0
	s.sawUserTurnAfterFirst = flags&(1<<1) != 0
	s.mayReplayFirstUserPrompt = flags&(1<<2) != 0
	s.lastTokenUsageSeen = flags&(1<<3) != 0
	if err := read(&s.lastTokenUsageDigest); err != nil {
		return err
	}
	if s.lastTaskEvent, err = readStr(); err != nil {
		return err
	}
	var count uint8
	if err := read(&count); err != nil {
		return err
	}
	if count > codexCursorMaxPendingCalls {
		return fmt.Errorf(
			"codex cursor pending call count %d exceeds bound %d",
			count, codexCursorMaxPendingCalls,
		)
	}
	s.pendingCallCount = count
	for i := 0; i < int(count); i++ {
		pending := &s.pendingCalls[i]
		if pending.id, err = readStr(); err != nil {
			return err
		}
		if pending.name, err = readStr(); err != nil {
			return err
		}
		var positionKnown uint8
		if err := read(&positionKnown); err != nil {
			return err
		}
		if positionKnown > 1 {
			return fmt.Errorf("invalid codex cursor position flag %d", positionKnown)
		}
		var messageOrdinal int64
		if err := read(&messageOrdinal); err != nil {
			return err
		}
		var callIndex int32
		if err := read(&callIndex); err != nil {
			return err
		}
		pending.positionKnown = positionKnown == 1
		pending.messageOrdinal = int(messageOrdinal)
		pending.callIndex = int(callIndex)
	}
	if r.Len() != 0 {
		return fmt.Errorf("codex cursor trailing bytes: %d", r.Len())
	}
	return nil
}

func (s *codexCursorState) rememberToolCall(
	id, name string, position *ParsedToolCallPosition,
) bool {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" || name == "" {
		return false
	}
	if int(s.pendingCallCount) >= len(s.pendingCalls) {
		s.pendingCallsOverflow = true
		return false
	}
	pending := codexPendingToolCall{id: id, name: name}
	if position != nil {
		pending.messageOrdinal = position.MessageOrdinal
		pending.callIndex = position.CallIndex
		pending.positionKnown = true
	}
	s.pendingCalls[s.pendingCallCount] = pending
	s.pendingCallCount++
	return true
}

func (s *codexCursorState) toolCallName(id string) (string, bool) {
	for i := 0; i < int(s.pendingCallCount); i++ {
		if s.pendingCalls[i].id == id {
			return s.pendingCalls[i].name, true
		}
	}
	return "", false
}

func (s *codexCursorState) toolCallPosition(
	id string,
) (*ParsedToolCallPosition, bool) {
	for i := 0; i < int(s.pendingCallCount); i++ {
		pending := s.pendingCalls[i]
		if pending.id != id {
			continue
		}
		if !pending.positionKnown {
			return nil, false
		}
		return &ParsedToolCallPosition{
			MessageOrdinal: pending.messageOrdinal,
			CallIndex:      pending.callIndex,
		}, true
	}
	return nil, false
}

func (s *codexCursorState) clearPendingCallPositions() {
	for i := 0; i < int(s.pendingCallCount); i++ {
		s.pendingCalls[i].positionKnown = false
	}
}

func (s *codexCursorState) forgetToolCall(id string) {
	for i := 0; i < int(s.pendingCallCount); i++ {
		if s.pendingCalls[i].id != id {
			continue
		}
		last := int(s.pendingCallCount) - 1
		copy(s.pendingCalls[i:last], s.pendingCalls[i+1:last+1])
		s.pendingCalls[last] = codexPendingToolCall{}
		s.pendingCallCount--
		return
	}
}

// observeUserPrompt advances the first-user replay state using only a digest
// of the full normalized prompt. first reports the initial genuine prompt;
// replay reports the one positively identified post-abort re-emission that the
// caller must suppress.
func (s *codexCursorState) observeUserPrompt(content string) (first, replay bool) {
	digest := sha256.Sum256([]byte(content))
	if !s.firstUserSeen {
		s.firstUserDigest = digest
		s.firstUserSeen = true
		return true, false
	}
	if digest == s.firstUserDigest &&
		!s.sawUserTurnAfterFirst &&
		s.mayReplayFirstUserPrompt {
		s.mayReplayFirstUserPrompt = false
		return false, true
	}
	s.sawUserTurnAfterFirst = true
	s.mayReplayFirstUserPrompt = false
	return false, false
}

func (s *codexCursorState) markFirstUserReplayPossible() {
	if !s.firstUserSeen || s.sawUserTurnAfterFirst {
		return
	}
	s.mayReplayFirstUserPrompt = true
}

func (s *codexCursorState) observeTaskEvent(eventType string) {
	switch eventType {
	case "task_started", "task_complete", "turn_aborted":
		s.lastTaskEvent = eventType
		if eventType == "turn_aborted" {
			s.markFirstUserReplayPossible()
		}
	}
}

// observeTokenUsage records the exact streaming token payload compactly and
// reports whether it repeats the most recently observed payload.
func (s *codexCursorState) observeTokenUsage(raw string) bool {
	digest := sha256.Sum256([]byte(raw))
	duplicate := s.lastTokenUsageSeen && digest == s.lastTokenUsageDigest
	s.lastTokenUsageDigest = digest
	s.lastTokenUsageSeen = true
	return duplicate
}

type codexCursorKey struct {
	path   string
	offset int64
	inode  uint64
	device uint64
}

type codexCursorEntry struct {
	key   codexCursorKey
	state codexCursorState
	bytes int64
}

// codexCursorCache is a concurrency-safe LRU keyed by an exact physical-file
// version and safe resume offset. Multiple offsets for the same file coexist;
// the caller's persisted offset decides which version is eligible.
type codexCursorCache struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int64
	totalBytes int64
	entries    map[codexCursorKey]*list.Element
	recent     *list.List
}

func newCodexCursorCache(maxEntries int, maxBytes int64) *codexCursorCache {
	return &codexCursorCache{
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		entries:    make(map[codexCursorKey]*list.Element),
		recent:     list.New(),
	}
}

func newProductionCodexCursorCache() *codexCursorCache {
	return newCodexCursorCache(
		codexCursorCacheMaxEntries,
		codexCursorCacheMaxBytes,
	)
}

func (c *codexCursorCache) Get(
	path string,
	offset int64,
	inode uint64,
	device uint64,
) (codexCursorState, bool) {
	if c == nil {
		return codexCursorState{}, false
	}
	key := newCodexCursorKey(path, offset, inode, device)

	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return codexCursorState{}, false
	}
	c.recent.MoveToFront(elem)
	return elem.Value.(codexCursorEntry).state, true
}

// Put stages one exact cursor version. It returns false when the entry cannot
// fit by itself; an oversized replacement leaves any existing value intact.
func (c *codexCursorCache) Put(
	path string,
	offset int64,
	inode uint64,
	device uint64,
	state codexCursorState,
) bool {
	if c == nil || c.maxEntries <= 0 || c.maxBytes <= 0 {
		return false
	}
	key := codexCursorKey{
		path:   filepath.Clean(path),
		offset: offset,
		inode:  inode,
		device: device,
	}
	entryBytes := estimateCodexCursorEntryBytes(key, state)
	if entryBytes > c.maxBytes {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.entries[key]; ok {
		entry := elem.Value.(codexCursorEntry)
		if entry.state == state {
			c.recent.MoveToFront(elem)
			return true
		}
		state = cloneCodexCursorState(state)
		c.totalBytes -= entry.bytes
		entry.state = state
		entry.bytes = entryBytes
		elem.Value = entry
		c.totalBytes += entryBytes
		c.recent.MoveToFront(elem)
		c.evictLocked()
		return true
	}

	key.path = strings.Clone(key.path)
	state = cloneCodexCursorState(state)
	entry := codexCursorEntry{key: key, state: state, bytes: entryBytes}
	elem := c.recent.PushFront(entry)
	c.entries[key] = elem
	c.totalBytes += entryBytes
	c.evictLocked()
	return true
}

func (c *codexCursorCache) evictLocked() {
	for len(c.entries) > c.maxEntries || c.totalBytes > c.maxBytes {
		elem := c.recent.Back()
		if elem == nil {
			return
		}
		entry := elem.Value.(codexCursorEntry)
		delete(c.entries, entry.key)
		c.totalBytes -= entry.bytes
		c.recent.Remove(elem)
	}
}

func newCodexCursorKey(
	path string,
	offset int64,
	inode uint64,
	device uint64,
) codexCursorKey {
	return codexCursorKey{
		path:   strings.Clone(filepath.Clean(path)),
		offset: offset,
		inode:  inode,
		device: device,
	}
}

func cloneCodexCursorState(state codexCursorState) codexCursorState {
	state.model = strings.Clone(state.model)
	state.cwd = strings.Clone(state.cwd)
	state.agentPath = strings.Clone(state.agentPath)
	state.lastTaskEvent = strings.Clone(state.lastTaskEvent)
	for i := 0; i < int(state.pendingCallCount); i++ {
		state.pendingCalls[i].id = strings.Clone(state.pendingCalls[i].id)
		state.pendingCalls[i].name = strings.Clone(state.pendingCalls[i].name)
	}
	state.forkGate.parentSessionID = strings.Clone(
		state.forkGate.parentSessionID,
	)
	return state
}

func estimateCodexCursorEntryBytes(
	key codexCursorKey,
	state codexCursorState,
) int64 {
	return codexCursorEntryOverheadBytes + int64(
		len(key.path)+
			len(state.model)+
			len(state.cwd)+
			len(state.agentPath)+
			len(state.lastTaskEvent)+
			codexPendingCallStringBytes(state)+
			len(state.forkGate.parentSessionID),
	)
}

func codexPendingCallStringBytes(state codexCursorState) int {
	total := 0
	for i := 0; i < int(state.pendingCallCount); i++ {
		total += len(state.pendingCalls[i].id) + len(state.pendingCalls[i].name)
	}
	return total
}
