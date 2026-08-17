// ABOUTME: Resolves Claude background-fork lineage against sibling transcripts.
// ABOUTME: Plans the replayed-prefix trim for background-forked session files (#1370).
package parser

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// Claude Code's background handoff (left-arrow picker, Ctrl+B,
// /background) spawns `claude --resume <transcript> --fork-session` with
// CLAUDE_CODE_SESSION_KIND=bg. The forked process re-persists the entire
// prior message chain into a new transcript in the same project
// directory: replayed entries keep their original uuid, timestamp,
// message id, and usage, while sessionId is rewritten and
// sessionKind:"bg" is stamped on every chain entry. The original
// interactive transcript carries no sessionKind and no pointer to the
// fork, so lineage can only be established from content overlap.
//
// Direction is anchored on the asymmetric bg stamp: only a transcript
// whose root chain entry is bg-marked is ever considered a fork. The
// stamp is process-derived — the writer re-stamps sessionKind from the
// current process on every persisted line, so a non-bg fork of a bg
// transcript carries no marker and never trims. Both non-bg and bg
// siblings qualify as ancestors, so chained backgrounding trims each
// link against its nearest ancestor, but a bg candidate that strictly
// contains the fork never wins: it is the fork's descendant or an
// extension, and trimming against it could erase the ancestor. Equal
// uuid content (a fork that is a pure copy of a bg sibling) elects
// direction deterministically by stem. Anything else ambiguous
// (unmarked manual --fork-session copies, missing or divergent
// siblings, several branches at the replay boundary) fails open: no
// trim and no relationship is emitted, leaving the status-quo
// duplicate rather than risking a wrongly-oriented trim.

const (
	// claudeLineageSniffMaxLines bounds how many leading lines are
	// scanned for a transcript's first chain entry. Real transcripts
	// carry at most a few non-chain records (summaries, ai-title,
	// mode, queue-operations) before the first uuid-bearing entry.
	claudeLineageSniffMaxLines = 256
	// claudeSniffCacheMaxEntries bounds the shared sniff memo. The
	// cache is rebuilt lazily after a reset, so overflow only costs
	// re-reads of transcript heads.
	claudeSniffCacheMaxEntries = 8192
)

// claudeHeadSniff summarizes the first uuid-bearing chain entry of a
// transcript head.
type claudeHeadSniff struct {
	rootUUID string
	rootIsBG bool
	ok       bool
}

type claudeSniffCacheEntry struct {
	size    int64
	mtimeNS int64
	sniff   claudeHeadSniff
}

// The sniff memo is package-level because providers are re-instantiated
// for every classification and parse pass; per-instance state would
// never get cache hits.
var (
	claudeSniffMu    sync.Mutex
	claudeSniffCache = map[string]claudeSniffCacheEntry{}
)

// claudeParseOptions gates opt-in parse behaviors that only the local
// Claude provider enables. Uploads, Cowork, and Qoder reuse the Claude
// parse body and must keep every option off.
type claudeParseOptions struct {
	// siblingLineage enables background-fork lineage resolution
	// against sibling transcripts in the same directory.
	siblingLineage bool
	// uploadIdentity enables adoption of explicit rooted transcript IDs.
	uploadIdentity bool
}

// claudeLineagePlan describes an established fork lineage: the leading
// dropCount uuid-bearing lines of the fork transcript are a replay of
// the parent transcript and are dropped from the parse.
type claudeLineagePlan struct {
	parentSessionID string
	dropCount       int
	// dropUUIDs holds the replayed uuids so retained entries whose
	// parentUuid points into the dropped region can be re-rooted.
	dropUUIDs map[string]struct{}
}

func claudeSniffHead(path string) claudeHeadSniff {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return claudeHeadSniff{}
	}
	claudeSniffMu.Lock()
	if e, ok := claudeSniffCache[path]; ok &&
		e.size == info.Size() && e.mtimeNS == info.ModTime().UnixNano() {
		claudeSniffMu.Unlock()
		return e.sniff
	}
	claudeSniffMu.Unlock()

	sniff := claudeSniffHeadUncached(path)

	claudeSniffMu.Lock()
	if len(claudeSniffCache) >= claudeSniffCacheMaxEntries {
		claudeSniffCache = map[string]claudeSniffCacheEntry{}
	}
	claudeSniffCache[path] = claudeSniffCacheEntry{
		size:    info.Size(),
		mtimeNS: info.ModTime().UnixNano(),
		sniff:   sniff,
	}
	claudeSniffMu.Unlock()
	return sniff
}

func claudeSniffHeadUncached(path string) claudeHeadSniff {
	f, err := os.Open(path)
	if err != nil {
		return claudeHeadSniff{}
	}
	defer f.Close()
	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	for range claudeLineageSniffMaxLines {
		lineBytes, ok := lr.nextBytes()
		if !ok {
			return claudeHeadSniff{}
		}
		if !gjson.ValidBytes(lineBytes) {
			continue
		}
		uuid := gjson.GetBytes(lineBytes, "uuid").Str
		if uuid == "" {
			continue
		}
		// The first chain entry of a well-formed transcript (or of a
		// replayed copy, which starts at the conversation root) has no
		// parentUuid. Any other head shape is unexpected: fail open.
		if gjson.GetBytes(lineBytes, "parentUuid").Str != "" {
			return claudeHeadSniff{}
		}
		return claudeHeadSniff{
			rootUUID: uuid,
			rootIsBG: gjson.GetBytes(lineBytes, "sessionKind").Str == "bg",
			ok:       true,
		}
	}
	return claudeHeadSniff{}
}

// claudeForkLine is one uuid-bearing line of a fork transcript.
type claudeForkLine struct {
	uuid       string
	parentUUID string
	// chainEntry marks user/assistant records — the ones the parser
	// collects into DAG entries and whose parent links matter for
	// boundary re-rooting.
	chainEntry bool
}

// claudeScanUUIDs streams every uuid-bearing line of a transcript in
// order. Errors and malformed lines are skipped; lineage resolution
// fails open on incomplete data.
func claudeScanUUIDs(path string, visit func(line claudeForkLine)) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	for {
		lineBytes, ok := lr.nextBytes()
		if !ok {
			break
		}
		if !gjson.ValidBytes(lineBytes) {
			continue
		}
		uuid := gjson.GetBytes(lineBytes, "uuid").Str
		if uuid == "" {
			continue
		}
		entryType := gjson.GetBytes(lineBytes, "type").Str
		visit(claudeForkLine{
			uuid:       uuid,
			parentUUID: gjson.GetBytes(lineBytes, "parentUuid").Str,
			chainEntry: entryType == "user" || entryType == "assistant",
		})
	}
	return lr.Err() == nil
}

// claudeResolveSiblingLineage establishes the background-fork lineage
// for path, or returns nil when no lineage can be positively oriented.
// Sibling discovery is head-sniff only (memoized per size and mtime);
// the qualifying candidates' full uuid sets are read once per full
// parse of a bg-marked fork.
func claudeResolveSiblingLineage(path string) *claudeLineagePlan {
	self := claudeSniffHead(path)
	if !self.ok || !self.rootIsBG {
		return nil
	}
	dir := filepath.Dir(path)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	base := filepath.Base(path)
	type candidate struct {
		path string
		stem string
		bg   bool
	}
	var candidates []candidate
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || name == base ||
			!strings.HasSuffix(name, ".jsonl") ||
			strings.HasPrefix(name, "agent-") {
			continue
		}
		sibling := claudeSniffHead(filepath.Join(dir, name))
		if !sibling.ok || sibling.rootUUID != self.rootUUID {
			continue
		}
		candidates = append(candidates, candidate{
			path: filepath.Join(dir, name),
			stem: strings.TrimSuffix(name, ".jsonl"),
			bg:   sibling.rootIsBG,
		})
	}
	if len(candidates) == 0 {
		return nil
	}
	// Non-bg candidates first so an interactive original wins a run
	// tie against an unrelated bg fork of the same original.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].bg != candidates[j].bg {
			return !candidates[i].bg
		}
		return candidates[i].stem < candidates[j].stem
	})

	var forkSeq []claudeForkLine
	if !claudeScanUUIDs(path, func(line claudeForkLine) {
		forkSeq = append(forkSeq, line)
	}) || len(forkSeq) == 0 {
		return nil
	}

	// Pick the candidate whose uuid set covers the longest contiguous
	// leading run of the fork: with chained ancestors sharing one
	// root, the nearest ancestor is the one the fork replayed. A bg
	// candidate that fully covers the fork is its descendant or twin,
	// where content gives no direction. A strict superset never wins.
	// Equal-set twins elect direction deterministically by stem: the
	// larger stem trims against the smaller, never the reverse, so
	// the smaller twin keeps the content and no cycle can form.
	forkStem := strings.TrimSuffix(base, ".jsonl")
	forkUUIDs := make(map[string]struct{}, len(forkSeq))
	for _, line := range forkSeq {
		forkUUIDs[line.uuid] = struct{}{}
	}
	bestRun := 0
	bestStem := ""
	var bestSet map[string]struct{}
	for _, c := range candidates {
		set := make(map[string]struct{})
		if !claudeScanUUIDs(c.path, func(line claudeForkLine) {
			set[line.uuid] = struct{}{}
		}) {
			continue
		}
		run := 0
		for _, line := range forkSeq {
			if _, ok := set[line.uuid]; !ok {
				break
			}
			run++
		}
		if c.bg && run == len(forkSeq) &&
			(len(set) != len(forkUUIDs) || forkStem < c.stem) {
			continue
		}
		if run > bestRun {
			bestRun = run
			bestStem = c.stem
			bestSet = set
		}
	}
	if bestRun == 0 {
		return nil
	}
	dropUUIDs := make(map[string]struct{}, bestRun)
	for _, line := range forkSeq[:bestRun] {
		if _, ok := bestSet[line.uuid]; ok {
			dropUUIDs[line.uuid] = struct{}{}
		}
	}
	// More than one retained chain entry hanging off the dropped
	// region means the replay boundary carries retry or fork
	// branches. Re-rooting them all would collapse the DAG into a
	// linear merge, so the trim fails open and the intact DAG keeps
	// its branch semantics.
	danglingChildren := 0
	for _, line := range forkSeq[bestRun:] {
		if !line.chainEntry {
			continue
		}
		if _, dropped := dropUUIDs[line.parentUUID]; dropped {
			danglingChildren++
		}
	}
	if danglingChildren > 1 {
		return nil
	}
	return &claudeLineagePlan{
		parentSessionID: bestStem,
		dropCount:       bestRun,
		dropUUIDs:       dropUUIDs,
	}
}
