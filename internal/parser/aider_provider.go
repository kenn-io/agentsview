package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
				WithWatchRoots(aiderWatchRoots),
				WithChangedPathClassifier(aiderClassifyPath),
				WithMemberLookup(
					func(root, rawID string) (multiSessionMatch, bool) {
						return aiderFindMember(root, rawID, identity)
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
				WithFingerprint(aiderFingerprintSource),
				WithContainerParse(
					func(src multiSessionSource, req ParseRequest) ([]ParseResult, error) {
						return aiderParseContainer(src, req.Machine, identity)
					},
				),
				WithMemberParse(
					func(src multiSessionSource, req ParseRequest) (*ParseResult, error) {
						return aiderParseMember(src, req.Machine, identity)
					},
				),
				// Every run shares the history file's content hash, so a write
				// re-parses and re-stamps every run.
				WithContainerHashStamping(),
			)
		},
	)
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

func aiderParseContainer(
	src multiSessionSource, machine string, identity func(string) string,
) ([]ParseResult, error) {
	idPath := aiderIdentityForPath(src.Container, identity)
	return parseAiderRunsWithID(src.Container, idPath, machine)
}

func aiderProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilityNotApplicable,
			IncrementalAppend:    CapabilityNotApplicable,
			MultiSessionSource:   CapabilitySupported,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilitySupported,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Cwd:                  CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
		},
	}
}
