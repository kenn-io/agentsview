package sync

import (
	"context"
	"os"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// sourceCollisionID returns the raw session id to store s under. A stored
// session keeps its id unless its file is gone and an unclaimed replacement
// has at least as many messages. Other files with the same id are stored under
// parser.AltSessionID as continuations, unless the provider recognizes a move.
// A session the write step will filter out (admitted false) keeps any id its
// file already holds but never claims or mints one.
func (e *Engine) sourceCollisionID(
	ctx context.Context,
	provider parser.Provider,
	lookupPath string,
	s *parser.ParsedSession,
	admitted bool,
) (string, error) {
	if !collisionPolicyApplies(provider) {
		return s.ID, nil
	}
	fullID := applyIDPrefixToID(e.idPrefix, s.ID)
	records, err := e.sessionPathRecords(ctx, fullID)
	if err != nil {
		return "", err
	}
	var stored, deleted string
	var hasStored, deletedAnyFile bool
	var owner db.SessionPathRecord
	for _, r := range records {
		switch {
		case r.ID != fullID:
		case r.Excluded:
			deleted, deletedAnyFile = r.FilePath, r.FilePath == ""
		default:
			stored, hasStored = r.FilePath, true
			owner = r
		}
	}
	// A permanently deleted id stays with the file it was deleted for; a
	// deletion recorded without its file covers every file with the id.
	if deletedAnyFile || e.storedSourceLivesAt(ctx, provider, stored, lookupPath) ||
		e.storedSourceLivesAt(ctx, provider, deleted, lookupPath) {
		return s.ID, nil
	}
	altID := e.existingAltID(ctx, provider, records, fullID, s.ID, lookupPath)
	if altID == "" && !admitted {
		return s.ID, nil
	}
	if altID == "" {
		available := !hasStored
		if hasStored && !owner.Trashed && stored != "" &&
			s.MessageCount >= owner.MessageCount {
			available = e.storedSourceGone(ctx, provider, stored)
		}
		if available && deleted == "" && e.claimSessionID(ctx, provider, fullID, lookupPath) {
			return s.ID, nil
		}
		altID = parser.AltSessionID(s.ID, lookupPath)
	}
	s.ParentSessionID = s.ID
	s.RelationshipType = parser.RelContinuation
	s.ID = altID
	return altID, nil
}

// storedSourceGone requires absence on disk and at the provider. Remote paths
// must resolve inside a complete mirror; absence from a partial import is not
// evidence that the remote file is gone.
func (e *Engine) storedSourceGone(ctx context.Context, provider parser.Provider, stored string) bool {
	if e.pathRewriter != nil {
		if !e.completeSourceMirror || e.storedPathResolver == nil {
			return false
		}
		resolved, ok := e.storedPathResolver(stored)
		if !ok {
			return false
		}
		stored = resolved
	}
	// FindSource may decline a path outside the configured roots. Check the
	// file itself so an unscanned or unreadable source keeps its identity.
	if _, err := os.Stat(stored); !os.IsNotExist(err) {
		return false
	}
	_, live := e.providerSourcePath(ctx, provider, stored)
	return !live
}

// collisionPolicyApplies reports whether the provider declares that two of
// its files can carry the same session id. Every other provider keeps its own
// rules, including planned moves between its files.
func collisionPolicyApplies(provider parser.Provider) bool {
	return provider.Capabilities().Source.SharedSessionIDs == parser.CapabilitySupported
}

// collisionPolicyAgents lists the configured agents sourceCollisionID covers.
func (e *Engine) collisionPolicyAgents() []string {
	var agents []string
	for agent, factory := range e.sources().providerFactories {
		if factory != nil && factory.Capabilities().Source.SharedSessionIDs == parser.CapabilitySupported {
			agents = append(agents, string(agent))
		}
	}
	return agents
}

// sessionPathRecords returns the stored and deleted records for fullID and
// its derived ids. During a rebuild it adds the original archive's records
// for ids the new archive has not written yet.
func (e *Engine) sessionPathRecords(ctx context.Context, fullID string) ([]db.SessionPathRecord, error) {
	records, err := e.db.ListSessionPathRecords(ctx, fullID)
	if err != nil {
		return nil, err
	}
	if index := e.archiveRebuildIndex; index != nil {
		seen := make(map[string]bool, len(records))
		for _, r := range records {
			seen[r.ID] = true
		}
		for _, r := range index.pathRecords[fullID] {
			if !seen[r.ID] {
				records = append(records, r)
			}
		}
	}
	return records, nil
}

// existingAltID returns the derived id already held by this file, stored or
// deleted, including one the provider has since moved to lookupPath, so its
// curation and any deletion carry over. It returns "" when there is none.
func (e *Engine) existingAltID(
	ctx context.Context, provider parser.Provider, records []db.SessionPathRecord,
	fullID, rawID, lookupPath string,
) string {
	minted := applyIDPrefixToID(e.idPrefix, parser.AltSessionID(rawID, lookupPath))
	for _, r := range records {
		if r.ID != fullID && (r.ID == minted || e.storedSourceLivesAt(ctx, provider, r.FilePath, lookupPath)) {
			return rawID + r.ID[len(fullID):]
		}
	}
	return ""
}

// claimSessionID records path as the owner of an id no stored session holds,
// for this pass. It fails when another file claimed the id earlier in the pass.
func (e *Engine) claimSessionID(
	ctx context.Context, provider parser.Provider, fullID, path string,
) bool {
	e.sourceClaimsMu.Lock()
	defer e.sourceClaimsMu.Unlock()
	if claimed := e.sourceClaims[fullID]; claimed != "" &&
		!e.storedSourceLivesAt(ctx, provider, claimed, path) {
		return false
	}
	if e.sourceClaims == nil {
		e.sourceClaims = make(map[string]string)
	}
	e.sourceClaims[fullID] = path
	return true
}

// resetSourceClaims forgets the previous pass's claims; their rows are written.
func (e *Engine) resetSourceClaims() {
	e.sourceClaimsMu.Lock()
	e.sourceClaims = nil
	e.sourceClaimsMu.Unlock()
}

// storedSourceLivesAt reports whether a stored source path is the file at
// path, or the provider has moved that source there.
func (e *Engine) storedSourceLivesAt(
	ctx context.Context, provider parser.Provider, stored, path string,
) bool {
	if stored == "" || stored == path || e.pathRewriter != nil {
		return stored != "" && stored == path
	}
	at, live := e.providerSourcePath(ctx, provider, stored)
	return live && at == path
}

// providerSourcePath asks the provider where it serves a stored source path
// now. live is false only when the provider proves the source gone. The
// session id is left out on purpose: a lookup by id can find another file
// with the same id and report a move that did not happen.
func (e *Engine) providerSourcePath(
	ctx context.Context, provider parser.Provider, stored string,
) (path string, live bool) {
	source, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
		StoredFilePath: stored, RequireFreshSource: true,
	})
	if err != nil {
		return stored, true
	}
	return providerDiscoveredPath(source), found
}
