package parser

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var _ Provider = (*openCodeFormatProvider)(nil)

// A SQLite WAL begins with a 32-byte header. Bytes beyond the header are
// transaction frames, so a larger WAL can contain source changes worth
// syncing. Read-only SQLite connections may create an empty WAL and its SHM
// index simply by opening a quiet WAL-mode database; those sidecars must not
// make the watcher trigger itself.
const sqliteWALHeaderSize = int64(32)

type openCodeFormatProviderFactory struct {
	def   AgentDef
	spec  openCodeProviderSpec
	index *openCodeFormatSourceIndex
}

func newOpenCodeProviderFactory(def AgentDef) ProviderFactory {
	return openCodeFormatProviderFactory{
		def:   cloneAgentDef(def),
		spec:  openCodeProviderSpecForAgent(AgentOpenCode),
		index: newOpenCodeFormatSourceIndex(),
	}
}

func newKiloProviderFactory(def AgentDef) ProviderFactory {
	return openCodeFormatProviderFactory{
		def:   cloneAgentDef(def),
		spec:  openCodeProviderSpecForAgent(AgentKilo),
		index: newOpenCodeFormatSourceIndex(),
	}
}

func newMiMoCodeProviderFactory(def AgentDef) ProviderFactory {
	return openCodeFormatProviderFactory{
		def:   cloneAgentDef(def),
		spec:  openCodeProviderSpecForAgent(AgentMiMoCode),
		index: newOpenCodeFormatSourceIndex(),
	}
}

func (f openCodeFormatProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f openCodeFormatProviderFactory) Capabilities() Capabilities {
	return openCodeFormatProviderCapabilities()
}

func (f openCodeFormatProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &openCodeFormatProvider{
		Def:    cloneAgentDef(f.def),
		Caps:   openCodeFormatProviderCapabilities(),
		Config: cfg,
		sources: newOpenCodeFormatSourceSet(
			cfg.Roots, f.spec, cfg.SQLiteContainerUnchangedSinceTrust, f.index,
		),
	}
}

type openCodeFormatProvider struct {
	ProviderBase
	sources openCodeFormatSourceSet
}

func (p *openCodeFormatProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *openCodeFormatProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *openCodeFormatProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *openCodeFormatProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *openCodeFormatProvider) ChangedPathRelevance(
	ctx context.Context,
	req ChangedPathRequest,
) (ChangedPathRelevance, error) {
	return p.sources.ChangedPathRelevance(ctx, req)
}

func (p *openCodeFormatProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	return p.sources.SourceForReconciliation(ctx, path, project)
}

// ResolveReconciliationScopes widens a request naming the family database, a
// WAL or SHM sidecar, or one virtual member to the container itself. The
// container's membership is atomic: a proof of the bare database path admits
// no member row, and a proof of one member would let a completed pass promote
// container-state trust over siblings it never verified.
func (p *openCodeFormatProvider) ResolveReconciliationScopes(
	_ context.Context, req ReconciliationScopeRequest,
) (ReconciliationScopePlan, error) {
	if err := ValidateReconciliationScopeRoots(
		p.Def.Type, p.Config.Roots, req.Roots,
	); err != nil {
		return ReconciliationScopePlan{}, err
	}
	return containerAwareReconciliationScopePlan(
		p.Config.Roots, req.Roots, p.sources.reconciliationContainer,
	), nil
}

func (p *openCodeFormatProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *openCodeFormatProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *openCodeFormatProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("%s source path unavailable", p.Def.Type)
	}

	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	var (
		sess *ParsedSession
		msgs []ParsedMessage
		err  error
	)
	dbPath, sessionID, sqliteSource := p.sources.spec.parseVirtual(path)
	if sqliteSource {
		sess, msgs, err = p.sources.spec.parseSQLite(dbPath, sessionID, machine)
	} else {
		sess, msgs, err = p.sources.spec.parseFile(path, machine)
	}
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if sqliteSource && req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:  *sess,
				Messages: msgs,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

// openCodeProviderSpec parameterizes the one shared OpenCode-format
// provider implementation for OpenCode and its Kilo and MiMoCode forks.
// All three reuse the same discovery, source-lookup, fingerprinting,
// and parsing code; they differ only in the per-agent SQLite filename,
// the storage/<sessionSubdir> that holds session JSON, and the agent
// label/ID prefix applied via relabel. Kilo and MiMoCode parse through
// the OpenCode storage and SQLite readers, then relabel the result onto
// their own agent and ID prefix.
type openCodeProviderSpec struct {
	agent      AgentType
	format     openCodeFormat
	dbName     string
	listSQLite func(string) ([]OpenCodeSessionMeta, error)
	// listSQLiteWatermark is the bounded changed-path form of listSQLite: it
	// carries only the session-row watermark and no child digest, so a
	// watcher event on the shared container never scans the child tables.
	listSQLiteWatermark func(string) ([]OpenCodeSessionMeta, error)
	streamSQLite        func(context.Context, string, func(OpenCodeSessionMeta) error) error
	// streamSQLiteWatermark is the bounded trusted-container form of
	// streamSQLite, used by streamed reconciliation discovery for containers
	// the engine's container gate will skip wholesale.
	streamSQLiteWatermark func(context.Context, string, func(OpenCodeSessionMeta) error) error
	sourceMtime           func(string) (int64, error)
	relabel               func(*ParsedSession)
}

func openCodeProviderSpecForAgent(agent AgentType) openCodeProviderSpec {
	switch agent {
	case AgentOpenCode:
		return openCodeProviderSpec{
			agent:                 AgentOpenCode,
			format:                openCodeFmt,
			dbName:                openCodeFmt.dbName,
			listSQLite:            ListOpenCodeSessionMeta,
			listSQLiteWatermark:   ListOpenCodeSessionWatermarkMeta,
			streamSQLite:          ForEachOpenCodeSessionMeta,
			streamSQLiteWatermark: ForEachOpenCodeSessionWatermarkMeta,
			sourceMtime:           OpenCodeSourceMtime,
		}
	case AgentKilo:
		return openCodeProviderSpec{
			agent:                 AgentKilo,
			format:                kiloFmt,
			dbName:                kiloFmt.dbName,
			listSQLite:            ListKiloSessionMeta,
			listSQLiteWatermark:   listOpenCodeSessionWatermarkMetaAs(KiloSQLiteVirtualPath),
			streamSQLite:          streamOpenCodeSessionMetaAs(KiloSQLiteVirtualPath),
			streamSQLiteWatermark: streamOpenCodeSessionWatermarkMetaAs(KiloSQLiteVirtualPath),
			sourceMtime:           KiloSourceMtime,
			relabel:               relabelOpenCodeSessionAsKilo,
		}
	case AgentMiMoCode:
		return openCodeProviderSpec{
			agent:                 AgentMiMoCode,
			format:                mimoFmt,
			dbName:                mimoFmt.dbName,
			listSQLite:            ListMiMoCodeSessionMeta,
			listSQLiteWatermark:   listOpenCodeSessionWatermarkMetaAs(MiMoCodeSQLiteVirtualPath),
			streamSQLite:          streamOpenCodeSessionMetaAs(MiMoCodeSQLiteVirtualPath),
			streamSQLiteWatermark: streamOpenCodeSessionWatermarkMetaAs(MiMoCodeSQLiteVirtualPath),
			sourceMtime:           MiMoCodeSourceMtime,
			relabel:               relabelOpenCodeSessionAsMiMoCode,
		}
	case AgentIcodemate:
		return openCodeProviderSpec{
			agent:                 AgentIcodemate,
			format:                icodemateFmt,
			dbName:                icodemateFmt.dbName,
			listSQLite:            ListIcodemateSessionMeta,
			listSQLiteWatermark:   listOpenCodeSessionWatermarkMetaAs(IcodemateSQLiteVirtualPath),
			streamSQLite:          streamOpenCodeSessionMetaAs(IcodemateSQLiteVirtualPath),
			streamSQLiteWatermark: streamOpenCodeSessionWatermarkMetaAs(IcodemateSQLiteVirtualPath),
			sourceMtime:           IcodemateSourceMtime,
			relabel:               relabelOpenCodeSessionAsIcodemate,
		}
	default:
		return openCodeProviderSpec{}
	}
}

func streamOpenCodeSessionMetaAs(
	virtualPath func(string, string) string,
) func(context.Context, string, func(OpenCodeSessionMeta) error) error {
	return func(
		ctx context.Context, dbPath string, yield func(OpenCodeSessionMeta) error,
	) error {
		return ForEachOpenCodeSessionMeta(ctx, dbPath, func(meta OpenCodeSessionMeta) error {
			meta.VirtualPath = virtualPath(dbPath, meta.SessionID)
			return yield(meta)
		})
	}
}

func streamOpenCodeSessionWatermarkMetaAs(
	virtualPath func(string, string) string,
) func(context.Context, string, func(OpenCodeSessionMeta) error) error {
	return func(
		ctx context.Context, dbPath string, yield func(OpenCodeSessionMeta) error,
	) error {
		return ForEachOpenCodeSessionWatermarkMeta(
			ctx, dbPath,
			func(meta OpenCodeSessionMeta) error {
				meta.VirtualPath = virtualPath(dbPath, meta.SessionID)
				return yield(meta)
			},
		)
	}
}

func listOpenCodeSessionWatermarkMetaAs(
	virtualPath func(string, string) string,
) func(string) ([]OpenCodeSessionMeta, error) {
	return func(dbPath string) ([]OpenCodeSessionMeta, error) {
		metas, err := ListOpenCodeSessionWatermarkMeta(dbPath)
		if err != nil {
			return nil, err
		}
		for i := range metas {
			metas[i].VirtualPath = virtualPath(dbPath, metas[i].SessionID)
		}
		return metas, nil
	}
}

// resolve detects the OpenCode storage backend for a root.
func (spec openCodeProviderSpec) resolve(root string) OpenCodeSource {
	return resolveOpenCodeFormatSource(spec.format, root)
}

// discover lists file-backed storage session JSON files under a root.
func (spec openCodeProviderSpec) discover(root string) []DiscoveredFile {
	return discoverOpenCodeFormatSessions(spec.format, root)
}

// find locates a session source path (storage JSON or SQLite virtual
// path) by raw session ID under a root.
func (spec openCodeProviderSpec) find(root, sessionID string) string {
	return findOpenCodeFormatSourceFile(spec.format, root, sessionID)
}

// storageIDs returns the set of session IDs present as storage JSON
// under a root, used to skip duplicate SQLite metas in hybrid roots.
func (spec openCodeProviderSpec) storageIDs(root string) map[string]struct{} {
	return openCodeFormatStorageSessionIDs(spec.format, root)
}

// parseVirtual splits an opencode-format SQLite virtual path
// (<dbPath>#<sessionID>) when the DB base name matches this agent.
func (spec openCodeProviderSpec) parseVirtual(
	sourcePath string,
) (dbPath, sessionID string, ok bool) {
	return parseOpenCodeFormatVirtualPath(spec.dbName, sourcePath)
}

func (spec openCodeProviderSpec) containerGlobs() []string {
	if spec.agent == AgentOpenCode {
		return []string{"opencode*.db", "opencode*.db-wal"}
	}
	return []string{spec.dbName, spec.dbName + "-wal"}
}

// parseFile parses a file-backed storage session and relabels it onto
// this agent's ID prefix when the agent is a fork of OpenCode.
func (spec openCodeProviderSpec) parseFile(
	sessionPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	sess, msgs, err := parseOpenCodeStorageFile(sessionPath, machine)
	if err != nil || sess == nil {
		return sess, msgs, err
	}
	if spec.relabel != nil {
		spec.relabel(sess)
	}
	return sess, msgs, nil
}

// parseSQLite parses a single SQLite-backed session and relabels it
// onto this agent's ID prefix when the agent is a fork of OpenCode.
func (spec openCodeProviderSpec) parseSQLite(
	dbPath, sessionID, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	sess, msgs, err := parseOpenCodeDBSession(dbPath, sessionID, machine)
	if err != nil || sess == nil {
		return sess, msgs, err
	}
	if spec.relabel != nil {
		spec.relabel(sess)
	}
	return sess, msgs, nil
}

type openCodeFormatSource struct {
	Root string
	Path string
	// MTimeNS carries the session's change signal (already listed during
	// SQLite discovery, scaled to nanoseconds) so Fingerprint does not
	// reopen the shared DB once per session. Zero means unknown and makes
	// Fingerprint fall back to querying the DB.
	MTimeNS int64
	// CompositeMTime reports that MTimeNS is the per-session composite
	// (session, project, and child message/part time_updated) rather than
	// the session row's own time_updated. It gates dropping the shared
	// container's size from the fingerprint.
	CompositeMTime bool
	// ChildDigest carries the deletion-sensitive per-session identity into
	// Fingerprint.Hash.
	ChildDigest string
	// WatermarkOnly marks MTimeNS as only the session-row watermark from a
	// bounded changed-path listing (see OpenCodeSessionMeta.WatermarkOnly).
	// The engine may skip such a source against its stored composite
	// watermark without resolving the child digest.
	WatermarkOnly bool
}

type openCodeFormatSourceIndex struct {
	projectMetadataSessions map[string]map[string]struct{}
	projectMetadataIndexed  map[string]struct{}
	projectMetadataErrors   map[string]map[openCodeMetadataErrorPathKey]struct{}
	projectMetadataMu       *sync.RWMutex
}

// openCodeMetadataErrorPathKey keeps per-path recovery state bounded without
// retaining archive-sized path strings in the factory-owned index.
type openCodeMetadataErrorPathKey [sha256.Size / 2]byte

func newOpenCodeFormatSourceIndex() *openCodeFormatSourceIndex {
	return &openCodeFormatSourceIndex{
		projectMetadataSessions: make(map[string]map[string]struct{}),
		projectMetadataIndexed:  make(map[string]struct{}),
		projectMetadataErrors:   make(map[string]map[openCodeMetadataErrorPathKey]struct{}),
		projectMetadataMu:       &sync.RWMutex{},
	}
}

type openCodeFormatSourceSet struct {
	roots []string
	spec  openCodeProviderSpec
	// containerTrusted, when non-nil, reports that a shared container is
	// byte-identical to the last fully verified pass (see
	// ProviderConfig.SQLiteContainerUnchangedSinceTrust). Discover answers
	// with the bounded watermark-only listing for such containers: the
	// engine's container gate skips every member before fingerprinting, so
	// the full child digest would be archive-sized work nothing reads.
	containerTrusted func(dbPath string) bool
	// projectMetadataSessions indexes only sessions whose cwd resolution uses
	// project metadata, so project events do not rescan concrete sessions.
	projectMetadataSessions map[string]map[string]struct{}
	projectMetadataIndexed  map[string]struct{}
	projectMetadataErrors   map[string]map[openCodeMetadataErrorPathKey]struct{}
	projectMetadataMu       *sync.RWMutex
}

func newOpenCodeFormatSourceSet(
	roots []string,
	spec openCodeProviderSpec,
	containerTrusted func(dbPath string) bool,
	sharedIndex ...*openCodeFormatSourceIndex,
) openCodeFormatSourceSet {
	index := (*openCodeFormatSourceIndex)(nil)
	if len(sharedIndex) > 0 {
		index = sharedIndex[0]
	}
	if index == nil {
		index = newOpenCodeFormatSourceIndex()
	}
	return openCodeFormatSourceSet{
		roots:                   cleanJSONLRoots(roots),
		spec:                    spec,
		containerTrusted:        containerTrusted,
		projectMetadataSessions: index.projectMetadataSessions,
		projectMetadataIndexed:  index.projectMetadataIndexed,
		projectMetadataErrors:   index.projectMetadataErrors,
		projectMetadataMu:       index.projectMetadataMu,
	}
}

func (s openCodeFormatSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	var incomplete error
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		src := s.spec.resolve(root)
		storageIDs := map[string]struct{}{}
		if src.Mode == OpenCodeSourceStorage {
			for _, file := range s.spec.discover(root) {
				s.indexStorageSession(root, file.Path)
				source, ok := s.sourceRef(root, file.Path, false)
				if !ok {
					continue
				}
				source.ProjectHint = file.Project
				addJSONLSource(source, &sources, seen)
			}
			s.markDiscoveredProjectMetadata(root)
			storageIDs = s.spec.storageIDs(root)
		}
		for _, dbPath := range src.DBPaths {
			trusted := s.containerTrusted != nil && s.containerTrusted(dbPath)
			dbSources, err := s.sqliteSources(ctx, root, dbPath, storageIDs, trusted)
			if err != nil {
				if ctx.Err() != nil {
					return nil, err
				}
				if src.Mode == OpenCodeSourceStorage {
					log.Printf("sync %s: skipping unreadable %s: %v",
						s.spec.agent, dbPath, err)
					continue
				}
				incomplete = errors.Join(incomplete, incompleteDiscoveryError(
					s.spec.agent, "read SQLite "+dbPath, err,
				))
				continue
			}
			for _, source := range dbSources {
				addJSONLSource(source, &sources, seen)
				_, id, _ := s.spec.parseVirtual(source.Key)
				storageIDs[id] = struct{}{}
			}
		}
	}
	sortJSONLSources(sources)
	return sources, incomplete
}

func (s openCodeFormatSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	var incomplete error
	wrappedYield := func(source SourceRef) error {
		if err := yield(source); err != nil {
			return discoveryYieldError{cause: err}
		}
		return nil
	}
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		continuable, err := s.discoverRootEach(ctx, root, wrappedYield)
		if err == nil {
			continue
		}
		if cause, ok := discoveryYieldCause(err); ok {
			return cause
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if continuable {
			incomplete = errors.Join(incomplete, err)
			continue
		}
		if incomplete == nil {
			return err
		}
		return errors.Join(incomplete, err)
	}
	return incomplete
}

type openCodeDiscoveryMapError struct{ cause error }

func (err openCodeDiscoveryMapError) Error() string { return err.cause.Error() }
func (err openCodeDiscoveryMapError) Unwrap() error { return err.cause }

func (s openCodeFormatSourceSet) discoverRootEach(
	ctx context.Context, root string, yield func(SourceRef) error,
) (continuable bool, retErr error) {
	src := s.spec.resolve(root)
	hasSQLite := len(src.DBPaths) > 0
	var storageIDs *discoveryDiskMap
	if hasSQLite && (src.Mode == OpenCodeSourceStorage || len(src.DBPaths) > 1) {
		var err error
		storageIDs, err = newDiscoveryDiskMapForContext(ctx)
		if err != nil {
			return false, err
		}
		defer func() {
			if cleanupErr := storageIDs.close(); cleanupErr != nil {
				continuable = false
				retErr = errors.Join(retErr, cleanupErr)
			}
		}()
	}
	if src.Mode == OpenCodeSourceStorage {
		if err := s.discoverStorageEach(ctx, root, src, storageIDs, yield); err != nil {
			if _, ok := errors.AsType[openCodeDiscoveryMapError](err); ok {
				return false, err
			}
			return true, incompleteDiscoveryError(
				s.spec.agent, "stream storage "+src.SessionRoot, err,
			)
		}
	}
	if !hasSQLite {
		return false, nil
	}
	var incomplete error
	for _, dbPath := range src.DBPaths {
		var callbackErr error
		var membershipErr error
		stream := s.spec.streamSQLite
		if s.containerTrusted != nil && s.containerTrusted(dbPath) &&
			s.spec.streamSQLiteWatermark != nil {
			stream = s.spec.streamSQLiteWatermark
		}
		err := stream(ctx, dbPath, func(meta OpenCodeSessionMeta) error {
			if storageIDs != nil {
				_, exists, err := storageIDs.get(ctx, meta.SessionID)
				if err != nil {
					membershipErr = err
					return err
				}
				if exists {
					return nil
				}
			}
			source, ok := s.sqliteSourceRefFromMeta(root, meta)
			if !ok {
				return nil
			}
			callbackErr = yield(source)
			if callbackErr == nil && storageIDs != nil {
				callbackErr = storageIDs.put(
					ctx, meta.SessionID, meta.SessionID, false,
				)
			}
			return callbackErr
		})
		if callbackErr != nil {
			return false, callbackErr
		}
		if membershipErr != nil {
			return false, membershipErr
		}
		if err != nil {
			incomplete = errors.Join(
				incomplete,
				incompleteDiscoveryError(
					s.spec.agent, "stream SQLite "+dbPath, err,
				),
			)
		}
	}
	if incomplete != nil {
		return true, incomplete
	}
	return false, nil
}

func (s openCodeFormatSourceSet) discoverStorageEach(
	ctx context.Context,
	root string,
	src OpenCodeSource,
	storageIDs *discoveryDiskMap,
	yield func(SourceRef) error,
) error {
	var callbackErr error
	err := streamDirectoryEntries(ctx, src.SessionRoot, func(project os.DirEntry) error {
		isProjectDir, dirErr := streamingDirCandidateOrIncomplete(
			s.spec.agent, "project directory", project, src.SessionRoot,
		)
		if dirErr != nil {
			return dirErr
		}
		if !isProjectDir {
			return nil
		}
		projectDir := filepath.Join(src.SessionRoot, project.Name())
		err := streamDirectoryEntries(ctx, projectDir, func(entry os.DirEntry) error {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				return nil
			}
			path := filepath.Join(projectDir, entry.Name())
			s.indexStorageSession(root, path)
			source, ok := s.sourceRef(root, path, false)
			if !ok {
				return nil
			}
			if storageIDs != nil {
				id := strings.TrimSuffix(entry.Name(), ".json")
				if id != "" {
					if err := storageIDs.put(ctx, id, id, false); err != nil {
						return openCodeDiscoveryMapError{cause: err}
					}
				}
			}
			source.ProjectHint = openCodeSessionProject(path)
			callbackErr = yield(source)
			return callbackErr
		})
		if callbackErr != nil {
			return callbackErr
		}
		if err != nil {
			return err
		}
		s.markProjectMetadataIndexed(root, project.Name())
		return nil
	})
	if callbackErr != nil {
		return callbackErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func (s openCodeFormatSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, 2*len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, s.watchUnits(root)...)
	}
	return WatchPlan{Roots: roots}, nil
}

// watchUnits returns the coverage units for one configured root. The database
// and its WAL are direct children of the root, so a shallow container unit
// covers them, and a shallow watch never draws on the shared recursive budget.
// Under one recursive unit the archive and the container shared that budget:
// once earlier roots had spent it, the whole root registered with no native
// watch at all, so a large archive elsewhere in the plan could leave a live
// SQLite container uncovered. Exhaustion inside this root's own walk also
// marked the container's coverage degraded along with the archive's, although
// the root's watch had already been installed.
func (s openCodeFormatSourceSet) watchUnits(root string) []WatchRoot {
	if !s.splitsCoverageUnits(root) {
		return []WatchRoot{{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: append([]string{"*.json"}, s.spec.containerGlobs()...),
			DebounceKey:  string(s.spec.agent) + ":opencode:" + root,
		}}
	}
	return []WatchRoot{
		{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: s.spec.containerGlobs(),
			DebounceKey:  string(s.spec.agent) + ":container:" + root,
		},
		{
			Path:         openCodeStorageWatchDir(root),
			Recursive:    true,
			IncludeGlobs: []string{"*.json"},
			DebounceKey:  string(s.spec.agent) + ":storage:" + root,
		},
	}
}

// splitsCoverageUnits reports whether a configured root plans separate
// container and storage units.
//
// The split needs an existing storage directory. Naming an absent one would
// plan a watch root that cannot be established, and its polling obligation
// probes a path that may never appear, which defers every other obligation on
// the same configured dir. While storage is absent the single recursive unit
// costs one native watch and still covers the tree if it is created later, and
// the root's own watch is installed first so a growing archive can no longer
// starve the database.
//
// A symlinked root also keeps the single unit. The daemon refuses to watch a
// recursive root through a symlink and gates the configured dir's
// reconciliation on the link target instead, and that check reads the unit's
// Recursive flag: a shallow container unit would slip past it while the
// storage unit walked the link's target anyway.
//
// The split is forfeited when another provider plans a recursive root at the
// same path. The daemon keeps one watch root per path and merges a shallow
// unit into a recursive one, so the merged root's walk covers the archive
// again and the two share a budget once more. The merged root still gets its
// own native watch, so nothing is left uncovered; only the isolation is lost,
// and only for a directory two providers were configured to share.
func (s openCodeFormatSourceSet) splitsCoverageUnits(root string) bool {
	return !isSymlinkWatchPath(root) &&
		isDirWatchPath(openCodeStorageWatchDir(root))
}

// isSymlinkWatchPath reports whether path is itself a symbolic link, without
// resolving it.
func isSymlinkWatchPath(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info == nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

// isDirWatchPath reports whether path resolves to a directory.
func isDirWatchPath(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info == nil {
		return false
	}
	return info.IsDir()
}

// openCodeStorageWatchDir is the recursive storage unit's path for a
// configured root.
func openCodeStorageWatchDir(root string) string {
	return filepath.Join(root, "storage")
}

// reconciliationContainer maps a requested path to the SQLite container that
// atomically owns it, without statting: a deleted database must still resolve
// so its members remain reclaimable through the container proof. A virtual
// spelling splits at the raw separator instead of parseVirtual, whose exact
// basename check would let a Windows case variant of the database name skip
// widening and admit a single member; the alias comparison below already
// carries the platform's case rule.
func (s openCodeFormatSourceSet) reconciliationContainer(
	requested string,
) (string, bool) {
	physical := requested
	if idx := strings.LastIndex(requested, "#"); idx > 0 && idx < len(requested)-1 {
		physical = requested[:idx]
	}
	for _, root := range s.roots {
		if db, ok := s.spec.dbPathForEvent(root, physical); ok {
			return cleanReconciliationScopeRoot(db), true
		}
	}
	return "", false
}

func (s openCodeFormatSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.unitScopeAllows(req) {
		return nil, nil
	}
	if dbPath, _, virtual := s.spec.parseVirtual(req.Path); virtual {
		for _, root := range s.roots {
			if _, under := relUnder(root, dbPath); !under {
				continue
			}
			source, ok, err := s.canonicalVirtualSource(ctx, root, req.Path)
			if err != nil || !ok {
				return nil, err
			}
			return []SourceRef{source}, nil
		}
		return nil, nil
	}
	pathExists := true
	if _, err := os.Stat(req.Path); err != nil {
		if !os.IsNotExist(err) {
			return nil, nil
		}
		pathExists = false
	}
	for _, root := range s.roots {
		sources, ok, err := s.sourcesForChangedPathInRoot(
			ctx, root, req, pathExists,
		)
		if err != nil || ok {
			return sources, err
		}
	}
	return nil, nil
}

// unitScopeAllows scopes changed-path classification to the coverage units of
// the configured root that owns the path. The engine calls
// SourcesForChangedPath once per emitted watch root per event, so with two
// units per configured root an unscoped WAL event would run the whole SQLite
// fan-out twice. A request whose path (or, for a virtual path, its physical
// database path) lies outside req.WatchRoot belongs to another unit and yields
// no sources, and configured roots can nest, so only the most specific
// containing root's units claim it.
//
// The container unit does not defer storage paths to the storage unit, even
// though only one of the two watches them. Whether the storage unit exists is
// a filesystem fact, and the engine resolves each provider's watch roots once
// and reuses that set for the life of the process: a storage tree created
// after that set was cached would be dispatched only against the container
// root while the scope rule had already handed it to a root the caller never
// passes, so no unit would claim it at all. Classifying a storage path twice
// costs one repeated source lookup. The expensive claim, the shared
// container's session listing, still cannot double, because a storage watch
// root never contains the database or its WAL.
//
// An empty req.WatchRoot preserves unscoped behavior for callers that do not
// dispatch per watch root.
func (s openCodeFormatSourceSet) unitScopeAllows(req ChangedPathRequest) bool {
	if req.WatchRoot == "" {
		return true
	}
	path := req.Path
	if dbPath, _, virtual := s.spec.parseVirtual(req.Path); virtual {
		path = dbPath
	}
	if !pathAtOrUnder(req.WatchRoot, path) {
		return false
	}
	owner, owned := s.mostSpecificRootFor(path)
	if !owned {
		return true
	}
	return reconciliationScopeSamePath(req.WatchRoot, owner) ||
		reconciliationScopeSamePath(req.WatchRoot, openCodeStorageWatchDir(owner))
}

// mostSpecificRootFor returns the deepest configured root containing path.
func (s openCodeFormatSourceSet) mostSpecificRootFor(path string) (string, bool) {
	best := ""
	for _, root := range s.roots {
		clean := filepath.Clean(root)
		if !pathAtOrUnder(clean, path) {
			continue
		}
		if len(clean) > len(best) {
			best = clean
		}
	}
	return best, best != ""
}

// pathAtOrUnder reports whether path is root itself or lies within it.
func pathAtOrUnder(root, path string) bool {
	if filepath.Clean(root) == filepath.Clean(path) {
		return true
	}
	_, under := relUnder(root, path)
	return under
}

func (s openCodeFormatSourceSet) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, root := range s.roots {
		source, ok := s.sourceRef(root, path, true)
		if !ok {
			continue
		}
		if project != "" {
			source.ProjectHint = project
		}
		return source, true, nil
	}
	return SourceRef{}, false, nil
}

var errOpenCodeCanonicalSourceFound = errors.New("opencode canonical source found")

func (s openCodeFormatSourceSet) canonicalVirtualSource(
	ctx context.Context, root, virtualPath string,
) (SourceRef, bool, error) {
	_, sessionID, ok := s.spec.parseVirtual(virtualPath)
	if !ok {
		return SourceRef{}, false, nil
	}
	src := s.spec.resolve(root)
	if src.Mode == OpenCodeSourceStorage {
		var found SourceRef
		err := streamDirectoryEntries(ctx, src.SessionRoot, func(project os.DirEntry) error {
			if !isDirOrSymlink(project, src.SessionRoot) {
				return nil
			}
			path := filepath.Join(src.SessionRoot, project.Name(), sessionID+".json")
			if source, ok := s.sourceRef(root, path, false); ok {
				found = source
				return errOpenCodeCanonicalSourceFound
			}
			return nil
		})
		switch {
		case errors.Is(err, errOpenCodeCanonicalSourceFound):
			return found, true, nil
		case ctx.Err() != nil:
			return SourceRef{}, false, ctx.Err()
		}
	}
	source, ok := s.sourceRef(root, virtualPath, false)
	return source, ok, nil
}

func (s openCodeFormatSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			if source, ok := s.sourceRef(root, path, true); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.spec.find(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path, false); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

// sourceMtimeWithComposite resolves a source's change signal when discovery did
// not carry one (FindSource lookups, storage sessions), reporting whether the
// value is the per-session composite.
func (s openCodeFormatSourceSet) sourceMtimeWithComposite(
	path string,
) (int64, string, bool, error) {
	if dbPath, sessionID, ok := s.spec.parseVirtual(path); ok {
		return openCodeSQLiteSessionMtimeComposite(dbPath, sessionID)
	}
	mtime, err := s.spec.sourceMtime(path)
	return mtime, "", false, err
}

func (s openCodeFormatSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("%s source path unavailable", s.spec.agent)
	}
	mtime := sourceCarriedMTimeNS(source)
	composite := sourceCarriedCompositeMTime(source)
	digest := sourceCarriedChildDigest(source)
	dbPath, _, sqliteSource := s.spec.parseVirtual(path)
	var storageSnapshot *openCodeStorageSnapshot
	if !sqliteSource && mtime == 0 {
		snapshot, err := loadOpenCodeStorageSnapshot(path, true)
		if err != nil {
			return SourceFingerprint{}, err
		}
		storageSnapshot = &snapshot
		mtime = snapshot.fileMtime
	}
	// Only re-open the container when a digest is actually expected. A legacy
	// container reports composite=false and carries an empty digest by design,
	// so treating "empty" alone as "missing" would reopen and re-query the
	// shared database once per session on every cold or changed-container pass.
	if sqliteSource && (mtime == 0 || (composite && digest == "")) {
		// Sources rebuilt by FindSource or reconciliation carry no discovery
		// metadata, and watermark-only changed-path sources carry a
		// deliberately unresolved digest. Without this the hash would be
		// empty, and an empty hash is treated as no constraint by the
		// freshness gate — so a deletion-only change would pass unnoticed on
		// every non-discovery path.
		lookupMtime, lookupDigest, lookupComposite, err :=
			s.sourceMtimeWithComposite(path)
		if err != nil {
			return SourceFingerprint{}, err
		}
		// Adopt the looked-up watermark alongside the digest: a
		// watermark-only source carries the session-row watermark, which can
		// sit below the composite the digest folds in. The stored MTimeNS
		// must always be the composite, or the next full-discovery pass
		// would see a mismatched watermark and re-parse an unchanged session.
		if lookupMtime != 0 {
			mtime, composite = lookupMtime, lookupComposite
		} else if mtime == 0 {
			composite = lookupComposite
		}
		if digest == "" {
			digest = lookupDigest
		}
	}
	fingerprint := SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		MTimeNS: mtime,
	}
	if sqliteSource {
		info, err := os.Stat(dbPath)
		if err != nil {
			return SourceFingerprint{}, fmt.Errorf("stat %s: %w", dbPath, err)
		}
		// The watermark alone cannot see a deleted child, because the session
		// or project row usually already holds the higher timestamp. The
		// digest folds in the child row counts so a delete changes the
		// fingerprint; FingerprintHashRequiredForFreshness makes the gate
		// compare it against the stored value.
		fingerprint.Hash = digest
		// Every session in this root shares one physical container, so the
		// container's size moves whenever any single session is written.
		// Stamping it onto a per-session fingerprint made one session's
		// append change the fingerprint of every other session in the
		// container, dropping their freshness skip and re-parsing the whole
		// root for one changed session. When MTimeNS is the per-session
		// composite it already discriminates per session (including in-place
		// child edits and project worktree renames), so the container stat
		// is existence-only. Legacy containers whose schema cannot produce
		// the composite keep the size as their conservative fallback.
		if !composite {
			fingerprint.Size = info.Size()
		}
		return fingerprint, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	fingerprint.Size = info.Size()
	// Storage sessions must expose the same content fingerprint that Parse
	// persists. Project metadata is part of that fingerprint, so a metadata
	// rewrite still invalidates a row whose session size and mtime are stable.
	if storageSnapshot == nil {
		snapshot, snapshotErr := loadOpenCodeStorageSnapshot(path, false)
		if snapshotErr != nil {
			return SourceFingerprint{}, snapshotErr
		}
		storageSnapshot = &snapshot
	}
	fingerprint.Hash, err = openCodeStorageFingerprintFromSnapshot(*storageSnapshot)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return fingerprint, nil
}

// sourceCarriedMTimeNS returns the discovery-listed session mtime carried on
// a SQLite-backed source, or zero when the source was built without one
// (storage sessions, FindSource lookups).
func sourceCarriedChildDigest(source SourceRef) string {
	switch src := source.Opaque.(type) {
	case openCodeFormatSource:
		return src.ChildDigest
	case *openCodeFormatSource:
		if src != nil {
			return src.ChildDigest
		}
	}
	return ""
}

// SourceWatermarkOnlyMTimeNS returns the carried session-row watermark for a
// shared-container source listed by a watermark-only changed-path scan, and
// whether the source is such a listing. Full-discovery sources carry the
// composite watermark and child digest instead and report false, as do
// legacy containers without composite support.
func SourceWatermarkOnlyMTimeNS(source SourceRef) (int64, bool) {
	switch src := source.Opaque.(type) {
	case openCodeFormatSource:
		if src.WatermarkOnly {
			return src.MTimeNS, true
		}
	case *openCodeFormatSource:
		if src != nil && src.WatermarkOnly {
			return src.MTimeNS, true
		}
	}
	return 0, false
}

func sourceCarriedCompositeMTime(source SourceRef) bool {
	switch src := source.Opaque.(type) {
	case openCodeFormatSource:
		return src.CompositeMTime
	case *openCodeFormatSource:
		if src != nil {
			return src.CompositeMTime
		}
	}
	return false
}

func sourceCarriedMTimeNS(source SourceRef) int64 {
	switch src := source.Opaque.(type) {
	case openCodeFormatSource:
		return src.MTimeNS
	case *openCodeFormatSource:
		if src != nil {
			return src.MTimeNS
		}
	}
	return 0
}

func (s openCodeFormatSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case openCodeFormatSource:
		return src.Path, src.Path != ""
	case *openCodeFormatSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate, false); ok {
				src := ref.Opaque.(openCodeFormatSource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s openCodeFormatSourceSet) sqliteSources(
	ctx context.Context,
	root string,
	dbPath string,
	storageIDs map[string]struct{},
	watermarkOnly bool,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lister := s.spec.listSQLite
	if watermarkOnly && s.spec.listSQLiteWatermark != nil {
		lister = s.spec.listSQLiteWatermark
	}
	metas, err := lister(dbPath)
	if err != nil {
		return nil, err
	}
	sources := make([]SourceRef, 0, len(metas))
	for _, meta := range metas {
		if _, exists := storageIDs[meta.SessionID]; exists {
			continue
		}
		// meta was just read from this DB, so the session row is known to
		// exist. Build the SourceRef directly instead of routing through
		// sourceRef, which reopens the same SQLite DB once per row via
		// OpenCodeSQLiteSessionExists (O(n) opens for n sessions, and it would
		// silently drop a row whose redundant probe failed).
		source, ok := s.sqliteSourceRefFromMeta(root, meta)
		if !ok {
			continue
		}
		sources = append(sources, source)
	}
	return sources, nil
}

// changedWatermarkSources answers a shared-container change event with only
// the members whose carried session-row watermark is not already covered by
// the caller's stored freshness. The watermark listing streams in ascending
// virtual-path order and the stored side arrives through a paged cursor in
// the same order, so peak memory is one stored page plus the changed batch —
// never the container's full membership. A pager failure fails open: the
// remaining stream is kept unfiltered and the caller's per-file gate decides.
// Legacy rows without composite support (WatermarkOnly false) are always
// kept; their conservative container-size fingerprint must not be bypassed.
func (s openCodeFormatSourceSet) changedWatermarkSources(
	ctx context.Context,
	root string,
	dbPath string,
	storageIDs map[string]struct{},
	freshness StoredMemberFreshnessPager,
) ([]SourceRef, error) {
	cursor := storedMemberFreshnessCursor{pager: freshness}
	var sources []SourceRef
	err := s.spec.streamSQLiteWatermark(ctx, dbPath, func(meta OpenCodeSessionMeta) error {
		if _, exists := storageIDs[meta.SessionID]; exists {
			return nil
		}
		if meta.WatermarkOnly && !cursor.failed {
			covered, err := cursor.covers(ctx, meta.VirtualPath, meta.FileMtime)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				cursor.failed = true
			} else if covered {
				return nil
			}
		}
		source, ok := s.sqliteSourceRefFromMeta(root, meta)
		if !ok {
			return nil
		}
		sources = append(sources, source)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sources, nil
}

const storedMemberFreshnessPageSize = 512

// storedMemberFreshnessCursor advances through the caller's paged stored
// freshness in step with an ascending virtual-path stream, retaining one page
// at a time.
type storedMemberFreshnessCursor struct {
	pager  StoredMemberFreshnessPager
	rows   []StoredMemberFreshness
	index  int
	after  string
	done   bool
	failed bool
}

// covers reports whether the stored side vouches for path at watermarkNS.
// Paths must arrive in ascending order across calls.
func (c *storedMemberFreshnessCursor) covers(
	ctx context.Context, path string, watermarkNS int64,
) (bool, error) {
	for {
		for c.index < len(c.rows) {
			row := c.rows[c.index]
			if row.Path < path {
				c.index++
				continue
			}
			if row.Path > path {
				return false, nil
			}
			return watermarkNS <= row.CoveredThroughNS, nil
		}
		if c.done {
			return false, nil
		}
		rows, done, err := c.pager(ctx, c.after, storedMemberFreshnessPageSize)
		if err != nil {
			return false, err
		}
		c.rows, c.index, c.done = rows, 0, done
		if len(rows) > 0 {
			c.after = rows[len(rows)-1].Path
		} else if !done {
			// A pager reporting neither rows nor completion cannot make
			// progress; treat the stored side as exhausted.
			c.done = true
		}
	}
}

// sqliteSourceRefFromMeta builds a SourceRef for a session row already listed
// from the SQLite DB at root. It validates the virtual path parses and that its
// DB lives under root, but unlike sourceRef it skips the per-row
// OpenCodeSQLiteSessionExists probe because the caller read the row from that
// same DB moments earlier. The listed time_updated rides along so Fingerprint
// does not reopen the DB per session.
func (s openCodeFormatSourceSet) sqliteSourceRefFromMeta(
	root string,
	meta OpenCodeSessionMeta,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path := filepath.Clean(meta.VirtualPath)
	dbPath, _, ok := s.spec.parseVirtual(path)
	if !ok {
		return SourceRef{}, false
	}
	if _, under := relUnder(root, dbPath); !under {
		return SourceRef{}, false
	}
	ref := s.newSourceRef(root, path, "")
	if src, ok := ref.Opaque.(openCodeFormatSource); ok {
		src.MTimeNS = meta.FileMtime
		src.CompositeMTime = meta.CompositeMtime
		src.ChildDigest = meta.ChildDigest
		src.WatermarkOnly = meta.WatermarkOnly
		ref.Opaque = src
	}
	return ref, true
}

func (s openCodeFormatSourceSet) sourcesForChangedPathInRoot(
	ctx context.Context,
	root string,
	req ChangedPathRequest,
	pathExists bool,
) ([]SourceRef, bool, error) {
	path := req.Path
	rel, ok := relUnder(root, path)
	if !ok {
		return nil, false, nil
	}
	relevance, dbPath := s.sqliteChangeRelevance(root, path, rel)
	if dbPath != "" && relevance == ChangedPathNonData {
		return nil, true, nil
	}

	if dbPath != "" {
		if !IsRegularFile(dbPath) {
			return nil, true, nil
		}
		storageIDs := map[string]struct{}{}
		if s.spec.resolve(root).Mode == OpenCodeSourceStorage {
			storageIDs = s.spec.storageIDs(root)
		}
		for _, candidate := range s.spec.resolve(root).DBPaths {
			if reconciliationScopeSamePath(candidate, dbPath) {
				break
			}
			metas, err := s.spec.listSQLite(candidate)
			if err != nil {
				return nil, true, err
			}
			for _, meta := range metas {
				storageIDs[meta.SessionID] = struct{}{}
			}
		}
		if req.AllowWatermarkOnlySources &&
			req.StoredMemberFreshnessPage != nil &&
			s.spec.streamSQLiteWatermark != nil {
			sources, err := s.changedWatermarkSources(
				ctx, root, dbPath, storageIDs, req.StoredMemberFreshnessPage,
			)
			return sources, true, err
		}
		sources, err := s.sqliteSources(
			ctx, root, dbPath, storageIDs, req.AllowWatermarkOnlySources,
		)
		return sources, true, err
	}
	src := s.spec.resolve(root)
	if src.Mode != OpenCodeSourceStorage {
		return nil, false, nil
	}
	if projectID, ok := openCodeProjectIDFromPath(root, path); ok {
		sources, err := s.sourcesForProject(root, projectID)
		return sources, true, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	sessionSubdir := filepath.Base(src.SessionRoot)
	switch {
	case pathExists &&
		len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == sessionSubdir &&
		strings.HasSuffix(parts[3], ".json"):
		s.indexStorageSession(root, path)
		source, ok := s.sourceRef(root, path, false)
		if !ok {
			return nil, true, nil
		}
		return []SourceRef{source}, true, nil
	case !pathExists &&
		len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == sessionSubdir &&
		strings.HasSuffix(parts[3], ".json"):
		s.removeStorageSession(root, path)
		source, ok := s.sourceRefFromStoragePath(root, path)
		if !ok {
			return nil, true, nil
		}
		return []SourceRef{source}, true, nil
	case len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == "message" &&
		strings.HasSuffix(parts[3], ".json"):
		source, ok := s.sourceForRawID(root, parts[2])
		if !ok {
			return nil, false, nil
		}
		return []SourceRef{source}, true, nil
	case len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == "part" &&
		strings.HasSuffix(parts[3], ".json"):
		sessionID := ""
		if pathExists {
			sessionID = readOpenCodeProviderStorageSessionID(path)
		}
		if sessionID == "" {
			sessionID = findOpenCodeProviderStorageSessionIDByMessageID(root, parts[2])
		}
		if sessionID == "" {
			return nil, false, nil
		}
		source, ok := s.sourceForRawID(root, sessionID)
		if !ok {
			return nil, false, nil
		}
		return []SourceRef{source}, true, nil
	case !pathExists &&
		len(parts) == 3 &&
		parts[0] == "storage" &&
		parts[1] == "message":
		source, ok := s.sourceForRawID(root, parts[2])
		if !ok {
			return nil, false, nil
		}
		return []SourceRef{source}, true, nil
	case !pathExists &&
		len(parts) == 3 &&
		parts[0] == "storage" &&
		parts[1] == "part":
		sessionID := findOpenCodeProviderStorageSessionIDByMessageID(root, parts[2])
		if sessionID == "" {
			return nil, false, nil
		}
		source, ok := s.sourceForRawID(root, sessionID)
		if !ok {
			return nil, false, nil
		}
		return []SourceRef{source}, true, nil
	}
	return nil, false, nil
}

func (s openCodeFormatSourceSet) sourcesForProject(
	root, projectID string,
) ([]SourceRef, error) {
	if projectID == "" {
		return nil, nil
	}
	src := s.spec.resolve(root)
	if src.Mode != OpenCodeSourceStorage {
		return nil, nil
	}
	if err := s.ensureProjectMetadataIndex(root, src.SessionRoot, projectID); err != nil {
		return nil, err
	}
	key := projectMetadataIndexKey(root, projectID)
	s.projectMetadataMu.RLock()
	paths := make([]string, 0, len(s.projectMetadataSessions[key]))
	for path := range s.projectMetadataSessions[key] {
		paths = append(paths, path)
	}
	hasErrors := len(s.projectMetadataErrors[key]) > 0
	s.projectMetadataMu.RUnlock()
	sort.Strings(paths)
	sources := make([]SourceRef, 0, len(paths))
	for _, path := range paths {
		if !IsRegularFile(path) {
			s.removeStorageSession(root, path)
			continue
		}
		if source, ok := s.sourceRefFromStoragePath(root, path); ok {
			sources = append(sources, source)
		}
	}
	if hasErrors {
		var err error
		sources, err = s.appendProjectMetadataErrorSources(
			root, src.SessionRoot, projectID, sources,
		)
		if err != nil {
			return nil, err
		}
		sort.Slice(sources, func(i, j int) bool {
			return sources[i].DisplayPath < sources[j].DisplayPath
		})
	}
	return sources, nil
}

func (s openCodeFormatSourceSet) appendProjectMetadataErrorSources(
	root, sessionRoot, projectID string, sources []SourceRef,
) ([]SourceRef, error) {
	dir := filepath.Join(sessionRoot, projectID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return sources, nil
		}
		return nil, fmt.Errorf("reading opencode session project %s: %w", dir, err)
	}
	known := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		known[source.DisplayPath] = struct{}{}
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if _, ok := known[path]; ok ||
			!openCodeStorageSessionUsesProjectMetadata(path) {
			continue
		}
		if source, ok := s.sourceRefFromStoragePath(root, path); ok {
			sources = append(sources, source)
			known[path] = struct{}{}
		}
	}
	return sources, nil
}

func projectMetadataIndexKey(root, projectID string) string {
	return filepath.Clean(root) + "\x00" + projectID
}

func projectMetadataErrorPathKey(path string) openCodeMetadataErrorPathKey {
	digest := sha256.Sum256([]byte(filepath.Clean(path)))
	var key openCodeMetadataErrorPathKey
	copy(key[:], digest[:])
	return key
}

func (s openCodeFormatSourceSet) indexStorageSession(root, path string) {
	projectID := filepath.Base(filepath.Dir(path))
	if projectID == "." || projectID == "" {
		return
	}
	key := projectMetadataIndexKey(root, projectID)
	errorPathKey := projectMetadataErrorPathKey(path)
	usesProject, malformed := openCodeStorageSessionIndexState(path)
	s.projectMetadataMu.Lock()
	paths := s.projectMetadataSessions[key]
	if paths == nil {
		paths = make(map[string]struct{})
		s.projectMetadataSessions[key] = paths
	}
	if malformed {
		errors := s.projectMetadataErrors[key]
		if errors == nil {
			errors = make(map[openCodeMetadataErrorPathKey]struct{})
			s.projectMetadataErrors[key] = errors
		}
		errors[errorPathKey] = struct{}{}
		delete(paths, path)
	} else {
		errors := s.projectMetadataErrors[key]
		delete(errors, errorPathKey)
		if len(errors) == 0 {
			delete(s.projectMetadataErrors, key)
		}
		if usesProject {
			paths[path] = struct{}{}
		} else {
			delete(paths, path)
		}
	}
	s.projectMetadataMu.Unlock()
}

func (s openCodeFormatSourceSet) removeStorageSession(root, path string) {
	projectID := filepath.Base(filepath.Dir(path))
	key := projectMetadataIndexKey(root, projectID)
	errorPathKey := projectMetadataErrorPathKey(path)
	s.projectMetadataMu.Lock()
	delete(s.projectMetadataSessions[key], path)
	errors := s.projectMetadataErrors[key]
	delete(errors, errorPathKey)
	if len(errors) == 0 {
		delete(s.projectMetadataErrors, key)
	}
	s.projectMetadataMu.Unlock()
}

func (s openCodeFormatSourceSet) markProjectMetadataIndexed(
	root, projectID string,
) {
	s.projectMetadataMu.Lock()
	s.projectMetadataIndexed[projectMetadataIndexKey(root, projectID)] = struct{}{}
	s.projectMetadataMu.Unlock()
}

func (s openCodeFormatSourceSet) markDiscoveredProjectMetadata(root string) {
	s.projectMetadataMu.Lock()
	defer s.projectMetadataMu.Unlock()
	for projectID := range s.projectMetadataSessions {
		prefix := filepath.Clean(root) + "\x00"
		if strings.HasPrefix(projectID, prefix) {
			s.projectMetadataIndexed[projectID] = struct{}{}
		}
	}
}

func (s openCodeFormatSourceSet) ensureProjectMetadataIndex(
	root, sessionRoot, projectID string,
) error {
	key := projectMetadataIndexKey(root, projectID)
	s.projectMetadataMu.RLock()
	_, indexed := s.projectMetadataIndexed[key]
	s.projectMetadataMu.RUnlock()
	if indexed {
		return nil
	}
	dir := filepath.Join(sessionRoot, projectID)
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			s.markProjectMetadataIndexed(root, projectID)
			return nil
		}
		return fmt.Errorf("stat opencode session project %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("opencode session project %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			s.markProjectMetadataIndexed(root, projectID)
			return nil
		}
		return fmt.Errorf("reading opencode session project %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		s.indexStorageSession(root, filepath.Join(dir, entry.Name()))
	}
	s.markProjectMetadataIndexed(root, projectID)
	return nil
}

// Project changes only affect sessions that need project metadata for cwd.
// Keep unreadable or malformed sessions so their parse errors stay visible.
func openCodeStorageSessionUsesProjectMetadata(path string) bool {
	usesProject, _ := openCodeStorageSessionIndexState(path)
	return usesProject
}

func openCodeStorageSessionIndexState(path string) (usesProject, malformed bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return true, true
	}
	var session struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
	}
	if json.Unmarshal(raw, &session) != nil {
		return true, true
	}
	if session.ID == "" {
		return true, true
	}
	return !openCodeUsableWorktree(strings.TrimSpace(session.Directory)), false
}

func openCodeProjectIDFromPath(root, path string) (string, bool) {
	rel, ok := relUnder(root, path)
	if !ok {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] != "storage" ||
		parts[1] != "project" || !strings.HasSuffix(parts[2], ".json") {
		return "", false
	}
	projectID := strings.TrimSuffix(parts[2], ".json")
	return projectID, projectID != ""
}

func (s openCodeFormatSourceSet) ChangedPathRelevance(
	ctx context.Context,
	req ChangedPathRequest,
) (ChangedPathRelevance, error) {
	if err := ctx.Err(); err != nil {
		return ChangedPathUnclassified, err
	}
	if req.WatchRoot != "" {
		watchRoot := filepath.Clean(req.WatchRoot)
		for _, root := range s.roots {
			if samePath(watchRoot, root) ||
				samePath(watchRoot, openCodeStorageWatchDir(root)) {
				return s.changedPathRelevanceInRoot(root, req.Path), nil
			}
		}
		return ChangedPathUnclassified, nil
	}
	for _, root := range s.roots {
		if relevance := s.changedPathRelevanceInRoot(root, req.Path); relevance != ChangedPathUnclassified {
			return relevance, nil
		}
	}
	return ChangedPathUnclassified, nil
}

func (s openCodeFormatSourceSet) changedPathRelevanceInRoot(
	root, path string,
) ChangedPathRelevance {
	rel, ok := relUnder(root, path)
	if !ok {
		return ChangedPathUnclassified
	}
	src := s.spec.resolve(root)
	if src.Mode == OpenCodeSourceStorage {
		if _, ok := openCodeProjectIDFromPath(root, path); ok {
			return ChangedPathDataBearing
		}
	}
	relevance, _ := s.sqliteChangeRelevance(root, path, rel)
	return relevance
}

func (s openCodeFormatSourceSet) sqliteChangeRelevance(
	root, path, rel string,
) (ChangedPathRelevance, string) {
	dbPath, ok := s.spec.dbPathForEvent(root, path)
	if !ok {
		return ChangedPathUnclassified, ""
	}
	switch {
	case strings.HasSuffix(rel, "-shm"):
		// SHM is only SQLite's WAL index. WAL frames or the checkpointed main
		// database carry the source changes, so SHM events are redundant.
		return ChangedPathNonData, dbPath
	case strings.HasSuffix(rel, "-wal"):
		// A read-only connection can create an empty WAL while inspecting a
		// quiet database. Ignore it, as well as WAL removal after a checkpoint;
		// the corresponding main-database write is watched separately.
		if !sqliteWALHasFrames(path) {
			return ChangedPathNonData, dbPath
		}
		return ChangedPathDataBearing, dbPath
	default:
		// A missing main database is still a data-bearing change. It can mean
		// the container moved or was removed, so the watch push must remain.
		return ChangedPathDataBearing, dbPath
	}
}

func (spec openCodeProviderSpec) dbPathForEvent(root, path string) (string, bool) {
	path = filepath.Clean(path)
	for _, suffix := range []string{"-wal", "-shm"} {
		path = strings.TrimSuffix(path, suffix)
	}
	name := filepath.Base(path)
	if !reconciliationScopeSamePath(
		cleanReconciliationScopeRoot(filepath.Dir(path)),
		cleanReconciliationScopeRoot(root),
	) || !spec.format.matchesDBName(name) {
		return "", false
	}
	if strings.EqualFold(name, spec.format.dbName) {
		name = spec.format.dbName
	}
	return filepath.Join(root, name), true
}

func sqliteWALHasFrames(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		// Only a missing WAL is a definitive no-op. Other stat failures fail
		// open so a real update is synced instead of silently dropped; at
		// worst that costs one redundant sync.
		return !errors.Is(err, fs.ErrNotExist)
	}
	if info == nil {
		return false
	}
	return info.Mode().IsRegular() && info.Size() > sqliteWALHeaderSize
}

func (s openCodeFormatSourceSet) sourceForRawID(root, sessionID string) (SourceRef, bool) {
	path := s.spec.find(root, sessionID)
	if path == "" {
		return SourceRef{}, false
	}
	return s.sourceRef(root, path, false)
}

func (s openCodeFormatSourceSet) sourceRef(
	root string,
	path string,
	promoteVirtual bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if dbPath, sessionID, ok := s.spec.parseVirtual(path); ok {
		if _, under := relUnder(root, dbPath); !under {
			return SourceRef{}, false
		}
		if promoteVirtual {
			if selected := s.spec.find(root, sessionID); selected != "" &&
				selected != path {
				return s.sourceRef(root, selected, false)
			}
		}
		if !OpenCodeSQLiteSessionExists(dbPath, sessionID) {
			return SourceRef{}, false
		}
		return s.newSourceRef(root, path, ""), true
	}
	if !s.isStorageSessionPath(root, path, true) {
		return SourceRef{}, false
	}
	return s.sourceRefFromStoragePath(root, path)
}

func (s openCodeFormatSourceSet) sourceRefFromStoragePath(
	root string,
	path string,
) (SourceRef, bool) {
	if !s.isStorageSessionPath(root, path, false) {
		return SourceRef{}, false
	}
	return s.newSourceRef(root, path, openCodeSessionProject(path)), true
}

func (s openCodeFormatSourceSet) newSourceRef(
	root string,
	path string,
	project string,
) SourceRef {
	return SourceRef{
		Provider:       s.spec.agent,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: openCodeFormatSource{
			Root: root,
			Path: path,
		},
	}
}

func (s openCodeFormatSourceSet) isStorageSessionPath(
	root string,
	path string,
	requireExisting bool,
) bool {
	rel, ok := relUnder(root, path)
	if !ok {
		return false
	}
	src := s.spec.resolve(root)
	if src.Mode != OpenCodeSourceStorage {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == filepath.Base(src.SessionRoot) &&
		strings.HasSuffix(parts[3], ".json") &&
		(!requireExisting || IsRegularFile(path))
}

func readOpenCodeProviderStorageSessionID(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var data struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	return data.SessionID
}

func findOpenCodeProviderStorageSessionIDByMessageID(
	openCodeDir, messageID string,
) string {
	messageRoot := filepath.Join(openCodeDir, "storage", "message")
	entries, err := os.ReadDir(messageRoot)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(messageRoot, entry.Name(), messageID+".json")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return entry.Name()
		}
	}
	return ""
}

func openCodeFormatProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:       CapabilitySupported,
			StreamingDiscovery:    CapabilitySupported,
			WatchSources:          CapabilitySupported,
			ClassifyChangedPath:   CapabilitySupported,
			ChangedPathRelevance:  CapabilitySupported,
			FindSource:            CapabilitySupported,
			CompositeFingerprint:  CapabilitySupported,
			IncrementalAppend:     CapabilityNotApplicable,
			MultiSessionSource:    CapabilityNotApplicable,
			SharedContainerSource: CapabilitySupported,
			PerSessionErrors:      CapabilityNotApplicable,
			ExcludedSessions:      CapabilityNotApplicable,
			ForceReplaceOnParse:   CapabilityNotApplicable,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			UnchangedResults: UnchangedResultMTimeAndHash,
			// The per-session digest is the only signal that sees a deleted
			// child, so freshness must consult it. Containers without
			// composite support produce an empty hash, which the gate treats
			// as no constraint, preserving their previous behavior.
			FingerprintHashRequiredForFreshness: true,
		},
	}
}
