package parser

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Aider appends every run to a single history file
// (.aider.chat.history.md). It is a multi-session container provider:
// discovery surfaces the history file as one source and Parse fans it out into
// one session per run, addressed by "<history>#<runIdx>" virtual paths. All
// behavior is wired into the shared multi-session-container base via options.
//
// The PathRewriter (identity) maps an on-disk history path to its canonical
// stored form during remote sync, so per-run session IDs stay stable across
// syncs that extract the file to a different temp directory. It is threaded
// into the option closures by the build func and is nil for local sync.
func newAiderProviderFactory(def AgentDef) ProviderFactory {
	return NewMultiSessionProviderFactory(
		def,
		aiderProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			identity := cfg.PathRewriter
			return NewMultiSessionContainerSourceSet(
				AgentAider,
				cfg.Roots,
				WithContainerDiscovery(aiderDiscoverContainers),
				WithStreamingSourceDiscovery(
					func(ctx context.Context, root string, yield func(multiSessionMatch) error) error {
						return streamAiderContainers(ctx, root, func(container string) error {
							idPath := aiderIdentityForPath(container, identity)
							if err := streamAiderRunIndexes(ctx, container, idPath, func(idx int, rawID string) error {
								return yield(multiSessionMatch{
									Path:      AiderVirtualPath(container, idx),
									Container: container, MemberID: strconv.Itoa(idx),
									ReconciliationIdentity: rawID,
								})
							}); err != nil {
								return err
							}
							return nil
						})
					},
				),
				WithWatchRoots(aiderWatchRoots),
				WithChangedPathClassifier(aiderClassifyPath),
				WithMemberLookup(
					func(root, rawID string) (multiSessionMatch, bool) {
						return aiderFindMember(root, rawID, identity)
					},
				),
				WithReconciliationIdentity(
					func(ctx context.Context, match multiSessionMatch) (string, error) {
						return aiderReconciliationIdentity(ctx, match, identity)
					},
					func(fullSessionID string) string {
						return ProviderRawSessionIDFromFull(def, fullSessionID)
					},
				),
				// A canonical remote-sync path (rewritten identity) must map
				// back onto a local history file before it can classify.
				WithStoredPathFallback(
					func(root, path string) (multiSessionMatch, bool) {
						return aiderStoredPathFallback(root, path, identity)
					},
				),
				// Aider run paths are positional ("<history>#<idx>"), so a
				// RequireFreshSource resync must confirm the index still hashes to
				// the requested run before trusting the stored path; otherwise an
				// inserted or removed earlier run silently resyncs the wrong run.
				WithFreshStoredMember(
					func(src multiSessionSource, rawID string) bool {
						return aiderStoredMemberMatchesRawID(src, rawID, identity)
					},
				),
				WithContextFingerprint(aiderFingerprintSourceContext),
				WithContainerParse(
					func(src multiSessionSource, req ParseRequest) ([]ParseResult, error) {
						return aiderParseContainer(src, req.Machine, identity)
					},
				),
				WithContextMemberParse(
					func(ctx context.Context, src multiSessionSource, req ParseRequest) (*ParseResult, error) {
						return aiderParseMemberContext(ctx, src, req.Machine, identity)
					},
				),
				// Every run shares the history file's content hash, so a write
				// re-parses and re-stamps every run.
				WithContainerHashStamping(),
			)
		},
	)
}

// streamAiderContainers preserves Aider's rootless walk limits while reading
// every directory in fixed-size batches. The legacy collecting discovery path
// remains sorted for callers that request Discover; forced reconciliation uses
// this direct traversal and never materializes the archive's container list.
func streamAiderContainers(
	ctx context.Context, root string, yield func(string) error,
) error {
	if root == "" {
		return nil
	}
	root = filepath.Clean(root)
	skipProtected := aiderShouldSkipProtectedHomeDirs(root, aiderHomeDir(), runtime.GOOS)
	started := time.Now()
	limits := discoveryTraversalLimitsFor(ctx)
	maxDirs := aiderMaxDirs
	if limits.maxDirs > 0 {
		maxDirs = limits.maxDirs
	}
	maxFiles := aiderMaxFiles
	if limits.maxFiles > 0 {
		maxFiles = limits.maxFiles
	}
	expired := limits.expired
	if expired == nil {
		expired = func() bool { return time.Since(started) >= aiderWalkBudget }
	}
	dirs, files := 0, 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if expired() {
			return DiscoveryIncompleteError{Provider: AgentAider, Reason: "time budget exceeded"}
		}
		dirs++
		if dirs > maxDirs {
			return DiscoveryIncompleteError{Provider: AgentAider, Reason: "directory limit exceeded"}
		}
		return streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
			if expired() {
				return DiscoveryIncompleteError{Provider: AgentAider, Reason: "time budget exceeded"}
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if depth == 0 && skipProtected {
					if _, skip := aiderProtectedHomeDirs[entry.Name()]; skip {
						return nil
					}
				}
				if _, skip := aiderSkipDirs[entry.Name()]; skip {
					return nil
				}
				if depth+1 > aiderMaxWalkDepth {
					return nil
				}
				return walk(path, depth+1)
			}
			if entry.Name() != aiderHistoryFile {
				return nil
			}
			if err := yield(path); err != nil {
				return err
			}
			files++
			if files >= maxFiles {
				return DiscoveryIncompleteError{Provider: AgentAider, Reason: "file limit exceeded"}
			}
			return nil
		})
	}
	return walk(root, 0)
}

func streamAiderRunIndexes(
	ctx context.Context, path, idPath string, yield func(int, string) error,
) error {
	if reconciliationCacheAvailable(ctx) {
		return streamAndCacheAiderRuns(ctx, path, idPath, yield)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	idx := 0
	headerOrdinals := make(map[string]int)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if after, ok := strings.CutPrefix(line, aiderHeaderPrefix); ok {
			// splitAiderRuns trims the header before hashing; every
			// scanner path must match or the same run gets a second ID.
			rawHeader := strings.TrimSpace(after)
			ordinal := headerOrdinals[rawHeader]
			headerOrdinals[rawHeader]++
			observeStreamingDiscoveryBuffer(ctx, 1)
			rawID := aiderRawID(aiderIdentityPath(path, idPath), rawHeader, ordinal)
			if err := yield(idx, rawID); err != nil {
				return err
			}
			idx++
		}
	}
	return scanner.Err()
}

type cachedAiderRun struct {
	RawHeader string `json:"raw_header"`
	Body      string `json:"body"`
	Ordinal   int    `json:"ordinal"`
}

func aiderCachedRunKey(path string, idx int) string {
	return "aider:run:" + AiderVirtualPath(filepath.Clean(path), idx)
}

func aiderCachedRunCountKey(path string) string {
	return "aider:run-count:" + filepath.Clean(path)
}

func streamAndCacheAiderRuns(
	ctx context.Context, path, idPath string, yield func(int, string) error,
) error {
	replayed, err := replayCachedAiderRuns(ctx, path, idPath, yield)
	if err != nil {
		return err
	}
	if replayed {
		return nil
	}
	scanID, err := reconciliationCacheAddInt(ctx, "aider:scan")
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	observeSharedContainerScan(ctx)
	hasher := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(file, hasher))
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	idx := -1
	var rawHeader string
	var body strings.Builder
	flush := func() error {
		if idx < 0 {
			return nil
		}
		ordinal, err := reconciliationCacheAddInt(
			ctx, "aider:header:"+strconv.Itoa(scanID)+":"+
				filepath.Clean(path)+"\x00"+rawHeader,
		)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(cachedAiderRun{
			RawHeader: rawHeader, Body: body.String(), Ordinal: ordinal,
		})
		if err != nil {
			return err
		}
		observeStreamingRetainedBytes(ctx, int64(len(encoded)))
		err = reconciliationCachePut(ctx, aiderCachedRunKey(path, idx), string(encoded))
		observeStreamingRetainedBytes(ctx, -int64(len(encoded)))
		if err != nil {
			return err
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		rawID := aiderRawID(aiderIdentityPath(path, idPath), rawHeader, ordinal)
		return yield(idx, rawID)
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.HasPrefix(line, aiderHeaderPrefix) {
			if err := flush(); err != nil {
				return err
			}
			idx++
			rawHeader = strings.TrimSpace(
				strings.TrimPrefix(line, aiderHeaderPrefix),
			)
			body.Reset()
			continue
		}
		if idx >= 0 {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(SourceFingerprint{
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(),
		Hash: fmt.Sprintf("%x", hasher.Sum(nil)),
	})
	if err != nil {
		return err
	}
	if err := reconciliationCachePut(
		ctx, aiderCachedFingerprintKey(path), string(encoded),
	); err != nil {
		return err
	}
	return reconciliationCachePut(
		ctx, aiderCachedRunCountKey(path), strconv.Itoa(idx+1),
	)
}

func replayCachedAiderRuns(
	ctx context.Context, path, idPath string, yield func(int, string) error,
) (bool, error) {
	encodedCount, found, err := reconciliationCacheGet(
		ctx, aiderCachedRunCountKey(path),
	)
	if err != nil || !found {
		return false, err
	}
	count, err := strconv.Atoi(encodedCount)
	if err != nil || count < 0 {
		return false, fmt.Errorf("decode cached aider run count %q", encodedCount)
	}
	for idx := range count {
		encoded, runFound, err := reconciliationCacheGet(
			ctx, aiderCachedRunKey(path, idx),
		)
		if err != nil {
			return false, err
		}
		if !runFound {
			return false, fmt.Errorf(
				"cached aider run %d missing after completed scan", idx,
			)
		}
		observeStreamingRetainedBytes(ctx, int64(len(encoded)))
		var cached cachedAiderRun
		if err := json.Unmarshal([]byte(encoded), &cached); err != nil {
			observeStreamingRetainedBytes(ctx, -int64(len(encoded)))
			return false, fmt.Errorf("decode cached aider run: %w", err)
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		rawID := aiderRawID(
			aiderIdentityPath(path, idPath), cached.RawHeader, cached.Ordinal,
		)
		err = yield(idx, rawID)
		observeStreamingRetainedBytes(ctx, -int64(len(encoded)))
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func aiderReconciliationIdentity(
	ctx context.Context, match multiSessionMatch, identity func(string) string,
) (string, error) {
	idx, err := strconv.Atoi(match.MemberID)
	if err != nil {
		return "", fmt.Errorf("invalid aider run index %q: %w", match.MemberID, err)
	}
	encoded, found, err := reconciliationCacheGet(
		ctx, aiderCachedRunKey(match.Container, idx),
	)
	if err != nil {
		return "", err
	}
	if found {
		var cached cachedAiderRun
		if err := json.Unmarshal([]byte(encoded), &cached); err != nil {
			return "", fmt.Errorf("decode cached aider run identity: %w", err)
		}
		return aiderRawID(
			aiderIdentityPath(match.Container, aiderIdentityForPath(match.Container, identity)),
			cached.RawHeader, cached.Ordinal,
		), nil
	}
	rawID, ok := aiderRawIDAtWithID(
		match.Container, aiderIdentityForPath(match.Container, identity), idx,
	)
	if !ok {
		return "", nil
	}
	return rawID, nil
}

func aiderDiscoverContainers(root string) []string {
	sessions := discoverAiderSessions(root)
	out := make([]string, 0, len(sessions))
	for _, df := range sessions {
		out = append(out, df.Path)
	}
	return out
}

func aiderWatchRoots(roots []string) []WatchRoot {
	// Aider is rootless: history files live anywhere under a root. The legacy
	// config marks it ShallowWatch, so watch each root non-recursively for the
	// history filename and rely on periodic full-sync discovery for nested
	// files, matching the prior behavior.
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{aiderHistoryFile},
			DebounceKey:  string(AgentAider) + ":history:" + root,
		})
	}
	return out
}

// aiderClassifyPath maps a stored or changed path to its history-file container
// and run. A virtual "<history>#<idx>" path resolves to that single run; a bare
// history file resolves to the whole container (MemberID == ""). aider performs
// no existence check, so allowMissing is unused.
func aiderClassifyPath(root, path string, _ bool) (multiSessionMatch, bool) {
	root = filepath.Clean(root)
	if historyPath, idx, ok := ParseAiderVirtualPath(path); ok {
		historyPath = filepath.Clean(historyPath)
		if _, ok := relUnder(root, historyPath); !ok {
			return multiSessionMatch{}, false
		}
		return multiSessionMatch{
			Path:      path,
			Container: historyPath,
			MemberID:  strconv.Itoa(idx),
		}, true
	}
	path = filepath.Clean(path)
	if filepath.Base(path) != aiderHistoryFile {
		return multiSessionMatch{}, false
	}
	if _, ok := relUnder(root, path); !ok {
		return multiSessionMatch{}, false
	}
	return multiSessionMatch{Path: path, Container: path}, true
}

func aiderFindMember(
	root, rawID string, identity func(string) string,
) (multiSessionMatch, bool) {
	if rawID == "" {
		return multiSessionMatch{}, false
	}
	for _, df := range discoverAiderSessions(root) {
		virtualPath, ok := aiderVirtualPathForRawIDWithID(
			df.Path, aiderIdentityForPath(df.Path, identity), rawID,
		)
		if !ok {
			continue
		}
		if match, ok := aiderClassifyPath(root, virtualPath, false); ok {
			return match, true
		}
	}
	return multiSessionMatch{}, false
}

func aiderStoredPathFallback(
	root, path string, identity func(string) string,
) (multiSessionMatch, bool) {
	if path == "" {
		return multiSessionMatch{}, false
	}
	for _, df := range discoverAiderSessions(root) {
		localPath, ok := localAiderPathForCanonicalHint(df.Path, path, identity)
		if !ok {
			continue
		}
		if match, ok := aiderClassifyPath(root, localPath, false); ok {
			return match, true
		}
	}
	return multiSessionMatch{}, false
}

// localAiderPathForCanonicalHint maps a canonical hint (the identity path, or a
// virtual run path built on it) back to the corresponding local history path or
// local virtual run path. It mirrors the legacy provider helper of the same
// name.
func localAiderPathForCanonicalHint(
	historyPath, hint string, identity func(string) string,
) (string, bool) {
	idPath := aiderIdentityForPath(historyPath, identity)
	if idPath == "" {
		idPath = aiderAbsPath(historyPath)
	}
	if hint == idPath {
		return historyPath, true
	}
	hintHistoryPath, idx, ok := ParseAiderVirtualPath(hint)
	if !ok || hintHistoryPath != idPath {
		return "", false
	}
	return AiderVirtualPath(historyPath, idx), true
}

// aiderIdentityForPath returns the canonical identity path used to seed per-run
// session IDs: the rewritten path during remote sync, or "" locally (which
// makes the parser fall back to the on-disk absolute path). It mirrors the
// legacy Engine.aiderIdentityPath / aiderProvider.identityPath.
func aiderIdentityForPath(historyPath string, identity func(string) string) string {
	if identity == nil {
		return ""
	}
	return identity(historyPath)
}

func aiderVirtualPathForRawIDWithID(
	historyPath string,
	idPath string,
	rawID string,
) (string, bool) {
	data, err := os.ReadFile(historyPath)
	if err != nil {
		return "", false
	}
	runs := splitAiderRuns(string(data))
	ordinals := aiderEqualHeaderOrdinals(runs)
	identity := aiderIdentityPath(historyPath, idPath)
	for idx, run := range runs {
		if aiderRawID(identity, run.rawHeader, ordinals[idx]) == rawID {
			return AiderVirtualPath(historyPath, idx), true
		}
	}
	return "", false
}

// aiderStoredMemberMatchesRawID reports whether the run at the stored positional
// index still hashes to the requested raw session ID. Aider run paths are
// positional ("<history>#<idx>"), so an inserted or removed earlier run shifts
// the index onto a different run; without this a RequireFreshSource lookup would
// return the stale path and resync the wrong session. Any mismatch (an
// unreadable file, an out-of-range index, or a remote identity that does not
// reproduce the stored hash) returns false so the base re-resolves by raw ID.
func aiderStoredMemberMatchesRawID(
	src multiSessionSource, rawID string, identity func(string) string,
) bool {
	if rawID == "" {
		return false
	}
	idx, err := strconv.Atoi(src.MemberID)
	if err != nil {
		return false
	}
	idPath := aiderIdentityForPath(src.Container, identity)
	got, ok := aiderRawIDAtWithID(src.Container, idPath, idx)
	return ok && got == rawID
}

// aiderRawIDAtWithID recomputes the raw session ID of the run at positional
// index idx, honoring the canonical identity path used during remote sync. It
// mirrors AiderRawIDAt but threads idPath so the hash matches the stored ID for
// both local (idPath == "") and rewritten remote history files.
func aiderRawIDAtWithID(historyPath, idPath string, idx int) (string, bool) {
	data, err := os.ReadFile(historyPath)
	if err != nil {
		return "", false
	}
	runs := splitAiderRuns(string(data))
	if idx < 0 || idx >= len(runs) {
		return "", false
	}
	ordinals := aiderEqualHeaderOrdinals(runs)
	return aiderRawID(
		aiderIdentityPath(historyPath, idPath), runs[idx].rawHeader, ordinals[idx],
	), true
}

func aiderFingerprintSource(src multiSessionSource) (SourceFingerprint, error) {
	info, err := os.Stat(src.Container)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf(
			"stat %s: source is a directory", src.Container,
		)
	}
	hash, err := hashJSONLSourceFile(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Hash:    hash,
	}, nil
}

func aiderCachedFingerprintKey(path string) string {
	return "aider:fingerprint:" + filepath.Clean(path)
}

func aiderFingerprintSourceContext(
	ctx context.Context, src multiSessionSource,
) (SourceFingerprint, error) {
	encoded, found, err := reconciliationCacheGet(
		ctx, aiderCachedFingerprintKey(src.Container),
	)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if !found {
		return aiderFingerprintSource(src)
	}
	var fingerprint SourceFingerprint
	if err := json.Unmarshal([]byte(encoded), &fingerprint); err != nil {
		return SourceFingerprint{}, fmt.Errorf("decode cached aider fingerprint: %w", err)
	}
	return fingerprint, nil
}

func aiderParseMember(
	src multiSessionSource, machine string, identity func(string) string,
) (*ParseResult, error) {
	idx, err := strconv.Atoi(src.MemberID)
	if err != nil {
		return nil, fmt.Errorf("invalid aider run index %q: %w", src.MemberID, err)
	}
	idPath := aiderIdentityForPath(src.Container, identity)
	sess, msgs, err := parseAiderRunWithID(src.Container, idPath, idx, machine)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}
	return &ParseResult{Session: *sess, Messages: msgs}, nil
}

func aiderParseMemberContext(
	ctx context.Context, src multiSessionSource, machine string,
	identity func(string) string,
) (*ParseResult, error) {
	idx, err := strconv.Atoi(src.MemberID)
	if err != nil {
		return nil, fmt.Errorf("invalid aider run index %q: %w", src.MemberID, err)
	}
	encoded, found, err := reconciliationCacheGet(ctx, aiderCachedRunKey(src.Container, idx))
	if err != nil {
		return nil, err
	}
	if !found {
		return aiderParseMember(src, machine, identity)
	}
	observeStreamingRetainedBytes(ctx, int64(len(encoded)))
	defer observeStreamingRetainedBytes(ctx, -int64(len(encoded)))
	var cached cachedAiderRun
	if err := json.Unmarshal([]byte(encoded), &cached); err != nil {
		return nil, fmt.Errorf("decode cached aider run: %w", err)
	}
	info, err := os.Stat(src.Container)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	run := aiderRun{rawHeader: cached.RawHeader, body: cached.Body}
	run.started, run.hasTime = parseAiderTimestamp(cached.RawHeader)
	sess, msgs := buildAiderRunSession(
		src.Container, aiderIdentityForPath(src.Container, identity), machine,
		info, run, idx, cached.Ordinal,
	)
	if sess == nil {
		return nil, nil
	}
	return &ParseResult{Session: *sess, Messages: msgs}, nil
}

func aiderParseContainer(
	src multiSessionSource, machine string, identity func(string) string,
) ([]ParseResult, error) {
	idPath := aiderIdentityForPath(src.Container, identity)
	return parseAiderRunsWithID(src.Container, idPath, machine)
}

func aiderProviderCapabilities() Capabilities {
	return Capabilities{
		Source: multiSessionContainerSourceCapabilities(
			CapabilityNotApplicable,
			CapabilityUnsupported,
		),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Cwd:                  CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
		},
	}
}
