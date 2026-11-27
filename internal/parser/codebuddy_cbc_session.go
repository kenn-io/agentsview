package parser

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// codebuddyCBCIdentity names the sessions one transcript produces.
type codebuddyCBCIdentity struct {
	// rawID is the archive identity without the codebuddy: prefix.
	rawID string
	// parentRawID links a subagent transcript to its parent session.
	parentRawID string
	// runtimeID is the CLI session id used by codebuddy --resume.
	runtimeID string
	subagent  bool
}

// codebuddyCBCIdentityForPath derives archive identity from the transcript
// layout. Main transcripts use the filename stem; subagents nest under the
// parent session directory. Converted 4.x sessions keep their 4.x id, which
// is also their CBC filename stem.
func codebuddyCBCIdentityForPath(path string) codebuddyCBCIdentity {
	_, sessionStem, agentStem := codebuddyCBCPathParts(path)
	if agentStem != "" {
		return codebuddyCBCIdentity{
			rawID:       sessionStem + ":subagent:" + agentStem,
			parentRawID: sessionStem,
			runtimeID:   agentStem,
			subagent:    true,
		}
	}
	return codebuddyCBCIdentity{rawID: sessionStem, runtimeID: sessionStem}
}

// codebuddyCBCParseInput carries everything one transcript parse needs.
type codebuddyCBCParseInput struct {
	path    string
	project string
	machine string
	opts    codebuddyCBCBuildOptions
	catalog *codebuddyCatalogEntry
	// legacyRoots are the configured 4.x roots, read for a converted
	// conversation's request totals and pasted images.
	legacyRoots []string
}

// parseCodeBuddyCBCFile parses one CBC transcript into its active session and
// any preserved alternative branches. A transcript with no conversation
// returns no results. A conversation rewound before its first message has no
// active session, but its earlier history still returns as branches.
func parseCodeBuddyCBCFile(
	ctx context.Context, in codebuddyCBCParseInput,
) ([]ParseResult, error) {
	t, err := readCodeBuddyCBCTranscript(ctx, in.path)
	if err != nil {
		return nil, err
	}
	ident := codebuddyCBCIdentityForPath(in.path)
	// A transcript with no conversation item does not replace the 4.x copy
	// of the same conversation, which still holds its turns.
	if !ident.subagent && !t.hasConversation() &&
		codebuddyLegacySessionDir(ctx, in.legacyRoots, t.cwd, ident.rawID) != "" {
		return nil, nil
	}
	if t.sessionID != "" && !ident.subagent {
		ident.runtimeID = t.sessionID
	}
	var legacyOrder []string
	legacy := t.converted && !ident.subagent
	// Without its full 4.x directory a converted conversation lacks request
	// totals and pasted images only that directory holds.
	companionIncomplete := legacy
	if legacy && len(in.legacyRoots) > 0 {
		companion := loadCodeBuddyLegacyCompanion(ctx, in.legacyRoots, t.cwd, ident.rawID)
		companionIncomplete = !companion.complete
		in.opts.legacyUsage = codebuddyLegacyUsageMissingFrom(companion.usage, t.items)
		legacyOrder = companion.order
		if blobs, ok := in.opts.assets.(codebuddyBlobAssets); ok {
			blobs.legacyAssets = companion.assetsDir
			in.opts.assets = blobs
		} else if in.opts.assets == nil && companion.assetsDir != "" {
			in.opts.assets = codebuddyBlobAssets{legacyAssets: companion.assetsDir}
		}
	}
	if ident.subagent {
		in.opts.taskAssignment, in.opts.hasTaskAssignment = codebuddyTeamTaskAssignment(t.items)
	}
	active := codebuddyCBCReplay(t.items)
	main, err := buildCodeBuddyCBCMessages(ctx, t.items, active, in.opts)
	if err != nil {
		return nil, err
	}
	main.messages = codebuddyInsertLegacyUsage(main.messages, in.opts.legacyUsage,
		legacyOrder, main.legacyClaimed)
	codebuddyFillUsageModels(main.messages, "")
	project := in.project
	if t.cwd != "" {
		if extracted := ExtractProjectFromCwdWithBranchContext(ctx, t.cwd, ""); extracted != "" {
			project = extracted
		}
	}
	if project == "" {
		project = GetProjectName(in.project)
	}
	custom, generated := codebuddyCBCTitles(t.titleFacts)
	base := ParsedSession{
		ID:             "codebuddy:" + ident.rawID,
		Project:        project,
		Machine:        in.machine,
		Agent:          AgentCodeBuddy,
		Entrypoint:     t.entrypoint,
		Cwd:            t.cwd,
		MalformedLines: t.malformed,
		IsTruncated:    t.truncated,
		File:           codebuddyCBCFileInfo(in.path),
	}
	if !ident.subagent {
		base.SourceSessionID = ident.runtimeID
	}
	if t.teamMember != "" {
		base.AgentLabel = t.teamMember
	} else if ident.subagent {
		base.AgentLabel = t.speaker
	}
	if ident.subagent {
		base.ParentSessionID = "codebuddy:" + ident.parentRawID
		base.RelationshipType = RelSubagent
		base.SourceSessionID = ident.runtimeID
	}
	title := firstNonEmptyJSONLString(custom, generated)
	if in.catalog != nil {
		title = firstNonEmptyJSONLString(in.catalog.customTitle, custom,
			in.catalog.title, generated)
		parent, err := codebuddyCBCForkParent(ctx, in.path, in.catalog)
		if err != nil {
			return nil, err
		}
		if parent != "" {
			base.ParentSessionID = "codebuddy:" + parent
			base.RelationshipType = RelFork
		}
	}
	var results []ParseResult
	// An empty active session is left out: the sidebar hides an empty
	// session together with its children, so emitting one would also hide
	// the preserved branches. The branches still name it as their parent and
	// nest under it once the conversation continues.
	if len(main.messages) > 0 || t.truncated {
		mainSess := base
		codebuddyFinishSession(&mainSess, main, title)
		mainSess.LegacyCompanionIncomplete = companionIncomplete ||
			(legacy && main.legacyImageMissing)
		mainSess.StartedAt = earlierTime(t.firstTime, main.startedAt)
		mainSess.EndedAt = laterTime(t.lastTime, main.endedAt)
		mainSess.TerminationStatus = Classify(main.messages,
			codebuddyStopReason(t.stopReason, main.messages), t.truncated)
		results = append(results, ParseResult{Session: mainSess, Messages: main.messages})
	}
	// A call in a preserved branch names its child transcript only in that
	// branch's result, so every path contributes its mappings.
	calls := main.subagentCalls
	for _, branch := range codebuddyCBCAlternatives(t.items, active) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		alt, err := buildCodeBuddyCBCMessages(ctx, t.items, branch.indexes, in.opts)
		if err != nil {
			return nil, err
		}
		// A branch with no prompt or visible reply is kept when it holds
		// billed usage, such as a rewound empty response, so its tokens
		// still count; each usage row keeps its response identity, which
		// counts it once.
		if alt.userCount == 0 && !codebuddyHasAssistantContent(alt.messages) &&
			!codebuddyHasUsage(alt.messages) {
			continue
		}
		for callID, agentID := range alt.subagentCalls {
			if _, ok := calls[callID]; !ok {
				calls[callID] = agentID
			}
		}
		fork := base
		fork.ID = base.ID + "-" + branch.key
		fork.ParentSessionID = base.ID
		fork.RelationshipType = RelFork
		fork.SourceSessionID = ""
		fork.IsTruncated = false
		codebuddyFillUsageModels(alt.messages, codebuddyModelBefore(t.items, branch.indexes))
		codebuddyFinishSession(&fork, alt, "")
		fork.LegacyCompanionIncomplete = companionIncomplete ||
			(legacy && alt.legacyImageMissing)
		fork.StartedAt = alt.startedAt
		fork.EndedAt = alt.endedAt
		fork.TerminationStatus = Classify(alt.messages,
			codebuddyStopReason("", alt.messages), false)
		results = append(results, ParseResult{Session: fork, Messages: alt.messages})
	}
	if len(results) == 0 {
		return nil, nil
	}
	codebuddyLinkSubagentCalls(results, ident, calls)
	return results, nil
}

func codebuddyHasAssistantContent(msgs []ParsedMessage) bool {
	for _, msg := range msgs {
		if msg.Role == RoleAssistant && !msg.IsSystem && strings.TrimSpace(msg.Content) != "" {
			return true
		}
	}
	return false
}

func codebuddyHasUsage(msgs []ParsedMessage) bool {
	for _, msg := range msgs {
		if len(msg.TokenUsage) > 0 {
			return true
		}
	}
	return false
}

// codebuddyModelBefore returns the model the turns a rewound branch shares
// with the conversation last named, which the branch's own items leave out:
// it follows the parent chain from the branch's first item through items the
// branch does not hold. The walk is bounded by the transcript length, so a
// looping chain ends.
func codebuddyModelBefore(items []codebuddyCBCItem, branch []int) string {
	if len(branch) == 0 {
		return ""
	}
	held := make(map[int]bool, len(branch))
	for _, idx := range branch {
		held[idx] = true
	}
	byID := make(map[string]int, len(items))
	for i, it := range items {
		if _, ok := byID[it.id]; !ok && it.id != "" {
			byID[it.id] = i
		}
	}
	cur := branch[0]
	for range items {
		it := items[cur]
		parent := firstNonEmptyJSONLString(it.parentID, it.logicalParentID)
		next, ok := byID[parent]
		if !ok || next >= cur {
			return ""
		}
		cur = next
		if held[cur] {
			continue
		}
		if model := codebuddyCBCModel(items[cur].providerData()); model != "" {
			return model
		}
	}
	return ""
}

// codebuddyCBCForkParent returns the archive id of the conversation a main
// transcript's catalog row names as its fork parent, or "" when the row names
// none. A parent that arrives after the branch, under a filename that differs
// from the id the catalog records, changes the result while the branch's own
// files stay the same, so both freshness checks include it.
func codebuddyCBCForkParent(
	ctx context.Context, path string, entry *codebuddyCatalogEntry,
) (string, error) {
	if entry == nil || entry.forkParentRuntimeID == "" ||
		codebuddyCBCIdentityForPath(path).subagent {
		return "", nil
	}
	// The parent's runtime ids are tried in catalog order, so the link
	// names a transcript that exists whenever one of them has one.
	for _, runtimeID := range entry.forkParentRuntimeIDs {
		rawID, found, err := codebuddyCBCRawIDForRuntime(ctx, path, runtimeID)
		if err != nil || found {
			return rawID, err
		}
	}
	return entry.forkParentRuntimeID, nil
}

// codebuddyCBCRawIDForRuntime returns the archive id of the main transcript
// that records runtimeID, searched under the projects root holding path. The
// IDE catalog names a conversation by the session id its transcript records,
// while the archive names it by the transcript's filename, and the two can
// differ. The transcript named after the id is checked first; otherwise the
// root's recorded-id index is consulted. found is false when no transcript
// other than path records the id.
//
// Cost: both freshness checks call this for every fork branch, the warm stat
// hash on every sync. The common case, a parent named after its id, costs a
// stat and a header read. Otherwise the index costs one stat per project
// directory while the layout is unchanged, and a header read of every main
// transcript in the root after a change (see codebuddyRecordedIDIndexes).
func codebuddyCBCRawIDForRuntime(
	ctx context.Context, path, runtimeID string,
) (rawID string, found bool, err error) {
	root, ok := codebuddyCBCRootFromPath(path)
	if !ok {
		return "", false, nil
	}
	projectDir, _, _ := codebuddyCBCPathParts(path)
	named := filepath.Join(projectDir, runtimeID+".jsonl")
	if named != filepath.Clean(path) && codebuddyCBCSourcePath(root, named) {
		info, err := os.Stat(named)
		if err == nil && info.Mode().IsRegular() &&
			readCodeBuddyCBCHeader(ctx, named).catalogKey(named) == runtimeID {
			return runtimeID, true, nil
		}
	}
	recorders, err := codebuddyCBCTranscriptsRecording(ctx, root, runtimeID)
	if err != nil {
		return "", false, err
	}
	for _, recorder := range recorders {
		if recorder != filepath.Clean(path) {
			return codebuddyCBCIdentityForPath(recorder).rawID, true, nil
		}
	}
	return "", false, nil
}

// codebuddyRecordedIDIndexes caches, per projects root, the main transcripts
// that record each session id, so resolving fork parents does not read every
// transcript header on each sync. An index stays valid while the metadata of
// the paths it watched is unchanged: the root (a project directory added or
// removed), each project directory (a transcript added, removed, or
// renamed), and each transcript whose header recorded no session id when
// read, whose id can still appear as the file is written. A transcript's
// recorded id is not expected to change once written. Each path is read
// before its contents are listed, so a change during a scan invalidates the
// index. Without change times or inodes nothing is cached.
var codebuddyRecordedIDIndexes = struct {
	sync.Mutex
	byRoot map[string]*codebuddyRecordedIDIndex
}{byRoot: make(map[string]*codebuddyRecordedIDIndex)}

// codebuddyRecordedIDIndexRoots bounds codebuddyRecordedIDIndexes. A daemon
// configures a few CodeBuddy homes; remote syncs add one root per import.
const codebuddyRecordedIDIndexRoots = 16

type codebuddyRecordedIDIndex struct {
	watched []string
	stamp   uint64
	byID    map[string][]string
}

// codebuddyCBCTranscriptsRecording returns the main transcripts under root
// whose catalog key, the session id they record, is runtimeID, in the order
// a root listing finds them.
func codebuddyCBCTranscriptsRecording(
	ctx context.Context, root, runtimeID string,
) ([]string, error) {
	codebuddyRecordedIDIndexes.Lock()
	idx := codebuddyRecordedIDIndexes.byRoot[root]
	codebuddyRecordedIDIndexes.Unlock()
	if idx != nil {
		h := fnv.New64a()
		valid := true
		for _, path := range idx.watched {
			tuple, ok := codebuddyStatTuple(path)
			if !ok {
				valid = false
				break
			}
			_, _ = h.Write(tuple[:])
		}
		if valid && h.Sum64() == idx.stamp {
			return idx.byID[runtimeID], nil
		}
	}
	idx, cacheable, err := scanCodeBuddyRecordedIDs(ctx, root)
	if err != nil {
		return nil, err
	}
	codebuddyRecordedIDIndexes.Lock()
	if cacheable {
		// Each remote sync extracts into a new temporary root, so the map
		// keeps at most a few roots, as the catalog cache does.
		if _, ok := codebuddyRecordedIDIndexes.byRoot[root]; !ok &&
			len(codebuddyRecordedIDIndexes.byRoot) >= codebuddyRecordedIDIndexRoots {
			clear(codebuddyRecordedIDIndexes.byRoot)
		}
		codebuddyRecordedIDIndexes.byRoot[root] = idx
	} else {
		delete(codebuddyRecordedIDIndexes.byRoot, root)
	}
	codebuddyRecordedIDIndexes.Unlock()
	return idx.byID[runtimeID], nil
}

// scanCodeBuddyRecordedIDs reads the header of every main transcript under
// root. It records each watched path's metadata before reading beneath it.
func scanCodeBuddyRecordedIDs(
	ctx context.Context, root string,
) (*codebuddyRecordedIDIndex, bool, error) {
	idx := &codebuddyRecordedIDIndex{byID: make(map[string][]string)}
	h := fnv.New64a()
	cacheable := true
	watch := func(path string, tuple [32]byte, ok bool) {
		idx.watched = append(idx.watched, path)
		_, _ = h.Write(tuple[:])
		cacheable = ok && cacheable
	}
	tuple, ok := codebuddyStatTuple(root)
	watch(root, tuple, ok)
	projects, _ := os.ReadDir(root)
	for _, project := range projects {
		if !isDirOrSymlink(project, root) {
			continue
		}
		dir := filepath.Join(root, project.Name())
		tuple, ok := codebuddyStatTuple(dir)
		watch(dir, tuple, ok)
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			path := filepath.Join(dir, entry.Name())
			if !entry.Type().IsRegular() || !codebuddyCBCSourcePath(root, path) {
				continue
			}
			// Stat before reading, so a header written after the read
			// invalidates the index.
			tuple, ok := codebuddyStatTuple(path)
			header := readCodeBuddyCBCHeader(ctx, path)
			if header.sessionID == "" {
				watch(path, tuple, ok)
			}
			key := header.catalogKey(path)
			idx.byID[key] = append(idx.byID[key], path)
		}
	}
	idx.stamp = h.Sum64()
	return idx, cacheable, ctx.Err()
}

// codebuddyStatTuple encodes path's size, modification and change times,
// and inode, or zeros when path is missing. It reports false when the change
// time or inode cannot be read.
func codebuddyStatTuple(path string) ([32]byte, bool) {
	var buf [32]byte
	var size, mtime, ctime int64
	var inode uint64
	if info, err := os.Stat(path); err == nil {
		var ok bool
		if ctime, ok = codexIndexChangeTime(path, info); !ok {
			return buf, false
		}
		if inode, _ = sourceFileIdentityForPath(path, info); inode == 0 {
			return buf, false
		}
		size, mtime = info.Size(), info.ModTime().UnixNano()
	}
	binary.LittleEndian.PutUint64(buf[:8], uint64(size))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(mtime))
	binary.LittleEndian.PutUint64(buf[16:24], uint64(ctime))
	binary.LittleEndian.PutUint64(buf[24:32], inode)
	return buf, true
}

// codebuddyStopReason prefers the producer's recorded terminal state and
// falls back to how the final assistant response ended.
func codebuddyStopReason(recorded string, msgs []ParsedMessage) string {
	if recorded != "" {
		return recorded
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant && !msgs[i].IsSystem {
			return msgs[i].StopReason
		}
	}
	return ""
}

func codebuddyFinishSession(sess *ParsedSession, built codebuddyCBCBuilt, title string) {
	sess.FirstMessage = built.firstMessage
	sess.MessageCount = len(built.messages)
	sess.UserMessageCount = built.userCount
	if title == "" && built.firstMessage != "" {
		title = truncate(built.firstMessage, 80)
	}
	sess.SessionName = title
	accumulateMessageTokenUsage(sess, built.messages)
}

// codebuddyLinkSubagentCalls points each Agent/Task call at the subagent
// session its result names.
func codebuddyLinkSubagentCalls(
	results []ParseResult, ident codebuddyCBCIdentity, calls map[string]string,
) {
	if len(calls) == 0 || ident.subagent {
		return
	}
	for r := range results {
		for m := range results[r].Messages {
			for c := range results[r].Messages[m].ToolCalls {
				call := &results[r].Messages[m].ToolCalls[c]
				agentID, ok := calls[call.ToolUseID]
				if !ok {
					continue
				}
				call.SubagentSessionID = "codebuddy:" + ident.rawID + ":subagent:" + agentID
			}
		}
	}
}

func codebuddyCBCFileInfo(path string) FileInfo {
	info := FileInfo{Path: path}
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		info.Size = st.Size()
		info.Mtime = st.ModTime().UnixNano()
	}
	return info
}
