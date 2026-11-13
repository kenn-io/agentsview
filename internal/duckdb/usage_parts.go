package duckdb

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/kept"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

type usagePart[T any] struct {
	ID, Fingerprint string
	Group           string
	Present         bool
	Value           T
}

var usagePartReadObserver func([]string)
var usagePartPublishObserver func()

// refreshUsageParts retains only parts whose source sessions are unchanged.
func refreshUsageParts[T any](ctx context.Context, s *Store, slot, priceDigest string, f db.UsageFilter, size func(T) int64, read func(context.Context, []string) (map[string]T, error)) (map[string]T, error) {
	if !s.usageDisk.Enabled() || slot == "" {
		return read(ctx, nil)
	}
	stamp := s.keptVersion()
	versions, err := s.usagePartVersions(ctx)
	if err != nil {
		return nil, err
	}
	version := usagePartsVersion(f, priceDigest)
	old := map[string]usagePart[T]{}
	if data, ok := s.usageDisk.Get(slot, version); ok {
		if rows, ok := kept.DecodeRows[usagePart[T]](data); ok {
			for _, row := range rows {
				old[row.ID] = row
			}
		}
	}
	changed := map[string]bool{}
	for id, fingerprint := range versions {
		part, ok := old[id]
		if !ok || fingerprint == "" || fingerprint != part.Fingerprint {
			changed[id] = true
		}
	}
	for id := range old {
		if _, ok := versions[id]; !ok {
			changed[id] = true
		}
	}
	if len(changed) == 0 && len(old) != 0 {
		values := make(map[string]T, len(old))
		for id, part := range old {
			if part.Present {
				values[id] = part.Value
			}
		}
		return values, nil
	}
	peers, err := s.usagePartPeers(ctx)
	if err != nil {
		return nil, err
	}
	oldGroups, newGroups := map[string]bool{}, map[string]bool{}
	for id := range changed {
		if group := old[id].Group; group != "" {
			oldGroups[group] = true
		}
		if group := peers[id]; group != "" {
			newGroups[group] = true
		}
	}
	for id := range versions {
		if oldGroups[old[id].Group] || newGroups[peers[id]] {
			changed[id] = true
		}
	}
	values := make(map[string]T, len(versions))
	for id, part := range old {
		if !changed[id] && part.Present {
			values[id] = part.Value
		}
	}
	var ids []string
	if len(old) != 0 {
		ids = make([]string, 0, len(changed))
		for id := range changed {
			if _, ok := versions[id]; ok {
				ids = append(ids, id)
			}
		}
	}
	if len(old) == 0 || len(ids) > 0 {
		if usagePartReadObserver != nil {
			usagePartReadObserver(ids)
		}
		updated, err := read(ctx, ids)
		if err != nil {
			return nil, err
		}
		for id, value := range updated {
			values[id] = value
		}
	}
	if usagePartPublishObserver != nil {
		usagePartPublishObserver()
	}
	if ctx.Err() == nil && stamp != "" && stamp == s.keptVersion() {
		rows := make([]usagePart[T], 0, len(versions))
		for id, fingerprint := range versions {
			value, present := values[id]
			rows = append(rows, usagePart[T]{id, fingerprint, peers[id], present, value})
		}
		if data, err := kept.EncodeRows(rows, 128<<20, func(part usagePart[T]) int64 {
			return int64(len(part.ID)+len(part.Fingerprint)+len(part.Group)) + size(part.Value)
		}); err == nil {
			s.usageDisk.Put(slot, version, data)
		}
	}
	return values, nil
}

func (s *Store) usagePartVersions(ctx context.Context) (map[string]string, error) {
	rows, err := s.queryContext(ctx, `SELECT id, COALESCE(agentsview_push_fingerprint, '') FROM sessions WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("querying usage fingerprints: %w", err)
	}
	versions := map[string]string{}
	for rows.Next() {
		var id, fingerprint string
		if err := rows.Scan(&id, &fingerprint); err != nil {
			rows.Close()
			return nil, err
		}
		versions[id] = fingerprint
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var count int64
	var hash uint64
	if err := s.queryRowContext(ctx, `SELECT COUNT(*), COALESCE(BIT_XOR(HASH(cu)), 0) FROM cursor_usage_events cu`).Scan(&count, &hash); err != nil {
		return nil, err
	}
	versions[""] = fmt.Sprintf("%d|%d", count, hash)
	return versions, nil
}

func (s *Store) usagePartPeers(ctx context.Context) (map[string]string, error) {
	rows, err := s.queryContext(ctx, `WITH keys AS (
		SELECT DISTINCT m.session_id,
			CASE WHEN m.claude_message_id != '' THEN 'c:' || m.claude_message_id || ':' || m.claude_request_id
			ELSE 's:' || s.agent || ':' || m.source_uuid END AS k
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.deleted_at IS NULL AND `+duckUsageMessageSourceEligibility+`
		AND (m.claude_message_id != '' OR (s.agent != '' AND m.source_uuid != ''))
	), shared AS (SELECT k FROM keys GROUP BY k HAVING COUNT(*) > 1)
	SELECT keys.k, keys.session_id FROM keys JOIN shared USING (k) ORDER BY keys.k`)
	if err != nil {
		return nil, fmt.Errorf("querying usage peers: %w", err)
	}
	defer rows.Close()
	parents := map[string]string{}
	var root func(string) string
	root = func(id string) string {
		if parent, ok := parents[id]; !ok {
			parents[id] = id
			return id
		} else if parent != id {
			parents[id] = root(parent)
		}
		return parents[id]
	}
	var previous, first string
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		if key != previous || first == "" {
			previous, first = key, id
			root(id)
			continue
		}
		a, b := root(first), root(id)
		if a < b {
			parents[b] = a
		} else {
			parents[a] = b
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id := range parents {
		parents[id] = root(id)
	}
	return parents, nil
}

func usagePartsVersion(f db.UsageFilter, priceDigest string) string {
	expr, zone := duckUsageLocalDateSQL(f)
	return fmt.Sprintf("usage-parts-2|%s|%s|%s|%v", priceDigest, pricingpkg.ConfiguredAliasesDigest(), expr, zone)
}
