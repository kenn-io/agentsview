package rawderive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

// ParsedManifest is the provider-owned normalized outcome for one raw source
// generation. Tombstones intentionally carry an empty outcome.
type ParsedManifest struct {
	Outcome   parser.ParseOutcome
	Tombstone bool
}

// ProviderParser dispatches materialized sources through registered provider
// implementations.
type ProviderParser struct {
	factories map[parser.AgentType]parser.ProviderFactory
	machine   string
}

// NewProviderParser constructs a provider dispatcher from an explicit factory
// registry.
func NewProviderParser(
	factories []parser.ProviderFactory,
	machine string,
) (*ProviderParser, error) {
	if strings.TrimSpace(machine) == "" {
		return nil, fmt.Errorf("%w: parser machine is required", rawsync.ErrInvalid)
	}
	registry := make(map[parser.AgentType]parser.ProviderFactory, len(factories))
	for _, factory := range factories {
		if factory == nil || factory.Definition().Type == "" {
			return nil, fmt.Errorf("%w: provider factory is invalid", rawsync.ErrInvalid)
		}
		providerType := factory.Definition().Type
		if _, exists := registry[providerType]; exists {
			return nil, fmt.Errorf("%w: duplicate provider factory %q", rawsync.ErrInvalid, providerType)
		}
		registry[providerType] = factory
	}
	return &ProviderParser{factories: registry, machine: machine}, nil
}

// Parse discovers the single provider source described by manifest, verifies
// its raw-capture membership, and invokes that provider's parser.
func (p *ProviderParser) Parse(
	ctx context.Context,
	manifest rawsync.CanonicalManifest,
	materialized *Materialization,
) (ParsedManifest, error) {
	if p == nil {
		return ParsedManifest{}, fmt.Errorf("%w: provider parser is missing", rawsync.ErrInvalid)
	}
	if err := rawsync.ValidateCanonicalManifest(manifest); err != nil {
		return ParsedManifest{}, err
	}
	if manifest.Manifest.Kind == rawsync.ManifestTombstone {
		return ParsedManifest{Tombstone: true}, nil
	}
	if materialized == nil || materialized.Root() == "" {
		return ParsedManifest{}, fmt.Errorf("%w: materialized source is required", rawsync.ErrInvalid)
	}
	// The materialized tree is untrusted client data: every cwd recorded in
	// transcripts and provider databases names the capturing host, never this
	// worker. Pin project attribution to transcript metadata and lexical path
	// rules for the whole hosted pipeline -- discovery, plan matching,
	// fingerprinting, and parsing -- so provider helpers cannot walk this
	// server's filesystem or read its .git metadata from an attacker-chosen
	// cwd.
	ctx = parser.WithoutFilesystemProjectDiscovery(ctx)
	factory, ok := p.factories[manifest.Manifest.Provider]
	if !ok {
		return ParsedManifest{}, fmt.Errorf(
			"%w: provider %q is not registered", rawsync.ErrInvalid, manifest.Manifest.Provider,
		)
	}
	// A manifest is provider-owned custody data, but the factory registry
	// is this worker's: only a factory that explicitly advertises raw-capture
	// support may construct a provider for hosted derivation. Rejecting
	// earlier keeps unvetted discovery code off the materialized tree.
	if factory.Capabilities().RawCapture.Support != parser.CapabilitySupported {
		return ParsedManifest{}, fmt.Errorf(
			"%w: provider %q does not support %s", rawsync.ErrInvalid,
			manifest.Manifest.Provider, parser.ProviderFeatureRawCapture,
		)
	}
	paths := newStablePathMap(manifest, materialized)
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:                 []string{materialized.Root()},
		Machine:               p.machine,
		StableSourceSnapshots: true,
		PathRewriter:          paths.rewrite,
	})
	if provider == nil {
		return ParsedManifest{}, fmt.Errorf("%w: provider construction failed", rawsync.ErrInvalid)
	}
	discovery, err := parser.DiscoverRawCaptureSources(ctx, provider)
	if err != nil {
		return ParsedManifest{}, redactMaterializedError("discovering provider source", err, materialized.Root())
	}
	if !discovery.Complete {
		return ParsedManifest{}, fmt.Errorf("provider raw-capture discovery is incomplete")
	}
	source, err := matchProviderSource(ctx, provider, discovery.Sources, manifest, materialized)
	if err != nil {
		return ParsedManifest{}, err
	}
	paths.bindSource(source, manifest.Manifest.SourceKey)
	sessions, fanOut, err := parser.ResolveRawSnapshotSessions(ctx, provider, source)
	if err != nil {
		return ParsedManifest{}, redactMaterializedError(
			"resolving raw snapshot sessions", err, materialized.Root(),
		)
	}
	if fanOut {
		return p.parseRawSnapshotSessions(ctx, provider, paths, materialized, sessions)
	}
	fingerprint, err := provider.Fingerprint(ctx, source)
	if err != nil {
		return ParsedManifest{}, redactMaterializedError("fingerprinting provider source", err, materialized.Root())
	}
	fingerprint.Key = paths.rewrite(fingerprint.Key)
	source.Key = manifest.Manifest.SourceKey
	source.DisplayPath = paths.rewrite(source.DisplayPath)
	source.FingerprintKey = paths.rewrite(source.FingerprintKey)
	if source.DisplayPath == "" {
		source.DisplayPath = manifest.Manifest.SourceKey
	}
	if source.FingerprintKey == "" {
		source.FingerprintKey = manifest.Manifest.SourceKey
	}
	if fingerprint.Key == "" {
		fingerprint.Key = source.FingerprintKey
	}
	outcome, err := provider.Parse(ctx, parser.ParseRequest{
		Source:             source,
		Fingerprint:        fingerprint,
		Machine:            p.machine,
		ForceParse:         true,
		StoredPathResolver: paths.resolve,
	})
	if err != nil {
		return ParsedManifest{}, redactMaterializedError("parsing provider source", err, materialized.Root())
	}
	rewriteParseOutcome(&outcome, paths, materialized.Root())
	return ParsedManifest{Outcome: outcome}, nil
}

func (p *ProviderParser) parseRawSnapshotSessions(
	ctx context.Context,
	provider parser.Provider,
	paths *stablePathMap,
	materialized *Materialization,
	sessions []parser.SourceRef,
) (ParsedManifest, error) {
	// One physical SQLite snapshot replaces the provider's whole logical
	// session set for this source generation.
	aggregate := parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}
	if len(sessions) == 0 {
		aggregate.SkipReason = parser.SkipNoSession
		return ParsedManifest{Outcome: aggregate}, nil
	}
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return ParsedManifest{}, err
		}
		fingerprint, err := provider.Fingerprint(ctx, session)
		if err != nil {
			return ParsedManifest{}, redactMaterializedError(
				"fingerprinting raw snapshot session", err, materialized.Root(),
			)
		}
		fingerprint.Key = paths.rewrite(fingerprint.Key)
		source := session
		source.Key = paths.rewrite(session.Key)
		source.DisplayPath = paths.rewrite(session.DisplayPath)
		source.FingerprintKey = paths.rewrite(session.FingerprintKey)
		if source.DisplayPath == "" {
			source.DisplayPath = source.Key
		}
		if source.FingerprintKey == "" {
			source.FingerprintKey = source.Key
		}
		if fingerprint.Key == "" {
			fingerprint.Key = source.FingerprintKey
		}
		outcome, err := provider.Parse(ctx, parser.ParseRequest{
			Source:             source,
			Fingerprint:        fingerprint,
			Machine:            p.machine,
			ForceParse:         true,
			StoredPathResolver: paths.resolve,
		})
		if err != nil {
			return ParsedManifest{}, redactMaterializedError(
				"parsing raw snapshot session", err, materialized.Root(),
			)
		}
		aggregate.Results = append(aggregate.Results, outcome.Results...)
		aggregate.ExcludedSessionIDs = append(aggregate.ExcludedSessionIDs, outcome.ExcludedSessionIDs...)
		aggregate.SourceErrors = append(aggregate.SourceErrors, outcome.SourceErrors...)
		aggregate.ResultSetComplete = aggregate.ResultSetComplete && outcome.ResultSetComplete
	}
	rewriteParseOutcome(&aggregate, paths, materialized.Root())
	return ParsedManifest{Outcome: aggregate}, nil
}

// sameMaterializedEntryFile reports whether a validated provider plan entry
// and a materialized manifest entry resolve to the same existing regular
// file. Plan validation canonicalizes spelling through symlink resolution,
// so entries can carry equivalent spellings of one file (symlinked components,
// Windows short names, and case-insensitive paths).
func sameMaterializedEntryFile(planPath, materializedPath string) bool {
	planned, err := os.Stat(planPath)
	if err != nil || !planned.Mode().IsRegular() {
		return false
	}
	materialized, err := os.Stat(materializedPath)
	if err != nil || !materialized.Mode().IsRegular() {
		return false
	}
	if os.SameFile(planned, materialized) {
		return true
	}
	// Filesystems that cannot report stable file identities fall back to
	// comparing resolved spellings.
	resolvedPlan, planErr := filepath.EvalSymlinks(planPath)
	resolvedMaterialized, materializedErr := filepath.EvalSymlinks(materializedPath)
	return planErr == nil && materializedErr == nil &&
		filepath.Clean(resolvedPlan) == filepath.Clean(resolvedMaterialized)
}

func matchProviderSource(
	ctx context.Context,
	provider parser.Provider,
	sources []parser.SourceRef,
	manifest rawsync.CanonicalManifest,
	materialized *Materialization,
) (parser.SourceRef, error) {
	wantPaths := make([]string, 0, len(manifest.Manifest.Entries))
	// The manifest's primary entry is the relative entry its source key
	// ends with: the one entry the matching plan marks appendable. Sibling
	// lineage inputs make several project transcripts cover the same entry
	// set, so the appendable entry is what selects the primary source.
	sourceEntry := manifestPrimaryEntry(manifest)
	for _, entry := range manifest.Manifest.Entries {
		wantPaths = append(wantPaths, entry.Path)
	}
	slices.Sort(wantPaths)
	var matches, primaryMatches []parser.SourceRef
	for _, source := range sources {
		plan, supported, err := parser.ResolveRawCapturePlan(ctx, provider, source)
		if err != nil {
			return parser.SourceRef{}, redactMaterializedError(
				"resolving provider raw-capture plan", err, materialized.Root(),
			)
		}
		if !supported || len(plan.Entries) != len(wantPaths) {
			continue
		}
		gotPaths := make([]string, 0, len(plan.Entries))
		valid := true
		appendableEntry := ""
		for _, entry := range plan.Entries {
			wantLocal, err := materialized.EntryPath(entry.Path)
			if err != nil || !sameMaterializedEntryFile(entry.LocalPath, wantLocal) {
				valid = false
				break
			}
			if entry.Appendable {
				appendableEntry = entry.Path
			}
			gotPaths = append(gotPaths, entry.Path)
		}
		slices.Sort(gotPaths)
		if valid && slices.Equal(gotPaths, wantPaths) {
			matches = append(matches, source)
			if sourceEntry != "" && appendableEntry == sourceEntry {
				primaryMatches = append(primaryMatches, source)
			}
		}
	}
	if len(primaryMatches) == 1 {
		return primaryMatches[0], nil
	}
	if len(primaryMatches) > 1 || len(matches) != 1 {
		return parser.SourceRef{}, fmt.Errorf(
			"%w: manifest matched %d provider sources", rawsync.ErrInvalid, len(matches),
		)
	}
	return matches[0], nil
}

type stablePathMap struct {
	root    string
	forward map[string]string
	reverse map[string]string
	prefix  string
}

func newStablePathMap(
	manifest rawsync.CanonicalManifest,
	materialized *Materialization,
) *stablePathMap {
	paths := &stablePathMap{
		root:    filepath.Clean(materialized.Root()),
		forward: make(map[string]string, len(manifest.Manifest.Entries)),
		reverse: make(map[string]string, len(manifest.Manifest.Entries)),
		prefix:  "raw-manifest:" + manifest.ManifestID + ":",
	}
	sourceEntry := manifestPrimaryEntry(manifest)
	for _, entry := range manifest.Manifest.Entries {
		local, err := materialized.EntryPath(entry.Path)
		if err != nil {
			continue
		}
		stable := paths.prefix + entry.Path
		if entry.Path == sourceEntry {
			stable = manifest.Manifest.SourceKey
		}
		paths.bind(local, stable)
	}
	paths.bindClientAliases(manifest, sourceEntry, materialized)
	return paths
}

func manifestPrimaryEntry(manifest rawsync.CanonicalManifest) string {
	primary := ""
	for _, entry := range manifest.Manifest.Entries {
		if sourceKeyEndsWithEntry(manifest.Manifest.SourceKey, entry.Path) &&
			len(entry.Path) > len(primary) {
			primary = entry.Path
		}
	}
	return primary
}

func (m *stablePathMap) bindSource(source parser.SourceRef, stable string) {
	m.bind(source.DisplayPath, stable)
	m.bind(source.FingerprintKey, stable)
}

// bindClientAliases registers safe lexical aliases so an absolute client
// path embedded in a transcript (the spelling the capturing host wrote, not
// any server-side location) resolves back to the materialized copy of the
// same validated manifest entry. The derivation is purely lexical: the
// manifest source key minus the primary relative entry yields the captured
// client root, and each entry rejoins that root with the client's separator
// style, including Windows-style keys parsed on a different host OS. Aliases
// are lookup keys only; they never rewrite worker-local paths and resolve
// solely to entries the materialization already validated.
func (m *stablePathMap) bindClientAliases(
	manifest rawsync.CanonicalManifest,
	sourceEntry string,
	materialized *Materialization,
) {
	if sourceEntry == "" {
		return
	}
	root, separator := clientSourceRoot(manifest.Manifest.SourceKey, sourceEntry)
	if root == "" {
		return
	}
	for _, entry := range manifest.Manifest.Entries {
		local, err := materialized.EntryPath(entry.Path)
		if err != nil {
			continue
		}
		stable := m.prefix + entry.Path
		if entry.Path == sourceEntry {
			stable = manifest.Manifest.SourceKey
		}
		clientPath := strings.ReplaceAll(entry.Path, "/", separator)
		for _, alias := range []string{
			root + separator + clientPath,
			root + "/" + entry.Path,
		} {
			if alias == stable {
				continue
			}
			if _, exists := m.reverse[alias]; !exists {
				m.reverse[alias] = local
			}
		}
	}
}

// clientSourceRoot removes the primary relative entry from the manifest
// source key and returns the remaining client root together with the
// separator style the capturing host used. The root is empty when no safe
// lexical derivation exists.
func clientSourceRoot(sourceKey, sourceEntry string) (string, string) {
	normalized := strings.ReplaceAll(sourceKey, "\\", "/")
	if normalized == sourceEntry {
		// A relative source key carries no client root to strip.
		return "", ""
	}
	suffix := "/" + sourceEntry
	if !strings.HasSuffix(normalized, suffix) {
		return "", ""
	}
	root := strings.TrimRight(sourceKey[:len(sourceKey)-len(suffix)], "/\\")
	if root == "" {
		return "", ""
	}
	separator := "/"
	if strings.Contains(sourceKey, "\\") {
		separator = "\\"
	}
	return root, separator
}

func (m *stablePathMap) bind(local, stable string) {
	if local == "" || stable == "" {
		return
	}
	local = filepath.Clean(local)
	if previous, ok := m.forward[local]; ok {
		delete(m.reverse, previous)
	}
	m.forward[local] = stable
	m.reverse[stable] = local
}

func (m *stablePathMap) rewrite(path string) string {
	if path == "" {
		return ""
	}
	if stable, ok := m.forward[filepath.Clean(path)]; ok {
		return stable
	}
	if base, suffix, ok := strings.Cut(path, "#"); ok {
		if stable, exists := m.forward[filepath.Clean(base)]; exists {
			return stable + "#" + suffix
		}
	}
	clean := filepath.Clean(path)
	relative, err := filepath.Rel(m.root, clean)
	if err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
		return m.prefix + filepath.ToSlash(relative)
	}
	return path
}

func (m *stablePathMap) resolve(stable string) (string, bool) {
	local, ok := m.reverse[stable]
	if ok {
		return local, true
	}
	if base, _, hasSuffix := strings.Cut(stable, "#"); hasSuffix {
		local, ok = m.reverse[base]
	}
	return local, ok
}

func sourceKeyEndsWithEntry(sourceKey, entry string) bool {
	key := strings.ReplaceAll(sourceKey, "\\", "/")
	return key == entry || strings.HasSuffix(key, "/"+entry)
}

func rewriteParseOutcome(outcome *parser.ParseOutcome, paths *stablePathMap, root string) {
	for index := range outcome.Results {
		file := &outcome.Results[index].Result.Session.File
		file.Path = paths.rewrite(file.Path)
		// The materialization is worker-local: inode and device identities
		// cannot be reconstructed from the manifest, so strip values that
		// would otherwise differ on every reparse.
		file.Inode = 0
		file.Device = 0
		if strings.Contains(file.Path, root) {
			file.Path = paths.prefix + "source"
		}
	}
	for index := range outcome.SourceErrors {
		sourceErr := &outcome.SourceErrors[index]
		sourceErr.SourceKey = paths.rewrite(sourceErr.SourceKey)
		sourceErr.DisplayPath = paths.rewrite(sourceErr.DisplayPath)
		if sourceErr.Err != nil && strings.Contains(sourceErr.Err.Error(), root) {
			sourceErr.Err = redactedProviderError{
				message: strings.ReplaceAll(sourceErr.Err.Error(), root, "<materialized>"),
				cause:   sourceErr.Err,
			}
		}
	}
}

func redactMaterializedError(operation string, err error, root string) error {
	return redactedProviderError{
		operation: operation,
		message:   strings.ReplaceAll(err.Error(), root, "<materialized>"),
		cause:     err,
	}
}

type redactedProviderError struct {
	operation string
	message   string
	cause     error
}

func (e redactedProviderError) Error() string {
	if e.operation == "" {
		return e.message
	}
	return e.operation + ": " + e.message
}

func (e redactedProviderError) Unwrap() error {
	return e.cause
}
