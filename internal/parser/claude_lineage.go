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
// whose root chain entry is bg-marked is ever considered a fork, and
// only non-bg siblings qualify as its ancestor. Anything ambiguous
// (bg-to-bg pairs, unmarked manual --fork-session copies, missing or
// divergent siblings) fails open: no trim and no relationship is
// emitted, leaving the status-quo duplicate rather than risking a
// wrongly-oriented trim.

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
}

// claudeLineagePlan describes an established fork lineage: the leading
// dropCount uuid-bearing lines of the fork transcript are a replay of
// the parent transcript and are dropped from the parse.
type claudeLineagePlan struct {
	parentSessionID string
	dropCount       int
	totalUUIDLines  int
	// dropUUIDs holds the replayed uuids so retained entries whose
	// parentUuid points into the dropped region can be re-rooted.
	dropUUIDs map[string]struct{}
}

// pureReplay reports whether the fork holds no chain entries beyond the
// replayed prefix, i.e. it is currently an exact copy of its parent.
func (p *claudeLineagePlan) pureReplay() bool {
	return p != nil && p.dropCount == p.totalUUIDLines
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

// claudeScanUUIDs streams every uuid-bearing line of a transcript in
// order. Errors and malformed lines are skipped; lineage resolution
// fails open on incomplete data.
func claudeScanUUIDs(path string, visit func(uuid string)) bool {
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
		if uuid := gjson.GetBytes(lineBytes, "uuid").Str; uuid != "" {
			visit(uuid)
		}
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
		if !sibling.ok || sibling.rootIsBG ||
			sibling.rootUUID != self.rootUUID {
			continue
		}
		candidates = append(candidates, candidate{
			path: filepath.Join(dir, name),
			stem: strings.TrimSuffix(name, ".jsonl"),
		})
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].stem < candidates[j].stem
	})

	var forkSeq []string
	if !claudeScanUUIDs(path, func(uuid string) {
		forkSeq = append(forkSeq, uuid)
	}) || len(forkSeq) == 0 {
		return nil
	}

	// Pick the candidate whose uuid set covers the longest contiguous
	// leading run of the fork: with chained ancestors sharing one
	// root, the deepest ancestor is the one the fork replayed.
	bestRun := 0
	bestStem := ""
	var bestSet map[string]struct{}
	for _, c := range candidates {
		set := make(map[string]struct{})
		if !claudeScanUUIDs(c.path, func(uuid string) {
			set[uuid] = struct{}{}
		}) {
			continue
		}
		run := 0
		for _, uuid := range forkSeq {
			if _, ok := set[uuid]; !ok {
				break
			}
			run++
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
	for _, uuid := range forkSeq[:bestRun] {
		if _, ok := bestSet[uuid]; ok {
			dropUUIDs[uuid] = struct{}{}
		}
	}
	return &claudeLineagePlan{
		parentSessionID: bestStem,
		dropCount:       bestRun,
		totalUUIDLines:  len(forkSeq),
		dropUUIDs:       dropUUIDs,
	}
}
