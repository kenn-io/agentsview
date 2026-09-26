package parser

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
)

// Codex continues a long thread in a second rollout file instead of growing the
// first one past its history window. The continuation is named
// rollout-<timestamp>-<thread>_<rollout>.jsonl; its session_meta carries the
// thread's own id together with history_mode "paginated" and a history_base
// naming the thread and the entry ordinal the previous file stopped at, and its
// entries continue the thread's sequence without repeating any earlier entry.
//
// A continuation is therefore a companion of the thread's own rollout, the way
// session_index.jsonl is: the thread's first rollout stays the session's stored
// file_path, and the continuation's entries are appended to that one session.
// Treating it as a session source of its own instead would give two sources one
// session id, and whichever source the sync wrote last would decide which half
// of the thread the archive kept.
const (
	codexHistoryModePaginated = "paginated"

	// codexContinuationMetaScanLimit bounds how far into a candidate file the
	// paginated-history marker is looked for. Codex writes session_meta as the
	// first record; scanning a few lines tolerates a leading blank or a
	// malformed line without reading a multi-gigabyte rollout to decide a
	// filename question.
	codexContinuationMetaScanLimit = 8
)

// codexContinuationRe matches a continuation rollout filename stem:
// rollout-<timestamp>-<thread-uuid>_<rollout-uuid>. Capture 1 is the thread the
// file continues.
var codexContinuationRe = regexp.MustCompile(
	`^rollout-.*-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-` +
		`[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})` +
		`_([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-` +
		`[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`,
)

// CodexContinuationThreadUUIDFromFilename returns the thread UUID that a
// paginated continuation rollout filename continues, or "" when the name is not
// a continuation rollout filename.
//
// This is deliberately a separate resolver rather than a change to
// CodexSessionUUIDFromFilename, which must keep resolving a continuation
// filename to nothing. That function keys discovery
// (discoveredFileKey in internal/sync) and every stored-source lookup;
// resolving both rollouts of one thread to the same UUID would collapse them
// onto a single discovery key, and the discovery dedupe would then drop one of
// the two files by modification time — losing a whole file's entries
// deterministically instead of only when the walk order happened to be unkind.
func CodexContinuationThreadUUIDFromFilename(name string) string {
	if !isCodexSessionFilename(name) {
		return ""
	}
	match := codexContinuationRe.FindStringSubmatch(
		strings.TrimSuffix(name, ".jsonl"),
	)
	if len(match) < 3 {
		return ""
	}
	return match[1]
}

// codexContinuationMeta reads a candidate continuation's own session_meta and
// reports the thread it continues plus the entry ordinal its history starts
// after. ok is false for any rollout that does not carry the paginated-history
// marker, so a filename that merely looks like a continuation can never attach
// one thread's entries to another.
func codexContinuationMeta(
	path string,
) (threadID string, endOrdinalExclusive int64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	for scanned := 0; scanned < codexContinuationMetaScanLimit && scanner.Scan(); scanned++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !gjson.Valid(line) {
			continue
		}
		if gjson.Get(line, "type").Str != codexTypeSessionMeta {
			continue
		}
		payload := gjson.Get(line, "payload")
		if payload.Get("history_mode").Str != codexHistoryModePaginated {
			return "", 0, false
		}
		base := payload.Get("history_base")
		if !base.Exists() {
			return "", 0, false
		}
		thread := strings.TrimSpace(base.Get("thread_id").Str)
		if thread == "" {
			return "", 0, false
		}
		return thread, base.Get("end_ordinal_exclusive").Int(), true
	}
	return "", 0, false
}

// codexContinuationPathsFor returns the confirmed paginated continuations of the
// thread whose own rollout is headPath, ordered by the entry ordinal each one
// continues from. The returned paths are companions of headPath: their entries
// belong to headPath's session.
//
// Only siblings in headPath's own directory are joined. The chain has to be the
// same set seen from either end — the head must find exactly the continuations
// that name it, and a continuation must find exactly the head that owns it —
// because the sync engine keys freshness on a session's stored file_path, and a
// source whose parse wrote a different path's session would re-parse on every
// pass. One directory listing is the bounded scan both ends can agree on, and
// it is the layout Codex writes: a continuation lands in the day directory of
// its own start, beside the thread it continues. A continuation filed anywhere
// else is left alone: it is not joined, not skipped and not dropped, so it stays
// visible as an unexplained file rather than being attached to a thread from
// another directory on the strength of a filename.
func codexContinuationPathsFor(headPath string) []string {
	thread := CodexSessionUUIDFromFilename(filepath.Base(headPath))
	if thread == "" {
		return nil
	}
	dir := filepath.Dir(headPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type member struct {
		path    string
		ordinal int64
	}
	var members []member
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if CodexContinuationThreadUUIDFromFilename(name) != thread {
			continue
		}
		candidate := filepath.Join(dir, name)
		metaThread, ordinal, ok := codexContinuationMeta(candidate)
		if !ok || metaThread != thread {
			continue
		}
		members = append(members, member{path: candidate, ordinal: ordinal})
	}
	slices.SortStableFunc(members, func(a, b member) int {
		if a.ordinal != b.ordinal {
			if a.ordinal < b.ordinal {
				return -1
			}
			return 1
		}
		return strings.Compare(a.path, b.path)
	})
	paths := make([]string, 0, len(members))
	for _, m := range members {
		paths = append(paths, m.path)
	}
	return paths
}

// codexContinuationHeadFor returns the rollout whose session a paginated
// continuation belongs to, or "" when path is not a continuation or its thread's
// own rollout is not its sibling. An unmatched continuation keeps its own
// source, which is what keeps it visible instead of silently unread.
func codexContinuationHeadFor(path string) string {
	thread := CodexContinuationThreadUUIDFromFilename(filepath.Base(path))
	if thread == "" {
		return ""
	}
	metaThread, _, ok := codexContinuationMeta(path)
	if !ok || metaThread != thread {
		return ""
	}
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if CodexSessionUUIDFromFilename(name) != thread {
			continue
		}
		return filepath.Join(dir, name)
	}
	return ""
}

// codexChainSourceHash folds a thread's continuations into its rollout's source
// hash, mirroring claudeLayoutCompositeFingerprint. Without this the head's
// content hash would be unchanged while a continuation grew, and both freshness
// gates (shouldSkipCodexFingerprint and providerSourceUnchangedInDB, which
// require a matching stored hash for this provider) would skip the thread. A
// thread with no continuation keeps its plain transcript hash, so no stored
// fingerprint in an existing archive is invalidated by this change.
func codexChainSourceHash(
	ctx context.Context, headPath string, continuations []string,
) (string, error) {
	headHash, err := hashJSONLSourceFileContext(ctx, headPath)
	if err != nil {
		return "", err
	}
	if len(continuations) == 0 {
		return headHash, nil
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "transcript\x00%s\x00", headHash)
	for _, continuation := range continuations {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		continuationHash, hashErr := hashJSONLSourceFileContext(ctx, continuation)
		if hashErr != nil {
			return "", hashErr
		}
		_, _ = fmt.Fprintf(
			h, "continuation\x00%s\x00%s\x00",
			filepath.Base(continuation), continuationHash,
		)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// joinCodexContinuations appends each continuation's entries to the session
// parsed from the thread's own rollout. The continuation does not repeat earlier
// entries (history_base.end_ordinal_exclusive is where its history starts), so
// the join is an append: message ordinals continue after the head's last
// ordinal, which is what makes the session's next_ordinal resume across the
// chain instead of restarting at the continuation.
//
// A continuation whose own parse resolves to a different session id is not
// joined: session identity comes from the file's session_meta, and only the
// thread's own continuation carries the thread's id.
func (p *codexProvider) joinCodexContinuations(
	ctx context.Context,
	headPath, machine string,
	sess *ParsedSession,
	msgs []ParsedMessage,
	continuations []string,
) ([]ParsedMessage, bool, error) {
	joined := false
	for _, continuation := range continuations {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		contSess, contMsgs, _, _, _, _, _, err := p.parseSessionWithCursor(
			ctx, continuation, machine, false,
		)
		if err != nil {
			return nil, false, err
		}
		if contSess == nil || contSess.ID != sess.ID {
			continue
		}
		base := 0
		if len(msgs) > 0 {
			base = msgs[len(msgs)-1].Ordinal + 1
		}
		for i := range contMsgs {
			contMsgs[i].Ordinal += base
		}
		msgs = append(msgs, contMsgs...)
		joined = true

		if contSess.EndedAt.After(sess.EndedAt) {
			sess.EndedAt = contSess.EndedAt
		}
		sess.MalformedLines += contSess.MalformedLines
		sess.IsTruncated = contSess.IsTruncated
		if contSess.TerminationStatus != "" {
			sess.TerminationStatus = contSess.TerminationStatus
		}
		if sess.FirstMessage == "" {
			sess.FirstMessage = contSess.FirstMessage
		}
	}
	if !joined {
		return msgs, false, nil
	}
	sess.MessageCount = len(msgs)
	sess.UserMessageCount = codexProviderUserMessageCount(msgs)
	sess.TotalOutputTokens = 0
	sess.PeakContextTokens = 0
	sess.HasTotalOutputTokens = false
	sess.HasPeakContextTokens = false
	if err := accumulateMessageTokenUsageContext(ctx, sess, msgs); err != nil {
		return nil, false, err
	}
	chainHash, err := codexChainSourceHash(ctx, headPath, continuations)
	if err != nil {
		return nil, false, err
	}
	sess.File.Hash = chainHash
	return msgs, true, nil
}
