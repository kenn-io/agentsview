package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	_ Provider = (*openClawProvider)(nil)
	_ Provider = (*qClawProvider)(nil)
)

type clawProviderSpec struct {
	agent       AgentType
	sessionFile func(string) bool
	sessionID   func(string) string
}

type openClawProviderFactory struct {
	def AgentDef
}

func newOpenClawProviderFactory(def AgentDef) ProviderFactory {
	return openClawProviderFactory{def: cloneAgentDef(def)}
}

func (f openClawProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f openClawProviderFactory) Capabilities() Capabilities {
	return openClawProviderCapabilities()
}

func (f openClawProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &openClawProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    openClawProviderCapabilities(),
		Config:  cfg,
		sources: newOpenClawSourceSet(cfg.Roots),
	}
}

type openClawProvider struct {
	ProviderBase
	sources openClawSourceSet
}

func (p *openClawProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *openClawProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *openClawProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *openClawProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *openClawProvider) StoredSourceHintScopes(
	req ChangedPathRequest,
) []StoredSourceHintScope {
	return p.sources.StoredSourceHintScopes(req)
}

func (p *openClawProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *openClawProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *openClawProvider) ReconciliationSourceRank(
	source SourceRef,
) ReconciliationSourceRank {
	return p.sources.ReconciliationSourceRank(source)
}

func (p *openClawProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	return p.sources.SourceForReconciliation(ctx, path, project)
}

func (p *openClawProvider) PersistentArchiveSource(
	path, fullSessionID string,
) (string, bool) {
	return p.sources.PersistentArchiveSource(path, fullSessionID)
}

func (p *openClawProvider) ReconciliationMemberIdentity(
	fullSessionID string,
) string {
	return p.sources.ReconciliationMemberIdentity(fullSessionID)
}

func (p *openClawProvider) ResolveReconciliationScopes(
	ctx context.Context, req ReconciliationScopeRequest,
) (ReconciliationScopePlan, error) {
	if err := ValidateReconciliationScopeRoots(
		p.Def.Type, p.Config.Roots, req.Roots,
	); err != nil {
		return ReconciliationScopePlan{}, err
	}
	return containerAwareReconciliationScopePlan(
		p.Config.Roots, req.Roots, p.sources.ReconciliationContainer,
	), nil
}

func (p *openClawProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	req.Machine = machine
	return p.sources.Parse(ctx, req)
}

type openClawSourceSet struct {
	legacy clawSourceSet
	sqlite multiSessionContainerSourceSet
}

func newOpenClawSourceSet(roots []string) openClawSourceSet {
	return openClawSourceSet{
		legacy: newClawSourceSet(roots, openClawProviderSpec()),
		sqlite: newOpenClawSQLiteSourceSet(roots),
	}
}

func (s openClawSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	return collectDiscoveredSources(ctx, s.DiscoverEach)
}

func (s openClawSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	sqliteIDs := make(map[string]struct{})
	var incomplete error
	for _, root := range s.sqlite.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		var yieldErr error
		err := s.sqlite.cfg.discoverEach(ctx, root, func(
			match multiSessionMatch,
		) error {
			source := s.sqlite.sourceRef(root, match)
			if logicalID, ok := openClawLogicalSourceID(source); ok {
				if _, exists := sqliteIDs[logicalID]; exists {
					return nil
				}
				sqliteIDs[logicalID] = struct{}{}
			}
			yieldErr = yield(source)
			return yieldErr
		})
		if yieldErr != nil {
			return yieldErr
		}
		if err == nil {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(
			err, context.DeadlineExceeded,
		) {
			return err
		}
		if _, ok := errors.AsType[DiscoveryIncompleteError](err); ok {
			incomplete = errors.Join(incomplete, err)
			continue
		}
		return err
	}

	legacy := make(map[string]SourceRef)
	legacyErr := s.legacy.DiscoverEach(ctx, func(source SourceRef) error {
		logicalID, ok := openClawLogicalSourceID(source)
		if !ok {
			return nil
		}
		if _, shadowed := sqliteIDs[logicalID]; shadowed {
			return nil
		}
		previous, exists := legacy[logicalID]
		if !exists || openClawLegacySourcePreferred(source, previous) {
			legacy[logicalID] = source
		}
		return nil
	})
	if legacyErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(legacyErr, context.Canceled) || errors.Is(
			legacyErr, context.DeadlineExceeded,
		) {
			return legacyErr
		}
		if _, ok := errors.AsType[DiscoveryIncompleteError](legacyErr); ok {
			incomplete = errors.Join(incomplete, legacyErr)
		} else {
			return legacyErr
		}
	}
	legacySources := make([]SourceRef, 0, len(legacy))
	for _, source := range legacy {
		legacySources = append(legacySources, source)
	}
	sortJSONLSources(legacySources)
	for _, source := range legacySources {
		if err := yield(source); err != nil {
			return err
		}
	}
	return incomplete
}

func openClawLogicalSourceID(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case clawSource:
		return source.Key, source.Key != ""
	case *clawSource:
		return source.Key, src != nil && source.Key != ""
	case multiSessionSource:
		return src.MemberID, src.MemberID != ""
	case *multiSessionSource:
		return src.MemberID, src != nil && src.MemberID != ""
	default:
		return "", false
	}
}

func openClawLegacySourcePreferred(a, b SourceRef) bool {
	aRank := clawSourceRank(a)
	bRank := clawSourceRank(b)
	if aRank.Class != bRank.Class {
		return aRank.Class > bRank.Class
	}
	if aRank.Recency != bRank.Recency {
		return aRank.Recency > bRank.Recency
	}
	return a.DisplayPath < b.DisplayPath
}

func clawSourceRank(source SourceRef) ReconciliationSourceRank {
	path := source.DisplayPath
	if src, ok := source.Opaque.(clawSource); ok {
		path = src.Path
	}
	return clawReconciliationSourceRank(filepath.Base(path), source.DiscoveryMTimeNS)
}

func (s openClawSourceSet) WatchPlan(
	ctx context.Context,
) (WatchPlan, error) {
	legacy, err := s.legacy.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	sqlite, err := s.sqlite.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	merged := make(map[string]WatchRoot, len(legacy.Roots)+len(sqlite.Roots))
	for _, root := range append(legacy.Roots, sqlite.Roots...) {
		key := filepath.Clean(root.Path) + "\x00" + strconv.FormatBool(root.Recursive)
		previous, exists := merged[key]
		if !exists {
			merged[key] = root
			continue
		}
		previous.IncludeGlobs = appendUniqueStrings(
			previous.IncludeGlobs, root.IncludeGlobs,
		)
		previous.ExcludeGlobs = appendUniqueStrings(
			previous.ExcludeGlobs, root.ExcludeGlobs,
		)
		merged[key] = previous
	}
	paths := make([]string, 0, len(merged))
	for key := range merged {
		paths = append(paths, key)
	}
	sort.Strings(paths)
	plan := WatchPlan{Roots: make([]WatchRoot, 0, len(paths))}
	for _, key := range paths {
		plan.Roots = append(plan.Roots, merged[key])
	}
	return plan, nil
}

func appendUniqueStrings(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, value := range base {
		seen[value] = struct{}{}
	}
	for _, value := range extra {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		base = append(base, value)
	}
	return base
}

func (s openClawSourceSet) SourcesForChangedPath(
	ctx context.Context, req ChangedPathRequest,
) ([]SourceRef, error) {
	for _, root := range s.sqlite.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, root) {
			continue
		}
		if _, ok := s.sqlite.cfg.classifyPath(root, req.Path, true); ok {
			sources, err := s.sqlite.SourcesForChangedPath(ctx, req)
			if err != nil {
				return nil, err
			}
			return s.canonicalizeSQLiteChangedSources(ctx, sources)
		}
	}
	legacy, err := s.legacy.SourcesForChangedPath(ctx, req)
	if err != nil {
		return nil, err
	}
	return s.filterLegacySQLiteDuplicates(ctx, legacy)
}

func (s openClawSourceSet) canonicalizeSQLiteChangedSources(
	ctx context.Context, sources []SourceRef,
) ([]SourceRef, error) {
	if len(sources) == 0 {
		return sources, nil
	}
	out := make([]SourceRef, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	add := func(source SourceRef) {
		if logicalID, ok := openClawLogicalSourceID(source); ok {
			if _, exists := seen[logicalID]; exists {
				return
			}
			seen[logicalID] = struct{}{}
		}
		out = append(out, source)
	}
	for _, source := range sources {
		src, ok := s.sqlite.sourceFromRef(source)
		if !ok || src.MemberID == "" {
			if !ok {
				add(source)
				continue
			}
			expanded, err := s.expandSQLiteChangedContainer(
				ctx, source, openClawSQLiteSessionIDsEach,
			)
			if err != nil {
				return nil, err
			}
			for _, expandedSource := range expanded {
				add(expandedSource)
			}
			continue
		}
		canonical, found, err := s.findSQLiteMember(ctx, src.MemberID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}
		if found {
			add(canonical)
			continue
		}
		add(source)
	}
	return out, nil
}

func (s openClawSourceSet) expandSQLiteChangedContainer(
	ctx context.Context,
	container SourceRef,
	enumerate openClawSQLiteSessionEnumerator,
) ([]SourceRef, error) {
	src, ok := s.sqlite.sourceFromRef(container)
	if !ok || src.Container == "" {
		return []SourceRef{container}, nil
	}
	agentID, ok := openClawSQLiteAgentForDB(src.Root, src.Container)
	if !ok {
		return []SourceRef{container}, nil
	}
	sources := make([]SourceRef, 0)
	var callbackErr error
	emitted := 0
	err := enumerate(ctx, src.Container, func(
		sessionID string, _ int64,
	) error {
		memberID := agentID + ":" + sessionID
		winner, found, err := s.findSQLiteMember(ctx, memberID)
		if err != nil {
			callbackErr = err
			return err
		}
		if !found {
			return nil
		}
		winner.ProjectHint = agentID
		sources = append(sources, winner)
		emitted++
		return nil
	})
	if callbackErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sources, ctxErr
		}
		return sources, callbackErr
	}
	if err == nil {
		return sources, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return sources, ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(
		err, context.DeadlineExceeded,
	) {
		return sources, err
	}
	if emitted == 0 {
		return []SourceRef{container}, nil
	}
	return sources, incompleteDiscoveryError(
		AgentOpenClaw,
		"enumerate OpenClaw SQLite changed container "+src.Container,
		err,
	)
}

func (s openClawSourceSet) filterLegacySQLiteDuplicates(
	ctx context.Context, sources []SourceRef,
) ([]SourceRef, error) {
	if len(sources) == 0 {
		return sources, nil
	}
	filtered := make([]SourceRef, 0, len(sources))
	for _, source := range sources {
		rawID, ok := openClawLogicalSourceID(source)
		if !ok {
			filtered = append(filtered, source)
			continue
		}
		_, found, err := s.findSQLiteMember(ctx, rawID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			filtered = append(filtered, source)
			continue
		}
		if !found {
			filtered = append(filtered, source)
		}
	}
	return filtered, nil
}

func (s openClawSourceSet) StoredSourceHintScopes(
	req ChangedPathRequest,
) []StoredSourceHintScope {
	for _, root := range s.sqlite.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, root) {
			continue
		}
		if _, ok := s.sqlite.cfg.classifyPath(root, req.Path, true); ok {
			return s.sqlite.StoredSourceHintScopes(req)
		}
	}
	return nil
}

func (s openClawSourceSet) FindSource(
	ctx context.Context, req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	var preferredLegacy SourceRef
	preferredLegacyFound := false
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		var memberID string
		for _, root := range s.sqlite.roots {
			match, ok := s.sqlite.cfg.classifyPath(root, path, true)
			if !ok || match.MemberID == "" {
				continue
			}
			if req.RawSessionID != "" && match.MemberID != req.RawSessionID {
				continue
			}
			memberID = match.MemberID
			break
		}
		if memberID != "" {
			source, found, err := s.findSQLiteMember(ctx, memberID)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return SourceRef{}, false, ctxErr
				}
			} else if found {
				return source, true, nil
			}
		}
		if req.PreferStoredSource {
			legacy, found := s.legacy.sourceForExactStoredPath(path)
			if found {
				rawID, valid := openClawLogicalSourceID(legacy)
				if valid && (req.RawSessionID == "" || rawID == req.RawSessionID) {
					if source, found, err := s.findSQLiteMember(ctx, rawID); err != nil {
						if ctxErr := ctx.Err(); ctxErr != nil {
							return SourceRef{}, false, ctxErr
						}
					} else if found {
						return source, true, nil
					}
					preferredLegacy = legacy
					preferredLegacyFound = true
				}
			}
		}
	}
	if req.RawSessionID != "" {
		if source, found, err := s.findSQLiteMember(ctx, req.RawSessionID); err != nil {
			if ctx.Err() != nil {
				return SourceRef{}, false, err
			}
		} else if found {
			return source, true, nil
		}
	}
	if preferredLegacyFound {
		return preferredLegacy, true, nil
	}
	legacy, found, err := s.legacy.FindSource(ctx, req)
	if err != nil || !found {
		return legacy, found, err
	}
	rawID, ok := openClawLogicalSourceID(legacy)
	if !ok {
		return legacy, true, nil
	}
	if sqlite, found, err := s.findSQLiteMember(ctx, rawID); err != nil {
		if ctx.Err() != nil {
			return SourceRef{}, false, err
		}
	} else if found {
		return sqlite, true, nil
	}
	return legacy, true, nil
}

func (s openClawSourceSet) findSQLiteMember(
	ctx context.Context, rawID string,
) (SourceRef, bool, error) {
	var firstErr error
	for _, root := range s.sqlite.roots {
		if err := ctx.Err(); err != nil {
			return SourceRef{}, false, err
		}
		match, found, err := openClawSQLiteFindMemberChecked(
			ctx, root, rawID,
		)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return SourceRef{}, false, ctxErr
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if found {
			return s.sqlite.sourceRef(root, match), true, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	return SourceRef{}, false, firstErr
}

func (s openClawSourceSet) Fingerprint(
	ctx context.Context, source SourceRef,
) (SourceFingerprint, error) {
	if s.isSQLiteSource(source) {
		return s.sqlite.Fingerprint(ctx, source)
	}
	return s.legacy.Fingerprint(ctx, source)
}

func (s openClawSourceSet) Parse(
	ctx context.Context, req ParseRequest,
) (ParseOutcome, error) {
	if s.isSQLiteSource(req.Source) {
		return s.sqlite.Parse(ctx, req)
	}
	path, ok := s.legacy.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("%s source path unavailable", AgentOpenClaw)
	}
	sess, msgs, err := parseOpenClawSessionWithMachine(
		path, "", req.Machine,
	)
	outcome, err := clawParseOutcome(req, sess, msgs, err)
	if err == nil && len(outcome.Results) > 0 {
		outcome.ForceReplace = true
	}
	return outcome, err
}

func parseOpenClawSessionWithMachine(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	provider := &openClawProvider{}
	return provider.parseSession(path, project, machine)
}

func openClawSQLiteSource(source SourceRef) bool {
	switch source.Opaque.(type) {
	case multiSessionSource, *multiSessionSource:
		return true
	default:
		return false
	}
}

func (s openClawSourceSet) isSQLiteSource(source SourceRef) bool {
	if openClawSQLiteSource(source) {
		return true
	}
	for _, candidate := range []string{
		source.DisplayPath, source.FingerprintKey, source.Key,
	} {
		if candidate == "" {
			continue
		}
		for _, root := range s.sqlite.roots {
			if _, ok := s.sqlite.cfg.classifyPath(root, candidate, true); ok {
				return true
			}
		}
	}
	return false
}

func (s openClawSourceSet) ReconciliationSourceRank(
	source SourceRef,
) ReconciliationSourceRank {
	if s.isSQLiteSource(source) {
		return ReconciliationSourceRank{
			Class:   3,
			Recency: source.DiscoveryMTimeNS,
		}
	}
	return s.legacy.reconciliationSourceRank(source)
}

func (s openClawSourceSet) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, root := range s.sqlite.roots {
		if err := ctx.Err(); err != nil {
			return SourceRef{}, false, err
		}
		match, ok := s.sqlite.cfg.classifyPath(root, path, true)
		if !ok || match.MemberID == "" {
			continue
		}
		winner, found, err := s.findSQLiteMember(ctx, match.MemberID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return SourceRef{}, false, ctxErr
			}
			return SourceRef{}, false, err
		}
		if found {
			if project != "" {
				winner.ProjectHint = project
			}
			if s.sqlite.cfg.reconciliationIdentity != nil {
				identity, err := s.sqlite.cfg.reconciliationIdentity(ctx, match)
				if err != nil {
					return SourceRef{}, false, err
				}
				winner.ReconciliationIdentity = identity
			}
			return winner, true, nil
		}
		if project != "" {
			match.ProjectHint = project
		}
		if s.sqlite.cfg.reconciliationIdentity != nil {
			identity, err := s.sqlite.cfg.reconciliationIdentity(ctx, match)
			if err != nil {
				return SourceRef{}, false, err
			}
			match.ReconciliationIdentity = identity
		}
		return s.sqlite.sourceRef(root, match), true, nil
	}
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, root := range s.legacy.roots {
		if source, ok := s.legacy.sourceForPathInRoot(root, path); ok {
			if project != "" {
				source.ProjectHint = project
			}
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s openClawSourceSet) PersistentArchiveSource(
	path, fullSessionID string,
) (string, bool) {
	return s.sqlite.PersistentArchiveSource(path, fullSessionID)
}

func (s openClawSourceSet) ReconciliationMemberIdentity(
	fullSessionID string,
) string {
	return ProviderRawSessionIDFromFull(
		AgentDef{IDPrefix: "openclaw:"}, fullSessionID,
	)
}

func (s openClawSourceSet) ReconciliationContainer(
	requested string,
) (string, bool) {
	return s.sqlite.ReconciliationContainer(requested)
}

type qClawProviderFactory struct {
	def AgentDef
}

func newQClawProviderFactory(def AgentDef) ProviderFactory {
	return qClawProviderFactory{def: cloneAgentDef(def)}
}

func (f qClawProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f qClawProviderFactory) Capabilities() Capabilities {
	return qClawProviderCapabilities()
}

func (f qClawProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &qClawProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    qClawProviderCapabilities(),
		Config:  cfg,
		sources: newClawSourceSet(cfg.Roots, qClawProviderSpec()),
	}
}

type qClawProvider struct {
	ProviderBase
	sources clawSourceSet
}

func (p *qClawProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *qClawProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *qClawProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *qClawProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *qClawProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *qClawProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *qClawProvider) ReconciliationSourceRank(
	source SourceRef,
) ReconciliationSourceRank {
	return p.sources.reconciliationSourceRank(source)
}

func (p *qClawProvider) Parse(
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
	sess, msgs, err := p.parseSession(path, "", machine)
	return clawParseOutcome(req, sess, msgs, err)
}

type clawSource struct {
	Root string
	Path string
}

type clawSourceSet struct {
	roots []string
	spec  clawProviderSpec
}

func newClawSourceSet(roots []string, spec clawProviderSpec) clawSourceSet {
	return clawSourceSet{
		roots: cleanJSONLRoots(roots),
		spec:  spec,
	}
}

func (s clawSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, source := range s.discoverRoot(root) {
			key := string(source.Provider) + "\x00" + source.Key
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			sources = append(sources, source)
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].DisplayPath != sources[j].DisplayPath {
			return sources[i].DisplayPath < sources[j].DisplayPath
		}
		return sources[i].Key < sources[j].Key
	})
	return sources, nil
}

func (s clawSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := streamDirectoryEntries(ctx, root, func(agent os.DirEntry) error {
			if !IsValidSessionID(agent.Name()) {
				return nil
			}
			isAgentDir, dirErr := streamingDirCandidateOrIncomplete(
				s.spec.agent, "agent directory", agent, root,
			)
			if dirErr != nil {
				return dirErr
			}
			if !isAgentDir {
				return nil
			}
			dir := filepath.Join(root, agent.Name(), "sessions")
			return streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
				if entry.IsDir() || !s.spec.sessionFile(entry.Name()) ||
					!IsValidSessionID(s.spec.sessionID(entry.Name())) {
					return nil
				}
				source, ok := s.sourceRef(root, filepath.Join(dir, entry.Name()))
				if !ok {
					return nil
				}
				if info, infoErr := entry.Info(); infoErr == nil {
					source.DiscoveryMTimeNS = info.ModTime().UnixNano()
				}
				return yield(source)
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s clawSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl", "*.jsonl.*"},
			DebounceKey:  string(s.spec.agent) + ":claw:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s clawSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.WatchRoot != "" {
		for _, root := range s.roots {
			if !samePath(root, req.WatchRoot) {
				continue
			}
			source, ok := s.sourceForChangedPathInRoot(root, req)
			if !ok {
				return nil, nil
			}
			return []SourceRef{source}, nil
		}
		return nil, nil
	}
	source, ok := s.sourceForChangedPath(req)
	if !ok {
		return nil, nil
	}
	return []SourceRef{source}, nil
}

func (s clawSourceSet) FindSource(
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
		if source, ok := s.sourceForStoredPath(path); ok {
			return source, true, nil
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.sourcePathForRawID(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s clawSourceSet) Fingerprint(
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
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	// Legacy processOpenClaw/processQClaw persisted a full-file content hash
	// (file_hash). Without it here the parse outcome leaves Session.File.Hash
	// empty and a resync clears the stored hash to NULL.
	hash, err := hashJSONLSourceFile(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Hash:    hash,
	}, nil
}

func (s clawSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case clawSource:
		return src.Path, src.Path != ""
	case *clawSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		if ref, ok := s.sourceForPath(candidate); ok {
			src := ref.Opaque.(clawSource)
			return src.Path, true
		}
	}
	return "", false
}

func (s clawSourceSet) sourceForPath(path string) (SourceRef, bool) {
	for _, root := range s.roots {
		if source, ok := s.sourceForPathInRoot(root, path); ok {
			return source, true
		}
	}
	return SourceRef{}, false
}

func (s clawSourceSet) sourceForChangedPath(req ChangedPathRequest) (SourceRef, bool) {
	for _, root := range s.roots {
		if source, ok := s.sourceForChangedPathInRoot(root, req); ok {
			return source, true
		}
	}
	return SourceRef{}, false
}

func (s clawSourceSet) sourceForChangedPathInRoot(
	root string,
	req ChangedPathRequest,
) (SourceRef, bool) {
	if source, ok := s.sourceForPathInRoot(root, req.Path); ok {
		return source, true
	}
	if !jsonlMissingPathFallbackAllowed(req) {
		return SourceRef{}, false
	}
	return s.sourceForStoredPathInRoot(root, req.Path)
}

func (s clawSourceSet) sourceForStoredPath(path string) (SourceRef, bool) {
	for _, root := range s.roots {
		if source, ok := s.sourceForStoredPathInRoot(root, path); ok {
			return source, true
		}
	}
	return SourceRef{}, false
}

func (s clawSourceSet) sourceForExactStoredPath(path string) (SourceRef, bool) {
	path = filepath.Clean(path)
	if !IsRegularFile(path) {
		return SourceRef{}, false
	}
	for _, root := range s.roots {
		if _, ok := s.rawIDFromPath(root, path); !ok {
			continue
		}
		return s.sourceRef(root, path)
	}
	return SourceRef{}, false
}

func (s clawSourceSet) sourceForStoredPathInRoot(
	root string,
	path string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rawID, ok := s.rawIDFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	best := s.sourcePathForRawID(root, rawID)
	if best == "" {
		return SourceRef{}, false
	}
	return s.sourceRef(root, best)
}

func (s clawSourceSet) sourceForPathInRoot(root string, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rawID, ok := s.rawIDFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	best := s.sourcePathForRawID(root, rawID)
	if best == "" || !samePath(best, path) {
		return SourceRef{}, false
	}
	return s.sourceRef(root, best)
}

func (s clawSourceSet) sourceRef(root string, path string) (SourceRef, bool) {
	rawID, ok := s.rawIDFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	return SourceRef{
		Provider:       s.spec.agent,
		Key:            rawID,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    clawAgentIDFromRawID(rawID),
		Opaque: clawSource{
			Root: filepath.Clean(root),
			Path: filepath.Clean(path),
		},
	}, true
}

func (s clawSourceSet) rawIDFromPath(root string, path string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[1] != "sessions" {
		return "", false
	}
	if !IsValidSessionID(parts[0]) || !s.spec.sessionFile(parts[2]) {
		return "", false
	}
	sessionID := s.spec.sessionID(parts[2])
	if !IsValidSessionID(sessionID) {
		return "", false
	}
	return parts[0] + ":" + sessionID, true
}

func (s clawSourceSet) discoverRoot(root string) []SourceRef {
	if root == "" {
		return nil
	}

	agentEntries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var sources []SourceRef
	for _, agentEntry := range agentEntries {
		if !isDirOrSymlink(agentEntry, root) {
			continue
		}
		if !IsValidSessionID(agentEntry.Name()) {
			continue
		}

		sessionsDir := filepath.Join(root, agentEntry.Name(), "sessions")
		entries, err := os.ReadDir(sessionsDir)
		if err != nil {
			continue
		}

		best := make(map[string]os.DirEntry)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !s.spec.sessionFile(name) {
				continue
			}
			sessionID := s.spec.sessionID(name)
			if !IsValidSessionID(sessionID) {
				continue
			}
			prev, exists := best[sessionID]
			if !exists {
				best[sessionID] = entry
				continue
			}
			best[sessionID] = s.bestEntry(prev, entry)
		}

		for _, entry := range best {
			source, ok := s.sourceRef(root, filepath.Join(sessionsDir, entry.Name()))
			if ok {
				sources = append(sources, source)
			}
		}
	}
	return sources
}

func (s clawSourceSet) sourcePathForRawID(root, rawID string) string {
	if root == "" {
		return ""
	}
	agentID, sessionID, ok := strings.Cut(rawID, ":")
	if !ok || !IsValidSessionID(agentID) || !IsValidSessionID(sessionID) {
		return ""
	}

	sessionsDir := filepath.Join(root, agentID, "sessions")
	active := filepath.Join(sessionsDir, sessionID+".jsonl")
	if _, err := os.Stat(active); err == nil {
		return active
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return ""
	}

	var best os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !s.spec.sessionFile(name) {
			continue
		}
		if s.spec.sessionID(name) != sessionID {
			continue
		}
		if best == nil {
			best = entry
			continue
		}
		best = s.bestEntry(best, entry)
	}
	if best == nil {
		return ""
	}
	return filepath.Join(sessionsDir, best.Name())
}

func (s clawSourceSet) bestEntry(a, b os.DirEntry) os.DirEntry {
	aRank := clawDirEntryReconciliationRank(a)
	bRank := clawDirEntryReconciliationRank(b)
	if bRank.Class > aRank.Class ||
		(bRank.Class == aRank.Class && bRank.Recency > aRank.Recency) {
		return b
	}
	return a
}

func (s clawSourceSet) reconciliationSourceRank(
	source SourceRef,
) ReconciliationSourceRank {
	path, ok := s.pathFromSource(source)
	if !ok {
		return ReconciliationSourceRank{}
	}
	return clawReconciliationSourceRank(
		filepath.Base(path), source.DiscoveryMTimeNS,
	)
}

func clawDirEntryReconciliationRank(entry os.DirEntry) ReconciliationSourceRank {
	mtime := int64(0)
	if info, err := entry.Info(); err == nil {
		mtime = info.ModTime().UnixNano()
	}
	return clawReconciliationSourceRank(entry.Name(), mtime)
}

func clawReconciliationSourceRank(name string, mtime int64) ReconciliationSourceRank {
	if strings.HasSuffix(name, ".jsonl") {
		return ReconciliationSourceRank{Class: 2, Recency: mtime}
	}
	archiveTime := clawArchiveNameTime(name)
	if !archiveTime.IsZero() {
		return ReconciliationSourceRank{Class: 1, Recency: archiveTime.UnixNano()}
	}
	return ReconciliationSourceRank{Recency: mtime}
}

func clawArchiveNameTime(name string) time.Time {
	idx := strings.Index(name, ".jsonl.")
	if idx <= 0 {
		return time.Time{}
	}
	suffix := name[idx+len(".jsonl."):]
	_, tsStr, ok := strings.Cut(suffix, ".")
	if !ok {
		return time.Time{}
	}
	if tIdx := strings.IndexByte(tsStr, 'T'); tIdx >= 0 {
		datePart := tsStr[:tIdx+1]
		timePart := tsStr[tIdx+1:]
		timePart = strings.Replace(timePart, "-", ":", 1)
		timePart = strings.Replace(timePart, "-", ":", 1)
		tsStr = datePart + timePart
	}
	t, err := time.Parse("2006-01-02T15:04:05.000Z", tsStr)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", tsStr)
	}
	if err != nil {
		return time.Time{}
	}
	return t
}

func clawParseOutcome(
	req ParseRequest,
	sess *ParsedSession,
	msgs []ParsedMessage,
	err error,
) (ParseOutcome, error) {
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if req.Fingerprint.Hash != "" {
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

func openClawProviderSpec() clawProviderSpec {
	return clawProviderSpec{
		agent:       AgentOpenClaw,
		sessionFile: IsOpenClawSessionFile,
		sessionID:   OpenClawSessionID,
	}
}

func qClawProviderSpec() clawProviderSpec {
	return clawProviderSpec{
		agent:       AgentQClaw,
		sessionFile: IsQClawSessionFile,
		sessionID:   QClawSessionID,
	}
}

func clawAgentIDFromRawID(rawID string) string {
	agentID, _, ok := strings.Cut(rawID, ":")
	if !ok {
		return ""
	}
	return agentID
}

func openClawProviderCapabilities() Capabilities {
	source := clawProviderCapabilities().Source
	source.StreamingDiscovery = CapabilitySupported
	source.MultiSessionSource = CapabilitySupported
	source.SharedContainerSource = CapabilitySupported
	source.PerSessionErrors = CapabilitySupported
	source.ForceReplaceOnParse = CapabilitySupported
	source.PersistentArchive = CapabilitySupported
	source.StoredSourceHints = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
	}
}

func qClawProviderCapabilities() Capabilities {
	return clawProviderCapabilities()
}

func clawProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.StreamingDiscovery = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
