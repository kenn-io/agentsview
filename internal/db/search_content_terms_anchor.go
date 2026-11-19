package db

import "strings"

// termsAnchorTerm returns the first term the substring index lists, which
// anchors a terms search on the messages that hold it.
func termsAnchorTerm(terms []string, indexed map[string]string) (string, bool) {
	for _, term := range terms {
		if _, ok := indexed[term]; ok {
			return term, true
		}
	}
	return "", false
}

// buildTermsAnchorSQL renders the SQLite terms query for a search where the
// substring index lists anchor. It returns the same rows as
// buildTermsSearchSQL without numbering every message of every matching
// session: an exchange holding every term holds one of the messages the
// index lists for anchor, so the query starts from those messages.
//
// An exchange is the run of eligible messages on one session and sidechain
// branch from a user message up to the next one. The query finds each listed
// message's exchange from the nearest eligible user message at or before it
// and the next one after that, checks the other terms against the
// exchange's messages, and builds bodies only for the page. A term holds no
// whitespace, so an exchange's joined body contains a term exactly when one
// of its messages does.
func buildTermsAnchorSQL(
	f ContentSearchFilter, terms []string, anchor, anchorQuery string,
) (string, []any) {
	dialect := SQLiteQueryDialect()
	frag := termsSQLByDialect["sqlite"]
	where, scopeArgs := termsScopeSQL(f, dialect)
	b := NewQueryBuilder(dialect, len(scopeArgs))

	text := func(alias string) string {
		return "COALESCE(" + alias + ".dialogue_text, " + alias + ".content)"
	}
	// member and turn test role first, so rows of other roles are rejected
	// before SQLite reads the columns stored after the message body.
	member := func(alias string) string {
		return alias + ".role IN ('user','assistant') AND " + alias + ".is_system = 0 AND " +
			frag.systemPrefix(alias+".content", alias+".role")
	}
	turn := func(alias string) string {
		return alias + ".role = 'user' AND " + alias + ".is_system = 0 AND " +
			frag.systemPrefix(alias+".content", alias+".role")
	}
	within := func(alias, exchange string) string {
		return alias + ".session_id = " + exchange + ".session_id" +
			" AND " + alias + ".ordinal >= " + exchange + ".start_ordinal" +
			" AND " + alias + ".ordinal < COALESCE(" + exchange + ".next_start, 9223372036854775807)" +
			" AND " + alias + ".is_sidechain = " + exchange + ".is_sidechain"
	}

	// Placeholders are added in the order they appear in the SQL text.
	hitSQL := `SELECT DISTINCT m.session_id, m.is_sidechain, m.ordinal
			FROM messages m
			WHERE m.id IN (SELECT rowid FROM message_trigram_fts WHERE message_trigram_fts MATCH ` +
		b.Add(anchorQuery) + `)
			  AND m.session_id IN (SELECT id FROM scoped)
			  AND ` + member("m") + `
			  AND ` + b.ContainsPredicate(text("m"), anchor)
	predicates := make([]string, 0, len(terms))
	for _, term := range terms {
		if term == anchor {
			continue
		}
		predicates = append(predicates, "EXISTS (SELECT 1 FROM messages x WHERE "+
			within("x", "c")+" AND "+member("x")+" AND "+
			b.ContainsPredicate(text("x"), term)+")")
	}
	switch f.Scope {
	case "top":
		predicates = append(predicates, "c.subordinate = "+dialect.falseLiteral)
	case "subordinate":
		predicates = append(predicates, "c.subordinate = "+dialect.trueLiteral)
	}
	if len(predicates) == 0 {
		predicates = append(predicates, "1=1")
	}
	order := "subordinate ASC, julianday(sort_ts) DESC, session_id ASC, start_ordinal ASC"

	query := `
		WITH scoped AS (
			SELECT id FROM sessions WHERE ` + where + `
		), hit AS (
			` + hitSQL + `
		), started AS (
			SELECT DISTINCT h.session_id, h.is_sidechain, (
				SELECT u.ordinal FROM messages u
				WHERE u.session_id = h.session_id AND u.ordinal <= h.ordinal
				  AND u.is_sidechain = h.is_sidechain AND ` + turn("u") + `
				ORDER BY u.ordinal DESC LIMIT 1
			) AS start_ordinal
			FROM hit h
		), candidates AS (
			SELECT st.session_id, st.is_sidechain, st.start_ordinal, (
				SELECT u.ordinal FROM messages u
				WHERE u.session_id = st.session_id AND u.ordinal > st.start_ordinal
				  AND u.is_sidechain = st.is_sidechain AND ` + turn("u") + `
				ORDER BY u.ordinal LIMIT 1
			) AS next_start,
				s.project, s.agent, s.machine,
				COALESCE(s.display_name, s.session_name) AS display_name,
				s.transcript_revision AS transcript_revision,
				s.relationship_type AS relationship_type,
				COALESCE(s.parent_session_id,'') AS parent_session_id,
				` + frag.sessionSort + ` AS sort_ts,
				CASE WHEN st.is_sidechain = ` + dialect.trueLiteral + ` OR ` + SubordinateSessionSQL("s") + `
					THEN ` + dialect.trueLiteral + ` ELSE ` + dialect.falseLiteral + ` END AS subordinate
			FROM started st
			JOIN sessions s ON s.id = st.session_id
			WHERE st.start_ordinal IS NOT NULL
		), page AS (
			SELECT * FROM candidates c
			WHERE ` + strings.Join(predicates, " AND ") + `
			ORDER BY ` + order + `
			LIMIT ` + b.Add(f.Limit+1) + ` OFFSET ` + b.Add(f.Cursor) + `
		)
		SELECT p.session_id, p.project, p.agent, p.transcript_revision, 'message', 'user',
			p.start_ordinal, MIN(` + frag.timestamp + `),
			GROUP_CONCAT(` + text("m") + `, char(10) ORDER BY m.ordinal), MAX(m.ordinal),
			p.relationship_type, p.parent_session_id, p.is_sidechain, p.subordinate,
			p.machine, p.display_name
		FROM page p
		JOIN messages m ON ` + within("m", "p") + ` AND ` + member("m") + `
		GROUP BY p.session_id, p.start_ordinal
		ORDER BY p.subordinate ASC, julianday(p.sort_ts) DESC, p.session_id ASC, p.start_ordinal ASC`
	return query, append(scopeArgs, b.Args()...)
}
