package db

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

type bunContentSession struct {
	ID               string  `bun:"id"`
	Project          string  `bun:"project"`
	Agent            string  `bun:"agent"`
	RelationshipType string  `bun:"relationship_type"`
	ParentSessionID  *string `bun:"parent_session_id"`
}

type bunContentMessage struct {
	SessionID    string              `bun:"session_id"`
	Ordinal      int                 `bun:"ordinal"`
	Role         string              `bun:"role"`
	Content      string              `bun:"content"`
	RawTimestamp any                 `bun:"timestamp"`
	Timestamp    *bunmodel.Timestamp `bun:"-"`
	IsSystem     bool                `bun:"is_system"`
	IsSidechain  bool                `bun:"is_sidechain"`
}

type bunContentMessageKey struct {
	sessionID string
	ordinal   int
}

type bunHybridAnchor struct {
	SessionID   string `bun:"session_id"`
	Ordinal     int    `bun:"ordinal"`
	Role        string `bun:"role"`
	Content     string `bun:"content"`
	IsSystem    bool   `bun:"is_system"`
	IsSidechain bool   `bun:"is_sidechain"`
}

type bunContentCandidate struct {
	SessionID  string              `bun:"session_id"`
	Ordinal    int                 `bun:"ordinal"`
	Location   string              `bun:"location"`
	ToolName   string              `bun:"tool_name"`
	Body       string              `bun:"body"`
	Timestamp  *bunmodel.Timestamp `bun:"source_timestamp"`
	CallIndex  int                 `bun:"call_index"`
	EventIndex int                 `bun:"event_index"`
}

// SearchContent resolves dialect-specific lexical hits through canonical Bun
// session and message rows. Semantic and hybrid search join this dispatcher
// when their capability slice is installed.
func (s *BunStore) SearchContent(
	ctx context.Context, filter ContentSearchFilter,
) (ContentSearchPage, error) {
	if filter.Limit <= 0 || filter.Limit > MaxContentSearchLimit {
		filter.Limit = DefaultContentSearchLimit
	}
	if filter.Pattern == "" {
		return ContentSearchPage{}, nil
	}
	if filter.Mode == "semantic" || filter.Mode == "hybrid" {
		if err := ValidateSemanticFilter(filter); err != nil {
			return ContentSearchPage{}, err
		}
		semantic := s.backend.Capabilities().Semantic
		if semantic == nil {
			return ContentSearchPage{}, ErrSemanticUnavailable
		}
		if !semantic.Available() {
			return ContentSearchPage{}, semantic.UnavailableError()
		}
		if filter.Mode == "hybrid" {
			lexical := s.backend.Capabilities().HybridLexical
			if lexical == nil || !lexical.Available() {
				return ContentSearchPage{}, errFTSUnavailable
			}
			return s.searchContentHybrid(ctx, filter, semantic, lexical)
		}
		return s.searchContentSemantic(ctx, filter, semantic)
	}
	if filter.Mode == "fts" && len(filter.Sources) == 0 {
		filter.Sources = []string{"messages"}
	} else if len(filter.Sources) == 0 {
		filter.Sources = []string{"messages", "tool_input", "tool_result"}
	}
	for _, source := range filter.Sources {
		if source != "messages" && source != "tool_input" && source != "tool_result" {
			return ContentSearchPage{}, searchInputErrorf(
				"search: unknown source %q", source,
			)
		}
		if filter.Mode == "fts" && source != "messages" {
			return ContentSearchPage{}, searchInputErrorf(
				"search: full-text search only supports the messages source (got %q)",
				source,
			)
		}
	}
	switch filter.Mode {
	case "", "substring", "regex", "fts":
	default:
		return ContentSearchPage{}, searchInputErrorf(
			"search: invalid mode %q", filter.Mode,
		)
	}
	contentCapability := s.backend.Capabilities().ContentSearch
	if filter.Mode == "fts" && contentCapability != nil &&
		!contentCapability.Available() {
		return ContentSearchPage{}, errFTSUnavailable
	}

	var page ContentSearchPage
	err := s.consistentView(ctx, func(store bun.IDB) error {
		attemptFilter := filter
		attemptFilter.Limit = filter.Limit + 1
		var hits []ContentSearchHit
		var err error
		switch filter.Mode {
		case "", "substring":
			hits, err = s.bunContentSubstringHits(ctx, store, attemptFilter)
		case "fts":
			if contentCapability != nil {
				hits, err = contentCapability.SearchContent(ctx, store, attemptFilter)
			} else if s.backend.Capabilities().SearchDialect.portableContentFTS {
				hits, err = s.bunContentPortableFTSHits(ctx, store, attemptFilter)
			} else {
				return errFTSUnavailable
			}
		case "regex":
			hits, err = s.bunContentRegexHits(ctx, store, attemptFilter)
		}
		if err != nil {
			return fmt.Errorf("searching content candidates: %w", err)
		}
		attempt, err := s.hydrateContentSearchHits(ctx, store, filter, hits)
		if err != nil {
			return err
		}
		page = attempt
		return nil
	})
	return page, err
}

// HasSemantic reports whether the backend currently has an available vector
// search capability.
func (s *BunStore) HasSemantic() bool {
	if s == nil || s.backend == nil {
		return false
	}
	capability := s.backend.Capabilities().Semantic
	return capability != nil && capability.Available()
}

func (s *BunStore) searchContentSemantic(
	ctx context.Context, filter ContentSearchFilter,
	capability SemanticCapability,
) (ContentSearchPage, error) {
	attemptFilter := filter
	attemptFilter.Limit = max(filter.Limit*4, SemanticOverfetchMin)
	var hits []ContentSearchHit
	if err := s.view(ctx, func(store bun.IDB) error {
		var err error
		hits, err = capability.SearchContent(ctx, store, attemptFilter)
		return err
	}); err != nil {
		return ContentSearchPage{}, fmt.Errorf("searching semantic capability: %w", err)
	}
	var page ContentSearchPage
	err := s.consistentView(ctx, func(store bun.IDB) error {
		eligibleHits, err := s.filterContentHitsBySessionScope(
			ctx, store, filter, hits,
		)
		if err != nil {
			return err
		}
		eligible := make([]ContentSearchHit, 0, len(eligibleHits))
		for _, hit := range eligibleHits {
			if !ScopeExcludes(filter.Scope, hit.Subordinate) {
				eligible = append(eligible, hit)
			}
		}
		eligible = applyContentSubordinatePenalty(eligible)
		attempt, err := s.hydrateContentSearchHits(
			ctx, store, filter, eligible,
		)
		if err != nil {
			return err
		}
		page = attempt
		return nil
	})
	return page, err
}

type contentHybridLeg struct {
	ranked  []RankedUnit
	display map[string]ContentSearchHit
}

const maxHybridLexicalBatches = 4

func (s *BunStore) searchContentHybrid(
	ctx context.Context, filter ContentSearchFilter,
	semantic SemanticCapability, lexical HybridLexicalCapability,
) (ContentSearchPage, error) {
	candidateLimit := max(filter.Limit*4, SemanticOverfetchMin)
	candidateFilter := filter
	candidateFilter.Limit = candidateLimit
	var semanticHits []ContentSearchHit
	lexicalLeg := contentHybridLeg{display: make(map[string]ContentSearchHit)}
	if err := s.view(ctx, func(store bun.IDB) error {
		var err error
		semanticHits, err = semantic.SearchContent(ctx, store, candidateFilter)
		if err != nil {
			return fmt.Errorf("searching semantic capability: %w", err)
		}
		for batch := range maxHybridLexicalBatches {
			batchFilter := candidateFilter
			batchFilter.Cursor = batch * candidateLimit
			lexicalHits, err := lexical.SearchHybridContent(ctx, store, batchFilter)
			if err != nil {
				return fmt.Errorf("searching hybrid lexical capability: %w", err)
			}
			if err := s.appendHybridLexicalHits(
				ctx, store, semantic, filter.Scope, lexicalHits, &lexicalLeg,
			); err != nil {
				return err
			}
			if len(lexicalHits) < candidateLimit || len(lexicalLeg.ranked) >= candidateLimit {
				break
			}
		}
		return nil
	}); err != nil {
		return ContentSearchPage{}, err
	}
	var page ContentSearchPage
	err := s.consistentView(ctx, func(store bun.IDB) error {
		eligibleSemanticHits, err := s.filterContentHitsBySessionScope(
			ctx, store, filter, semanticHits,
		)
		if err != nil {
			return err
		}
		eligibleSemanticHits, err = filterContentHitsByLiveAnchor(
			ctx, store, eligibleSemanticHits, s.backend,
		)
		if err != nil {
			return err
		}
		semanticLeg := contentHybridLegFromHits(
			eligibleSemanticHits, filter.Scope, candidateLimit,
		)
		liveLexicalLeg, err := filterContentHybridLegByLiveAnchor(
			ctx, store, lexicalLeg, s.backend,
		)
		if err != nil {
			return err
		}

		merged := RRFMerge(
			[][]RankedUnit{semanticLeg.ranked, liveLexicalLeg.ranked}, filter.Limit,
		)
		fusedHits := make([]ContentSearchHit, 0, len(merged))
		for _, fused := range merged {
			hit, ok := liveLexicalLeg.display[fused.Unit.Key]
			if !ok {
				hit = semanticLeg.display[fused.Unit.Key]
			}
			score := fused.Score
			hit.Score = &score
			fusedHits = append(fusedHits, hit)
		}
		page, err = s.hydrateContentSearchHits(ctx, store, filter, fusedHits)
		return err
	})
	return page, err
}

func filterContentHitsByLiveAnchor(
	ctx context.Context, store bun.IDB, hits []ContentSearchHit,
	backend BunBackend,
) ([]ContentSearchHit, error) {
	messages, err := bunContentMessagesForHits(
		ctx, store, hits, backend,
	)
	if err != nil {
		return nil, err
	}
	live := make([]ContentSearchHit, 0, len(hits))
	for _, hit := range hits {
		if _, ok := messages[bunContentMessageKey{hit.SessionID, hit.Ordinal}]; ok {
			live = append(live, hit)
		}
	}
	return live, nil
}

func filterContentHybridLegByLiveAnchor(
	ctx context.Context, store bun.IDB, leg contentHybridLeg,
	backend BunBackend,
) (contentHybridLeg, error) {
	hits := make([]ContentSearchHit, 0, len(leg.ranked))
	for _, ranked := range leg.ranked {
		if hit, ok := leg.display[ranked.Key]; ok {
			hits = append(hits, hit)
		}
	}
	liveHits, err := filterContentHitsByLiveAnchor(
		ctx, store, hits, backend,
	)
	if err != nil {
		return contentHybridLeg{}, err
	}
	liveKeys := make(map[string]struct{}, len(liveHits))
	for _, hit := range liveHits {
		liveKeys[hit.DocKey] = struct{}{}
	}
	live := contentHybridLeg{
		ranked:  make([]RankedUnit, 0, len(liveHits)),
		display: make(map[string]ContentSearchHit, len(liveHits)),
	}
	for _, ranked := range leg.ranked {
		if _, ok := liveKeys[ranked.Key]; !ok {
			continue
		}
		live.ranked = append(live.ranked, ranked)
		live.display[ranked.Key] = leg.display[ranked.Key]
	}
	return live, nil
}

func contentHybridLegFromHits(
	hits []ContentSearchHit, scope string, limit int,
) contentHybridLeg {
	leg := contentHybridLeg{display: make(map[string]ContentSearchHit, len(hits))}
	for _, hit := range hits {
		if ScopeExcludes(scope, hit.Subordinate) {
			continue
		}
		key := hit.DocKey
		if key == "" {
			key = UnitFusionKey(hit.SessionID, hit.OrdinalStart)
		}
		if _, exists := leg.display[key]; exists {
			continue
		}
		hit.DocKey = key
		leg.ranked = append(leg.ranked, RankedUnit{Key: key, Subordinate: hit.Subordinate})
		leg.display[key] = hit
		if limit > 0 && len(leg.ranked) >= limit {
			break
		}
	}
	return leg
}

func (s *BunStore) appendHybridLexicalHits(
	ctx context.Context, store bun.IDB, semantic SemanticCapability, scope string,
	hits []ContentSearchHit, leg *contentHybridLeg,
) error {
	if len(hits) == 0 {
		return nil
	}
	refs := make([]MessageRef, len(hits))
	for i, hit := range hits {
		refs[i] = MessageRef{SessionID: hit.SessionID, Ordinal: hit.Ordinal}
	}
	units, err := semantic.ResolveMessageUnits(ctx, store, refs)
	if err != nil {
		return fmt.Errorf("resolving hybrid lexical hits to units: %w", err)
	}
	if len(units) != len(refs) {
		return fmt.Errorf(
			"resolving hybrid lexical hits to units: got %d units for %d refs",
			len(units), len(refs),
		)
	}
	var unitless []int
	for i := range hits {
		hits[i].DocKey = MessageFusionKey(hits[i].SessionID, hits[i].Ordinal)
		hits[i].OrdinalStart = hits[i].Ordinal
		hits[i].OrdinalEnd = hits[i].Ordinal
		if units[i].DocKey == "" {
			unitless = append(unitless, i)
			continue
		}
		hits[i].DocKey = UnitFusionKey(units[i].SessionID, units[i].OrdinalStart)
		hits[i].OrdinalStart = units[i].OrdinalStart
		hits[i].OrdinalEnd = units[i].OrdinalEnd
		hits[i].RangeResolved = true
		hits[i].Subordinate = units[i].Subordinate
	}
	if err := s.classifyUnitlessHybridContentHits(ctx, store, hits, unitless); err != nil {
		return err
	}
	for _, hit := range hits {
		if ScopeExcludes(scope, hit.Subordinate) {
			continue
		}
		if _, exists := leg.display[hit.DocKey]; exists {
			continue
		}
		leg.ranked = append(leg.ranked, RankedUnit{
			Key: hit.DocKey, Subordinate: hit.Subordinate,
		})
		leg.display[hit.DocKey] = hit
	}
	return nil
}

func (s *BunStore) filterContentHitsBySessionScope(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
	hits []ContentSearchHit,
) ([]ContentSearchHit, error) {
	ids := make([]string, 0, len(hits))
	seen := make(map[string]struct{}, len(hits))
	for _, hit := range hits {
		if hit.SessionID == "" {
			continue
		}
		if _, exists := seen[hit.SessionID]; exists {
			continue
		}
		seen[hit.SessionID] = struct{}{}
		ids = append(ids, hit.SessionID)
	}
	where, args := buildBunSessionBaseFilter(
		semanticContentSessionFilter(filter), s.backend.TimestampOrderExpr,
	)
	allowed := make(map[string]struct{}, len(ids))
	if err := queryChunked(ids, func(chunk []string) error {
		var rows []struct {
			ID string `bun:"id"`
		}
		query := store.NewSelect().TableExpr("sessions AS session").
			ColumnExpr("session.id AS id").Where("session.id IN (?)", bun.List(chunk))
		query = applyBunWhere(query, where, args)
		if err := query.Scan(ctx, &rows); err != nil {
			return fmt.Errorf("filtering semantic session scope: %w", err)
		}
		for _, row := range rows {
			allowed[row.ID] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]ContentSearchHit, 0, len(hits))
	for _, hit := range hits {
		if _, ok := allowed[hit.SessionID]; ok {
			out = append(out, hit)
		}
	}
	return out, nil
}

func (s *BunStore) classifyUnitlessHybridContentHits(
	ctx context.Context, store bun.IDB, hits []ContentSearchHit, indexes []int,
) error {
	if len(indexes) == 0 {
		return nil
	}
	selected := make([]ContentSearchHit, len(indexes))
	for i, index := range indexes {
		selected[i] = hits[index]
	}
	messages, err := bunHybridAnchorsForHits(ctx, store, selected)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(selected))
	seen := make(map[string]struct{}, len(selected))
	for _, hit := range selected {
		if _, exists := seen[hit.SessionID]; !exists {
			seen[hit.SessionID] = struct{}{}
			ids = append(ids, hit.SessionID)
		}
	}
	var sessions []bunContentSession
	if err := store.NewSelect().TableExpr("sessions AS session").
		ColumnExpr("session.id AS id").
		ColumnExpr("session.relationship_type AS relationship_type").
		ColumnExpr("session.parent_session_id AS parent_session_id").
		Where("session.id IN (?)", bun.List(ids)).Scan(ctx, &sessions); err != nil {
		return fmt.Errorf("classifying hybrid lexical sessions: %w", err)
	}
	bySession := make(map[string]bunContentSession, len(sessions))
	for _, session := range sessions {
		bySession[session.ID] = session
	}
	anchors := make([]UnitAnchor, len(indexes))
	for i, index := range indexes {
		hit := hits[index]
		message, ok := messages[bunContentMessageKey{hit.SessionID, hit.Ordinal}]
		anchors[i] = UnitAnchor{
			SessionID: hit.SessionID, Ordinal: hit.Ordinal, Role: message.Role,
			Sidechain: message.IsSidechain,
			Embeddable: ok && !message.IsSystem &&
				!IsSystemPrefixed(message.Content, message.Role),
			Missing: !ok,
		}
	}
	ranges, err := DeriveUnitRanges(
		ctx, bunUnitBoundsQuerier{store: store, parent: s}, anchors,
	)
	if err != nil {
		return fmt.Errorf("deriving hybrid unit-less ranges: %w", err)
	}
	for i, index := range indexes {
		session := bySession[hits[index].SessionID]
		parentID := ""
		if session.ParentSessionID != nil {
			parentID = *session.ParentSessionID
		}
		hits[index].OrdinalStart = ranges[i][0]
		hits[index].OrdinalEnd = ranges[i][1]
		hits[index].RangeResolved = true
		hits[index].Subordinate = anchors[i].Sidechain ||
			SubordinateSession(session.RelationshipType, parentID)
	}
	return nil
}

func bunHybridAnchorsForHits(
	ctx context.Context, store bun.IDB, hits []ContentSearchHit,
) (map[bunContentMessageKey]bunHybridAnchor, error) {
	out := make(map[bunContentMessageKey]bunHybridAnchor, len(hits))
	const chunkSize = 400
	for start := 0; start < len(hits); start += chunkSize {
		chunk := hits[start:min(start+chunkSize, len(hits))]
		values := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*2)
		for i, hit := range chunk {
			values[i] = "(?, ?)"
			args = append(args, hit.SessionID, hit.Ordinal)
		}
		var rows []bunHybridAnchor
		query := `WITH refs(session_id, ordinal) AS (VALUES ` +
			strings.Join(values, ", ") + `)
			SELECT message.session_id, message.ordinal, message.role,
				message.content, message.is_system, message.is_sidechain
			FROM refs
			JOIN messages AS message
				ON message.session_id = refs.session_id
				AND message.ordinal = refs.ordinal`
		if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
			return nil, fmt.Errorf("classifying hybrid lexical messages: %w", err)
		}
		for _, row := range rows {
			out[bunContentMessageKey{row.SessionID, row.Ordinal}] = row
		}
	}
	return out, nil
}

func applyContentSubordinatePenalty(
	hits []ContentSearchHit,
) []ContentSearchHit {
	leg := make([]RankedUnit, 0, len(hits))
	byKey := make(map[string]ContentSearchHit, len(hits))
	for _, hit := range hits {
		key := UnitFusionKey(hit.SessionID, hit.OrdinalStart)
		if _, exists := byKey[key]; exists {
			continue
		}
		leg = append(leg, RankedUnit{
			Key: key, Subordinate: hit.Subordinate,
		})
		byKey[key] = hit
	}
	merged := RRFMerge([][]RankedUnit{leg}, 0)
	out := make([]ContentSearchHit, 0, len(merged))
	for _, item := range merged {
		out = append(out, byKey[item.Unit.Key])
	}
	return out
}

func (s *BunStore) hydrateContentSearchHits(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
	hits []ContentSearchHit,
) (ContentSearchPage, error) {
	if len(hits) == 0 {
		return ContentSearchPage{}, nil
	}
	ids := make([]string, 0, len(hits))
	seen := make(map[string]struct{}, len(hits))
	for _, hit := range hits {
		if hit.SessionID == "" {
			continue
		}
		if _, ok := seen[hit.SessionID]; ok {
			continue
		}
		seen[hit.SessionID] = struct{}{}
		ids = append(ids, hit.SessionID)
	}
	var where string
	var args []any
	if filter.Mode == "semantic" || filter.Mode == "hybrid" {
		where, args = buildBunSessionBaseFilter(
			semanticContentSessionFilter(filter),
			s.backend.TimestampOrderExpr,
		)
	} else {
		where, args = buildBunSessionFilter(
			contentSessionFilter(filter), s.backend.TimestampOrderExpr,
		)
	}
	var sessions []bunContentSession
	query := store.NewSelect().TableExpr("sessions AS session").
		ColumnExpr("session.id AS id").ColumnExpr("session.project AS project").
		ColumnExpr("session.agent AS agent").
		ColumnExpr("session.relationship_type AS relationship_type").
		ColumnExpr("session.parent_session_id AS parent_session_id").
		Where("session.id IN (?)", bun.List(ids))
	query = applyBunWhere(query, where, args)
	if err := query.Scan(ctx, &sessions); err != nil {
		return ContentSearchPage{}, fmt.Errorf("hydrating content search sessions: %w", err)
	}
	bySession := make(map[string]bunContentSession, len(sessions))
	for _, session := range sessions {
		bySession[session.ID] = session
	}

	messages, err := bunContentMessagesForHits(
		ctx, store, hits, s.backend,
	)
	if err != nil {
		return ContentSearchPage{}, err
	}
	page := ContentSearchPage{
		Matches: make([]ContentMatch, 0, min(len(hits), filter.Limit+1)),
	}
	deriveRange := make([]bool, 0, min(len(hits), filter.Limit+1))
	for _, hit := range hits {
		session, ok := bySession[hit.SessionID]
		if !ok {
			continue
		}
		message, messageExists := messages[bunContentMessageKey{hit.SessionID, hit.Ordinal}]
		if (filter.Mode == "semantic" || filter.Mode == "hybrid") && !messageExists {
			continue
		}
		ordinalRange := [2]int{hit.OrdinalStart, hit.OrdinalEnd}
		if ordinalRange == [2]int{} && hit.Ordinal != 0 {
			ordinalRange = [2]int{hit.Ordinal, hit.Ordinal}
		}
		parentID := ""
		if session.ParentSessionID != nil {
			parentID = *session.ParentSessionID
		}
		role := message.Role
		if hit.Location != "message" {
			role = "assistant"
		}
		snippet := hit.Snippet
		if filter.Mode == "semantic" || filter.Mode == "hybrid" {
			snippet = filter.SemanticSnippet(message.Content, hit.Snippet)
		}
		page.Matches = append(page.Matches, ContentMatch{
			SessionID: hit.SessionID, Project: session.Project, Agent: session.Agent,
			Location: hit.Location, Role: role, ToolName: hit.ToolName,
			Ordinal: hit.Ordinal, Timestamp: bunAnalyticsTimeString(message.Timestamp),
			Snippet: snippet, Score: hit.Score, OrdinalRange: ordinalRange,
			Subordinate: hit.Subordinate || message.IsSidechain ||
				SubordinateSession(session.RelationshipType, parentID),
			Relationship: session.RelationshipType, ParentSessionID: parentID,
			Sidechain: message.IsSidechain,
		})
		deriveRange = append(deriveRange, !hit.RangeResolved)
		if hit.Timestamp != "" {
			page.Matches[len(page.Matches)-1].Timestamp = hit.Timestamp
		}
	}
	if len(page.Matches) > filter.Limit {
		page.Matches = page.Matches[:filter.Limit]
		deriveRange = deriveRange[:filter.Limit]
		if filter.Mode != "semantic" && filter.Mode != "hybrid" {
			page.NextCursor = filter.Cursor + filter.Limit
		}
	}
	anchors := make([]UnitAnchor, 0, len(page.Matches))
	anchorIndexes := make([]int, 0, len(page.Matches))
	for i, match := range page.Matches {
		if !deriveRange[i] {
			continue
		}
		message := messages[bunContentMessageKey{match.SessionID, match.Ordinal}]
		anchorIndexes = append(anchorIndexes, i)
		anchors = append(anchors, UnitAnchor{
			SessionID: match.SessionID, Ordinal: match.Ordinal,
			Role: message.Role, Sidechain: message.IsSidechain,
			Embeddable: !message.IsSystem &&
				!IsSystemPrefixed(message.Content, message.Role),
			Missing: message.Role == "",
		})
	}
	ranges, err := DeriveUnitRanges(
		ctx, bunUnitBoundsQuerier{store: store, parent: s}, anchors,
	)
	if err != nil {
		return ContentSearchPage{}, fmt.Errorf("deriving Bun content ranges: %w", err)
	}
	for i, matchIndex := range anchorIndexes {
		page.Matches[matchIndex].OrdinalRange = ranges[i]
	}
	return page, nil
}

func (s *BunStore) bunContentSubstringHits(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	if s.backend.Name() == "sqlite" {
		return s.bunContentUnicodeSubstringHits(ctx, store, filter)
	}
	rows, err := s.bunContentCandidateRows(
		ctx, store, filter, filter.Pattern, false,
	)
	if err != nil {
		return nil, err
	}
	hits := make([]ContentSearchHit, len(rows))
	for i, row := range rows {
		hits[i] = bunContentHitFromCandidate(
			row, filter.substringSnippet(row.Body),
		)
	}
	return hits, nil
}

func (s *BunStore) bunContentUnicodeSubstringHits(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	query, args := s.bunContentCandidateQuery(filter, "", true, false)
	if query == "" {
		return nil, nil
	}
	rows, err := store.QueryContext(ctx, store.NewRaw(query, args...).String())
	if err != nil {
		return nil, fmt.Errorf("streaming Bun Unicode substring candidates: %w", err)
	}
	defer rows.Close()
	confirmed := 0
	hits := make([]ContentSearchHit, 0, filter.Limit)
	for rows.Next() && len(hits) < filter.Limit {
		var row bunContentCandidate
		var rawTimestamp any
		if err := rows.Scan(
			&row.SessionID, &row.Ordinal, &row.Location, &row.ToolName,
			&row.Body, &rawTimestamp, &row.CallIndex, &row.EventIndex,
		); err != nil {
			return nil, fmt.Errorf("scanning Bun Unicode substring candidate: %w", err)
		}
		row.Timestamp, err = bunAvailableTimestamp(rawTimestamp)
		if err != nil {
			return nil, fmt.Errorf(
				"scanning Bun Unicode substring timestamp: %w", err,
			)
		}
		if CaseInsensitiveIndex(row.Body, filter.Pattern) < 0 {
			continue
		}
		if confirmed < filter.Cursor {
			confirmed++
			continue
		}
		hits = append(hits, bunContentHitFromCandidate(
			row, filter.substringSnippet(row.Body),
		))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating Bun Unicode substring candidates: %w", err)
	}
	return hits, nil
}

func (s *BunStore) bunContentRegexHits(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	re, err := regexp.Compile(filter.Pattern)
	if err != nil {
		return nil, searchInputErrorf("search: invalid regex: %v", err)
	}
	literal := literalPrefix(filter.Pattern)
	query, args := s.bunContentCandidateQuery(filter, literal, literal == "", false)
	if query == "" {
		return nil, nil
	}
	formatted := store.NewRaw(query, args...).String()
	rows, err := store.QueryContext(ctx, formatted)
	if err != nil {
		return nil, fmt.Errorf("streaming Bun regex candidates: %w", err)
	}
	defer rows.Close()
	confirmed := 0
	hits := make([]ContentSearchHit, 0, filter.Limit)
	for rows.Next() && len(hits) < filter.Limit {
		var row bunContentCandidate
		var rawTimestamp any
		if err := rows.Scan(
			&row.SessionID, &row.Ordinal, &row.Location, &row.ToolName,
			&row.Body, &rawTimestamp, &row.CallIndex, &row.EventIndex,
		); err != nil {
			return nil, fmt.Errorf("scanning Bun regex candidate: %w", err)
		}
		row.Timestamp, err = bunAvailableTimestamp(rawTimestamp)
		if err != nil {
			return nil, fmt.Errorf("scanning Bun regex timestamp: %w", err)
		}
		span := re.FindStringIndex(row.Body)
		if span == nil {
			continue
		}
		if confirmed < filter.Cursor {
			confirmed++
			continue
		}
		hits = append(hits, bunContentHitFromCandidate(
			row, filter.buildSnippet(row.Body, span[0], span[1]),
		))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating Bun regex candidates: %w", err)
	}
	return hits, nil
}

func (s *BunStore) bunContentPortableFTSHits(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	terms := FTSTerms(PrepareFTSQuery(filter.Pattern))
	if len(terms) == 0 {
		return nil, nil
	}
	where, scopeArgs := buildBunSessionFilter(
		contentSessionFilter(filter), s.backend.TimestampOrderExpr,
	)
	predicates := make([]string, len(terms))
	args := make([]any, 0, len(terms)+len(scopeArgs)+2)
	for i, term := range terms {
		predicates[i] = "LOWER(message.content) LIKE ? ESCAPE '\\'"
		args = append(args, "%"+EscapeLikePattern(strings.ToLower(term))+"%")
	}
	system := "1=1"
	if filter.ExcludeSystem {
		system = "message.is_system = FALSE AND " +
			s.bunContentSystemPrefixSQL("message.content", "message.role")
	}
	query := `SELECT message.session_id, message.ordinal,
		'message' AS location, '' AS tool_name, message.content AS body,
		-1 AS call_index, -1 AS event_index
		FROM messages AS message
		JOIN sessions AS session ON session.id = message.session_id
		WHERE ` + strings.Join(predicates, " AND ") + `
			AND ` + system + `
			AND message.session_id IN (SELECT id FROM sessions AS session WHERE ` + where + `)
		ORDER BY COALESCE(session.ended_at, session.started_at, session.created_at)
			DESC NULLS LAST, message.session_id ASC, message.ordinal ASC,
			COALESCE(message.id, 0) ASC
		LIMIT ? OFFSET ?`
	args = append(args, scopeArgs...)
	args = append(args, filter.Limit, filter.Cursor)
	var rows []bunContentCandidate
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying portable Bun FTS candidates: %w", err)
	}
	hits := make([]ContentSearchHit, len(rows))
	for i, row := range rows {
		hits[i] = bunContentHitFromCandidate(row, filter.ftsSnippet(row.Body))
	}
	return hits, nil
}

func (s *BunStore) bunContentCandidateRows(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
	literal string, matchAll bool,
) ([]bunContentCandidate, error) {
	query, args := s.bunContentCandidateQuery(filter, literal, matchAll, true)
	if query == "" {
		return nil, nil
	}
	var rows []bunContentCandidate
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying Bun content candidates: %w", err)
	}
	return rows, nil
}

func (s *BunStore) bunContentCandidateQuery(
	filter ContentSearchFilter, literal string, matchAll, paginate bool,
) (string, []any) {
	where, scopeArgs := buildBunSessionFilter(
		contentSessionFilter(filter), s.backend.TimestampOrderExpr,
	)
	scope := func(column string) string {
		return column + " IN (SELECT id FROM sessions AS session WHERE " + where + ")"
	}
	searchDialect := s.backend.Capabilities().SearchDialect
	pattern := searchDialect.contentSearchPattern(literal)
	var branches []string
	var args []any
	addArgs := func() {
		if !matchAll {
			args = append(args, pattern)
		}
		args = append(args, scopeArgs...)
	}
	predicate := func(column string) string {
		if matchAll {
			return column + " IS NOT NULL"
		}
		return searchDialect.contentSearchPredicate(column)
	}
	if hasSource(filter, "messages") {
		system := "1=1"
		if filter.ExcludeSystem {
			system = "message.is_system = FALSE AND " +
				s.bunContentSystemPrefixSQL("message.content", "message.role")
		}
		branches = append(branches, fmt.Sprintf(`
			SELECT message.session_id, message.ordinal, 'message' AS location,
				'' AS tool_name, message.content AS body,
				CAST(message.timestamp AS VARCHAR) AS source_timestamp,
				-1 AS call_index, -1 AS event_index,
				COALESCE(session.ended_at, session.started_at, session.created_at) AS sort_ts,
				0 AS source_order, COALESCE(message.id, 0) AS row_order
			FROM messages AS message
			JOIN sessions AS session ON session.id = message.session_id
			WHERE %s AND %s AND %s`, predicate("message.content"),
			system, scope("message.session_id")))
		addArgs()
	}
	if hasSource(filter, "tool_input") {
		branches = append(branches, fmt.Sprintf(`
			SELECT call.session_id, call.message_ordinal AS ordinal,
				'tool_input' AS location, call.tool_name,
				COALESCE(call.input_json, '') AS body,
				CAST(NULL AS VARCHAR) AS source_timestamp,
				call.call_index, -1 AS event_index,
				COALESCE(session.ended_at, session.started_at, session.created_at) AS sort_ts,
				1 AS source_order, COALESCE(call.id, 0) AS row_order
			FROM tool_calls AS call
			JOIN sessions AS session ON session.id = call.session_id
			WHERE %s AND %s`, predicate("call.input_json"),
			scope("call.session_id")))
		addArgs()
	}
	if hasSource(filter, "tool_result") {
		branches = append(branches, fmt.Sprintf(`
			SELECT call.session_id, call.message_ordinal AS ordinal,
				'tool_result' AS location, call.tool_name,
				COALESCE(call.result_content, '') AS body,
				CAST(NULL AS VARCHAR) AS source_timestamp,
				call.call_index, -1 AS event_index,
				COALESCE(session.ended_at, session.started_at, session.created_at) AS sort_ts,
				2 AS source_order, COALESCE(call.id, 0) AS row_order
			FROM tool_calls AS call
			JOIN sessions AS session ON session.id = call.session_id
			WHERE %s
				AND NOT EXISTS (
					SELECT 1 FROM tool_result_events AS event
					WHERE event.session_id = call.session_id
						AND event.tool_call_message_ordinal = call.message_ordinal
						AND event.call_index = call.call_index
						AND call.tool_use_id <> ''
				)
				AND %s`, predicate("call.result_content"), scope("call.session_id")))
		addArgs()
		branches = append(branches, fmt.Sprintf(`
			SELECT event.session_id,
				event.tool_call_message_ordinal AS ordinal,
				'tool_result' AS location, COALESCE(call.tool_name, '') AS tool_name,
				event.content AS body,
				CAST(event.timestamp AS VARCHAR) AS source_timestamp,
				event.call_index, event.event_index,
				COALESCE(session.ended_at, session.started_at, session.created_at) AS sort_ts,
				3 AS source_order, COALESCE(event.id, 0) AS row_order
			FROM tool_result_events AS event
			JOIN sessions AS session ON session.id = event.session_id
			LEFT JOIN tool_calls AS call
				ON call.session_id = event.session_id
				AND call.message_ordinal = event.tool_call_message_ordinal
				AND call.call_index = event.call_index
			WHERE %s AND %s`, predicate("event.content"),
			scope("event.session_id")))
		addArgs()
	}
	if len(branches) == 0 {
		return "", nil
	}
	orderTimestamp := s.backend.TimestampOrderExpr("sort_ts")
	query := `SELECT session_id, ordinal, location, tool_name, body,
		source_timestamp,
		call_index, event_index FROM (` + strings.Join(branches, " UNION ALL ") + `)
		ORDER BY ` + orderTimestamp + ` DESC NULLS LAST, session_id ASC,
			ordinal ASC, source_order ASC, call_index ASC, event_index ASC,
			row_order ASC`
	if paginate {
		query += "\n\t\tLIMIT ? OFFSET ?"
		args = append(args, filter.Limit, filter.Cursor)
	}
	return query, args
}

func bunContentHitFromCandidate(
	row bunContentCandidate, snippet string,
) ContentSearchHit {
	callIndex := row.CallIndex
	eventIndex := row.EventIndex
	hit := ContentSearchHit{
		SessionID: row.SessionID, Ordinal: row.Ordinal,
		OrdinalStart: row.Ordinal, OrdinalEnd: row.Ordinal,
		Location: row.Location, ToolName: row.ToolName,
		Snippet:   snippet,
		Timestamp: normalizeBunContentTimestamp(row.Timestamp),
	}
	if row.CallIndex >= 0 {
		hit.CallIndex = &callIndex
	}
	if row.EventIndex >= 0 {
		hit.EventIndex = &eventIndex
	}
	return hit
}

func normalizeBunContentTimestamp(value *bunmodel.Timestamp) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return requiredTimestampFromBunRow(*value)
}

func (s *BunStore) bunContentSystemPrefixSQL(content, role string) string {
	return s.backend.Capabilities().SearchDialect.systemPrefixPredicate(content, role)
}

func bunContentMessagesForHits(
	ctx context.Context, store bun.IDB, hits []ContentSearchHit,
	backend BunBackend,
) (map[bunContentMessageKey]bunContentMessage, error) {
	out := make(map[bunContentMessageKey]bunContentMessage, len(hits))
	const chunkSize = 400
	for start := 0; start < len(hits); start += chunkSize {
		chunk := hits[start:min(start+chunkSize, len(hits))]
		values := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*2)
		for i, hit := range chunk {
			values[i] = "(?, ?)"
			args = append(args, hit.SessionID, hit.Ordinal)
		}
		var rows []bunContentMessage
		timestampColumn := "message.timestamp"
		query := `WITH refs(session_id, ordinal) AS (VALUES ` +
			strings.Join(values, ", ") + `)
			SELECT message.session_id, message.ordinal, message.role,
				message.content, ` + timestampColumn + ` AS timestamp, message.is_system,
				message.is_sidechain
			FROM refs
			JOIN messages AS message
				ON message.session_id = refs.session_id
				AND message.ordinal = refs.ordinal`
		if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
			return nil, fmt.Errorf("hydrating content search messages: %w", err)
		}
		for index := range rows {
			timestamp, err := bunAvailableTimestamp(rows[index].RawTimestamp)
			if err != nil {
				return nil, fmt.Errorf(
					"scanning content search message timestamp: %w", err,
				)
			}
			rows[index].Timestamp = timestamp
			row := rows[index]
			out[bunContentMessageKey{row.SessionID, row.Ordinal}] = row
		}
	}
	return out, nil
}
