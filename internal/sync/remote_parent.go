package sync

import (
	"context"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// remoteParentStores returns the databases that may already hold a remote
// session's parent. During a resync or rebuild e.db is the fresh database,
// which may not have received the parent's host yet, so the original archive
// is consulted as well.
func (e *Engine) remoteParentStores() []*db.DB {
	if e.db == nil {
		return nil
	}
	stores := []*db.DB{e.db}
	if old, ok := e.archiveStore.(*db.DB); ok && old != nil && old != e.db {
		stores = append(stores, old)
	}
	return stores
}

// remoteParentResolver maps the parser's raw parent ID of one pulled session
// to the archived parent's full ID. Remote sync prefixes every pulled ID with
// the source host, but a parent can live on another host or locally. The
// resolver keeps the stored parents of the child so a reparse does not
// replace a parent that was already resolved.
type remoteParentResolver struct {
	e        *Engine
	stores   []*db.DB
	childID  string
	loaded   bool
	stored   [2]string // parent_session_id, parser_parent_session_id
	resolved map[string]string
}

const (
	storedEffectiveParent = 0
	storedParserParent    = 1
)

// resolve returns the full parent ID for raw. Resolution order, fail-closed:
//
//  1. The source host's own "<host>~<raw>" when it exists. This is the
//     pre-existing behavior and never a guess.
//  2. The child's stored parent when it is a host-scoped form of raw and
//     still exists, so a reparse keeps an earlier resolution.
//  3. The only existing "<other-host>~<raw>" or bare local raw ID.
//  4. Otherwise the source host prefix. With no match the link waits for the
//     parent to arrive; with two or more matches the hosts hold copies of the
//     same session and picking one would be a guess. A later reparse
//     resolves the link once exactly one candidate remains.
func (r *remoteParentResolver) resolve(
	ctx context.Context, raw string, storedSlot int,
) (string, error) {
	prefix := r.e.idPrefix
	fallback := applyIDPrefixToID(prefix, raw)
	if len(r.stores) == 0 || strings.Contains(raw, "~") ||
		strings.HasPrefix(raw, prefix) {
		return fallback, nil
	}
	found, err := r.existsAny(ctx, fallback)
	if err != nil || found {
		return fallback, err
	}
	if err := r.loadStored(ctx); err != nil {
		return "", err
	}
	if stored := r.stored[storedSlot]; isHostScopedForm(stored, raw) {
		found, err := r.existsAny(ctx, stored)
		if err != nil {
			return "", err
		}
		if found {
			return stored, nil
		}
	}
	if got, ok := r.resolved[raw]; ok {
		return got, nil
	}
	candidates := map[string]struct{}{}
	for _, store := range r.stores {
		ids, err := store.HostScopedSessionIDs(ctx, raw)
		if err != nil {
			return "", err
		}
		for _, id := range ids {
			candidates[id] = struct{}{}
		}
	}
	got := fallback
	if len(candidates) == 1 {
		for id := range candidates {
			got = id
		}
	}
	if r.resolved == nil {
		r.resolved = map[string]string{}
	}
	r.resolved[raw] = got
	return got, nil
}

func (r *remoteParentResolver) existsAny(
	ctx context.Context, id string,
) (bool, error) {
	for _, store := range r.stores {
		found, err := store.LiveSessionExists(ctx, id)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

func (r *remoteParentResolver) loadStored(ctx context.Context) error {
	if r.loaded {
		return nil
	}
	for _, store := range r.stores {
		parent, parserParent, err := store.SessionParentIDs(ctx, r.childID)
		if err != nil {
			return err
		}
		if parent != "" || parserParent != "" {
			r.stored = [2]string{parent, parserParent}
			break
		}
	}
	r.loaded = true
	return nil
}

// isHostScopedForm reports whether id stores raw bare or behind exactly one
// host prefix.
func isHostScopedForm(id, raw string) bool {
	if id == "" || raw == "" {
		return false
	}
	if id == raw {
		return true
	}
	host, ok := strings.CutSuffix(id, "~"+raw)
	return ok && host != "" && !strings.Contains(host, "~")
}
