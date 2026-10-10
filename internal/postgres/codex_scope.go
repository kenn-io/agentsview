package postgres

import "context"

// Scope and candidate are SQL aliases for a thread decision and a page. Old
// permanent exclusions have no owner; retain their original global meaning.
// Marker-less sessions also recognize machine names retained across renames.
const pgCodexScopeOwnerMatchSQL = `(
 scope.owner_marker IS NULL
 OR (scope.owner_marker <> '' AND scope.owner_marker = candidate.owner_marker)
 OR (scope.owner_marker = '' AND (
   scope.machine IN (candidate.machine, '', 'local')
   OR EXISTS (
     SELECT 1 FROM sync_metadata m
     WHERE (m.key = 'push_marker:' || candidate.owner_marker
       OR starts_with(m.key, 'push_marker:' || candidate.owner_marker || ':scope:'))
       AND m.value = scope.machine)
   OR EXISTS (
     SELECT 1 FROM sync_metadata m
     WHERE CASE WHEN m.key = 'push_marker_machine_aliases:' || candidate.owner_marker
       OR starts_with(m.key, 'push_marker_machine_aliases:' || candidate.owner_marker || ':scope:')
       THEN m.value::jsonb ? scope.machine ELSE FALSE END)
 )))`

// A scope match must not become a global exact-ID tombstone or delete a row
// owned by another archive when its excluded source next attempts a push.
func deletePGScopedExcludedSessionRows(ctx context.Context, pg pgSessionExecer, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := pg.ExecContext(ctx, `DELETE FROM sessions candidate USING excluded_sessions scope
		WHERE candidate.id = ANY($1) AND candidate.provenance_kind = 'legacy'
			AND scope.include_codex_pages AND left(right(candidate.id, 37), 1) = '_'
			AND scope.id = left(candidate.id, -37)
			AND (scope.agent IS NULL OR scope.agent = candidate.agent)
			AND `+pgCodexScopeOwnerMatchSQL, ids)
	return err
}
