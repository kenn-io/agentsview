package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ErrAgentRemapRulesChanged is returned by ApplyAgentRemapRules when the
// rule set or the matching sessions changed between the caller's preview and
// the apply attempt. Mirrors ErrWorktreeMappingSetChanged.
var ErrAgentRemapRulesChanged = errors.New(
	"agent remap preview changed",
)

// AgentRemapRule is one user-defined reclassification rule: sessions whose
// agent matches SourceAgent and whose message models or ID match the optional
// pattern fields are relabeled onto TargetAgent. The parser phase is never
// involved; this rewrites the archived agent column only.
type AgentRemapRule struct {
	ID          int64  `json:"id"`
	SourceAgent string `json:"source_agent"`
	ModelGlob   string `json:"model_glob"`
	IDPrefix    string `json:"id_prefix"`
	TargetAgent string `json:"target_agent"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// AgentRemapSample is one affected session shown in a preview.
type AgentRemapSample struct {
	ID           string `json:"id"`
	CurrentAgent string `json:"current_agent"`
	NextAgent    string `json:"next_agent"`
	StartedAt    string `json:"started_at"`
}

// AgentRemapPreview is the projected effect of the current enabled rule set.
// Token is a consistency token: apply rejects the operation unless the caller
// echoes the token from the preview it reviewed.
type AgentRemapPreview struct {
	Token           string             `json:"token"`
	MatchedSessions int                `json:"matched_sessions"`
	PerRuleCounts   map[string]int     `json:"per_rule_counts"`
	Samples         []AgentRemapSample `json:"samples"`
}

const agentRemapSampleLimit = 10

// normalizeAgentRemapRule validates and trims a rule draft.
func normalizeAgentRemapRule(rule AgentRemapRule) (AgentRemapRule, error) {
	rule.SourceAgent = strings.TrimSpace(rule.SourceAgent)
	rule.TargetAgent = strings.TrimSpace(rule.TargetAgent)
	rule.ModelGlob = strings.TrimSpace(rule.ModelGlob)
	rule.IDPrefix = strings.TrimSpace(rule.IDPrefix)
	if rule.SourceAgent == "" {
		return AgentRemapRule{}, fmt.Errorf("source_agent is required")
	}
	if rule.TargetAgent == "" {
		return AgentRemapRule{}, fmt.Errorf("target_agent is required")
	}
	if rule.SourceAgent == rule.TargetAgent {
		return AgentRemapRule{}, fmt.Errorf(
			"source_agent and target_agent must differ",
		)
	}
	return rule, nil
}

// agentRemapGlobRegexp converts a SQLite GLOB-style pattern into an anchored
// regexp. GLOB semantics: * matches any run, ? matches one character, [...]
// character classes with ! negation, case-sensitive.
func agentRemapGlobRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		switch c := glob[i]; c {
		case '*':
			b.WriteString(".*")
			i++
		case '?':
			b.WriteString(".")
			i++
		case '[':
			end := strings.IndexByte(glob[i+1:], ']')
			if end < 0 {
				// Unterminated class: literal bracket.
				b.WriteString(`\[`)
				i++
				continue
			}
			body := glob[i+1 : i+1+end]
			if body == "" || body == "^" || body == "!" {
				b.WriteString(`\[`)
				i++
				continue
			}
			negated := strings.HasPrefix(body, "^") ||
				strings.HasPrefix(body, "!")
			if negated {
				body = body[1:]
			}
			// Escape backslashes inside the class so path separators stay
			// literal, matching SQLite's treatment of GLOB classes.
			body = strings.ReplaceAll(body, `\`, `\\`)
			if negated {
				b.WriteString("[^")
			} else {
				b.WriteString("[")
			}
			b.WriteString(body)
			b.WriteString("]")
			i = i + end + 2
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// agentRemapGlobPatterns splits a rule's ModelGlob into individual GLOB
// patterns. "|" separates alternatives (e.g. "ossington-*|rosedale-*")
// because SQLite GLOB has no alternation; the Go matcher mirrors this split
// so both paths agree.
func agentRemapGlobPatterns(modelGlob string) []string {
	if modelGlob == "" {
		return nil
	}
	parts := strings.Split(modelGlob, "|")
	patterns := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			patterns = append(patterns, part)
		}
	}
	return patterns
}

// agentRemapGlobMatches reports whether any model matches any of the rule's
// GLOB patterns. An empty pattern set matches regardless of models.
func agentRemapGlobMatches(glob string, models []string) bool {
	patterns := agentRemapGlobPatterns(glob)
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		re, err := agentRemapGlobRegexp(pattern)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(models, re.MatchString) {
			return true
		}
	}
	return false
}

// AgentRemapTarget returns the remapped agent for sess under the first
// matching enabled rule (rules evaluated in ascending ID order), or the
// original agent. models is the session's distinct non-empty message models.
// A rule's ModelGlob matches when ANY model matches GLOB; an empty ModelGlob
// matches regardless of models. An empty IDPrefix matches regardless of ID.
func AgentRemapTarget(
	rules []AgentRemapRule, sess Session, models []string,
) string {
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if rule.SourceAgent != sess.Agent {
			continue
		}
		if rule.IDPrefix != "" && !strings.HasPrefix(sess.ID, rule.IDPrefix) {
			continue
		}
		if !agentRemapGlobMatches(rule.ModelGlob, models) {
			continue
		}
		return rule.TargetAgent
	}
	return sess.Agent
}

func scanAgentRemapRules(rows *sql.Rows) ([]AgentRemapRule, error) {
	var out []AgentRemapRule
	for rows.Next() {
		var (
			r       AgentRemapRule
			enabled int
		)
		if err := rows.Scan(
			&r.ID, &r.SourceAgent, &r.ModelGlob, &r.IDPrefix,
			&r.TargetAgent, &enabled, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning agent remap rule: %w", err)
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating agent remap rules: %w", err)
	}
	return out, nil
}

const agentRemapRuleColumns = `id, source_agent, model_glob, id_prefix,
	target_agent, enabled, created_at, updated_at`

// ListAgentRemapRules returns every rule ordered by id.
func (db *DB) ListAgentRemapRules(
	ctx context.Context,
) ([]AgentRemapRule, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT `+agentRemapRuleColumns+` FROM agent_remap_rules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing agent remap rules: %w", err)
	}
	defer rows.Close()
	return scanAgentRemapRules(rows)
}

func (db *DB) getAgentRemapRuleTx(
	ctx context.Context, tx *sql.Tx, id int64,
) (AgentRemapRule, error) {
	var r AgentRemapRule
	var enabled int
	err := tx.QueryRowContext(ctx,
		`SELECT `+agentRemapRuleColumns+` FROM agent_remap_rules WHERE id = ?`,
		id,
	).Scan(
		&r.ID, &r.SourceAgent, &r.ModelGlob, &r.IDPrefix,
		&r.TargetAgent, &enabled, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentRemapRule{}, sql.ErrNoRows
	}
	if err != nil {
		return AgentRemapRule{}, fmt.Errorf(
			"reading agent remap rule: %w", err,
		)
	}
	r.Enabled = enabled != 0
	return r, nil
}

func (db *DB) upsertAgentRemapRuleTx(
	ctx context.Context, tx *sql.Tx, rule AgentRemapRule,
) (AgentRemapRule, error) {
	normalized, err := normalizeAgentRemapRule(rule)
	if err != nil {
		return AgentRemapRule{}, err
	}
	// RETURNING id instead of LastInsertId: on the ON CONFLICT update
	// branch SQLite leaves last_insert_rowid pointing at whatever row was
	// inserted most recently, so an update after any other insert would
	// read an unrelated rule's ID.
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO agent_remap_rules
			(source_agent, model_glob, id_prefix, target_agent, enabled)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(source_agent, model_glob, id_prefix) DO UPDATE SET
			target_agent = excluded.target_agent,
			enabled = excluded.enabled,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		RETURNING id`,
		normalized.SourceAgent, normalized.ModelGlob, normalized.IDPrefix,
		normalized.TargetAgent, boolInt(normalized.Enabled),
	).Scan(&id)
	if err != nil {
		return AgentRemapRule{}, fmt.Errorf(
			"upserting agent remap rule: %w", err,
		)
	}
	return db.getAgentRemapRuleTx(ctx, tx, id)
}

// CreateAgentRemapRule inserts a rule, or updates the existing rule with the
// same (source_agent, model_glob, id_prefix) identity.
func (db *DB) CreateAgentRemapRule(
	ctx context.Context, rule AgentRemapRule,
) (AgentRemapRule, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return AgentRemapRule{}, fmt.Errorf(
			"beginning agent remap rule create: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	created, err := db.upsertAgentRemapRuleTx(ctx, tx, rule)
	if err != nil {
		return AgentRemapRule{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentRemapRule{}, fmt.Errorf(
			"committing agent remap rule create: %w", err,
		)
	}
	return created, nil
}

// UpdateAgentRemapRule replaces an existing rule's fields by id.
func (db *DB) UpdateAgentRemapRule(
	ctx context.Context, rule AgentRemapRule,
) (AgentRemapRule, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := normalizeAgentRemapRule(rule)
	if err != nil {
		return AgentRemapRule{}, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return AgentRemapRule{}, fmt.Errorf(
			"beginning agent remap rule update: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_remap_rules
		SET source_agent = ?, model_glob = ?, id_prefix = ?,
			target_agent = ?, enabled = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`,
		normalized.SourceAgent, normalized.ModelGlob, normalized.IDPrefix,
		normalized.TargetAgent, boolInt(normalized.Enabled), rule.ID,
	)
	if err != nil {
		return AgentRemapRule{}, fmt.Errorf(
			"updating agent remap rule: %w", err,
		)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return AgentRemapRule{}, err
	}
	if changed == 0 {
		return AgentRemapRule{}, sql.ErrNoRows
	}
	updated, err := db.getAgentRemapRuleTx(ctx, tx, rule.ID)
	if err != nil {
		return AgentRemapRule{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentRemapRule{}, fmt.Errorf(
			"committing agent remap rule update: %w", err,
		)
	}
	return updated, nil
}

// DeleteAgentRemapRule removes a rule by id.
func (db *DB) DeleteAgentRemapRule(ctx context.Context, id int64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning agent remap rule delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM agent_remap_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting agent remap rule: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing agent remap rule delete: %w", err)
	}
	return nil
}

type agentRemapEvaluation struct {
	matched int
	matches []agentRemapSessionMatch
	samples []AgentRemapSample
	perRule map[string]int
}

type agentRemapSessionMatch struct {
	id           string
	currentAgent string
	nextAgent    string
}

// agentRemapModelGlobSQL appends the model-glob predicate for one rule to
// the candidate query. patterns is agentRemapGlobPatterns(rule.ModelGlob);
// an empty slice matches any session. Must stay in agreement with
// agentRemapGlobMatches; TestAgentRemapGlobSQLAgreement cross-checks both.
func agentRemapModelGlobSQL(patterns []string) (string, []any) {
	if len(patterns) == 0 {
		return "", nil
	}
	clauses := make([]string, len(patterns))
	args := make([]any, len(patterns))
	for i, pattern := range patterns {
		clauses[i] = "m.model GLOB ?"
		args[i] = pattern
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// evaluateAgentRemapTx finds sessions the enabled rules would remap. Rules
// are evaluated in ascending ID order; first match wins, and a session whose
// agent already equals the target is not counted.
func evaluateAgentRemapTx(
	ctx context.Context, tx *sql.Tx, rules []AgentRemapRule,
) (agentRemapEvaluation, error) {
	eval := agentRemapEvaluation{perRule: map[string]int{}}
	ordered := applyAgentRemapRuleOrder(rules)
	seen := map[string]bool{}
	for _, rule := range ordered {
		if !rule.Enabled {
			continue
		}
		query := `
			SELECT s.id, s.agent, COALESCE(s.started_at, '')
			FROM sessions s
			WHERE s.agent = ? AND s.deleted_at IS NULL`
		args := []any{rule.SourceAgent}
		globClauses, globArgs := agentRemapModelGlobSQL(
			agentRemapGlobPatterns(rule.ModelGlob),
		)
		if globClauses != "" {
			query += ` AND EXISTS (
				SELECT 1 FROM messages m
				WHERE m.session_id = s.id AND m.model <> ''
					AND ` + globClauses + `)`
			args = append(args, globArgs...)
		}
		if rule.IDPrefix != "" {
			query += ` AND substr(s.id, 1, ?) = ?`
			args = append(args, len(rule.IDPrefix), rule.IDPrefix)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return eval, fmt.Errorf(
				"evaluating agent remap rule %d: %w", rule.ID, err,
			)
		}
		var (
			ids    []string
			agents = map[string]string{}
			starts = map[string]string{}
		)
		for rows.Next() {
			var id, agent, started string
			if err := rows.Scan(&id, &agent, &started); err != nil {
				rows.Close()
				return eval, fmt.Errorf("scanning remap candidate: %w", err)
			}
			ids = append(ids, id)
			agents[id] = agent
			starts[id] = started
		}
		closeErr := rows.Err()
		rows.Close()
		if closeErr != nil {
			return eval, closeErr
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			next := rule.TargetAgent
			if agents[id] == next {
				continue
			}
			seen[id] = true
			eval.matches = append(eval.matches, agentRemapSessionMatch{
				id: id, currentAgent: agents[id], nextAgent: next,
			})
			eval.perRule[strconv.FormatInt(rule.ID, 10)]++
			eval.matched++
			if len(eval.samples) < agentRemapSampleLimit {
				eval.samples = append(eval.samples, AgentRemapSample{
					ID: id, CurrentAgent: agents[id], NextAgent: next,
					StartedAt: starts[id],
				})
			}
		}
	}
	sort.Slice(eval.matches, func(i, j int) bool {
		return eval.matches[i].id < eval.matches[j].id
	})
	return eval, nil
}

// agentRemapRulesToken builds the preview consistency token: a digest over
// the enabled rule set plus the current match set, mirroring the intent of
// worktreeReclassificationToken.
func agentRemapRulesToken(
	rules []AgentRemapRule, eval agentRemapEvaluation,
) string {
	h := sha256.New()
	for _, rule := range applyAgentRemapRuleOrder(rules) {
		if !rule.Enabled {
			continue
		}
		fmt.Fprintf(h, "rule:%d:%s:%s:%s:%s:%t\n",
			rule.ID, rule.SourceAgent, rule.ModelGlob, rule.IDPrefix,
			rule.TargetAgent, rule.Enabled)
	}
	for _, m := range eval.matches {
		fmt.Fprintf(h, "match:%s:%s:%s\n", m.id, m.currentAgent, m.nextAgent)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func loadAgentRemapRulesTx(
	ctx context.Context, tx *sql.Tx,
) ([]AgentRemapRule, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+agentRemapRuleColumns+
			` FROM agent_remap_rules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("loading agent remap rules: %w", err)
	}
	defer rows.Close()
	return scanAgentRemapRules(rows)
}

func agentRemapPreviewFromEval(
	token string, eval agentRemapEvaluation,
) AgentRemapPreview {
	return AgentRemapPreview{
		Token:           token,
		MatchedSessions: eval.matched,
		PerRuleCounts:   eval.perRule,
		Samples:         eval.samples,
	}
}

// PreviewAgentRemapRules evaluates the enabled rules in a read-only
// transaction and returns the projected rewrites plus a consistency token.
func (db *DB) PreviewAgentRemapRules(
	ctx context.Context,
) (AgentRemapPreview, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AgentRemapPreview{}, fmt.Errorf(
			"beginning agent remap preview: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	rules, err := loadAgentRemapRulesTx(ctx, tx)
	if err != nil {
		return AgentRemapPreview{}, err
	}
	eval, err := evaluateAgentRemapTx(ctx, tx, rules)
	if err != nil {
		return AgentRemapPreview{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentRemapPreview{}, fmt.Errorf(
			"committing agent remap preview: %w", err,
		)
	}
	return agentRemapPreviewFromEval(
		agentRemapRulesToken(rules, eval), eval,
	), nil
}

// applyAgentRemapMatchesTx rewrites each match's agent inside the caller's
// transaction, guarding on the current agent value. Returns the matched IDs
// whose row actually changed.
func applyAgentRemapMatchesTx(
	ctx context.Context, tx *sql.Tx, eval agentRemapEvaluation,
) ([]string, error) {
	matchedIDs := make([]string, 0, len(eval.matches))
	for _, m := range eval.matches {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET agent = ?,
				local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE id = ? AND agent = ? AND deleted_at IS NULL`,
			m.nextAgent, m.id, m.currentAgent,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"remapping session %s: %w", m.id, err,
			)
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed > 0 {
			matchedIDs = append(matchedIDs, m.id)
		}
	}
	return matchedIDs, nil
}

// ApplyAgentRemapRules re-evaluates the enabled rules under the write lock
// and rewrites sessions.agent for every match, unless the rule set or the
// match set changed since the caller's preview (ErrAgentRemapRulesChanged).
// Rewrites bump local_modified_at so mirrors pick the change up; the usage
// cache is notified after commit. Session IDs are never rewritten.
func (db *DB) ApplyAgentRemapRules(
	ctx context.Context, acceptedToken string,
) (AgentRemapPreview, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return AgentRemapPreview{}, fmt.Errorf(
			"beginning agent remap apply: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	rules, err := loadAgentRemapRulesTx(ctx, tx)
	if err != nil {
		return AgentRemapPreview{}, err
	}
	eval, err := evaluateAgentRemapTx(ctx, tx, rules)
	if err != nil {
		return AgentRemapPreview{}, err
	}
	if acceptedToken == "" || acceptedToken != agentRemapRulesToken(rules, eval) {
		return AgentRemapPreview{}, ErrAgentRemapRulesChanged
	}
	matchedIDs, err := applyAgentRemapMatchesTx(ctx, tx, eval)
	if err != nil {
		return AgentRemapPreview{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentRemapPreview{}, fmt.Errorf(
			"committing agent remap apply: %w", err,
		)
	}
	db.notifyUsageSessions(matchedIDs)
	preview := agentRemapPreviewFromEval(acceptedToken, eval)
	preview.MatchedSessions = len(matchedIDs)
	return preview, nil
}

// ApplyAgentRemapRulesFromSync re-applies the enabled rules without a token
// check, for full-rebuild contributors where the caller just rebuilt the
// world. Mirrors ApplyWorktreeProjectMappingsFromSync.
func (db *DB) ApplyAgentRemapRulesFromSync(
	ctx context.Context,
) (AgentRemapPreview, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return AgentRemapPreview{}, fmt.Errorf(
			"beginning agent remap sync apply: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	rules, err := loadAgentRemapRulesTx(ctx, tx)
	if err != nil {
		return AgentRemapPreview{}, err
	}
	eval, err := evaluateAgentRemapTx(ctx, tx, rules)
	if err != nil {
		return AgentRemapPreview{}, err
	}
	matchedIDs, err := applyAgentRemapMatchesTx(ctx, tx, eval)
	if err != nil {
		return AgentRemapPreview{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentRemapPreview{}, fmt.Errorf(
			"committing agent remap sync apply: %w", err,
		)
	}
	db.notifyUsageSessions(matchedIDs)
	return agentRemapPreviewFromEval("", eval), nil
}

// agentRemapSessionModels returns the distinct non-empty message models of
// one session, for the single-session incremental path.
func (db *DB) agentRemapSessionModels(
	ctx context.Context, sessionID string,
) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT DISTINCT model FROM messages
		 WHERE session_id = ? AND model <> ''`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("reading session models: %w", err)
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, rows.Err()
}

// ApplyAgentRemapRulesToSession evaluates the enabled rules against one
// freshly written session and rewrites its agent when a rule matches. It
// returns the (possibly unchanged) agent. Used by the incremental write
// path so new sessions land with rules applied.
func (db *DB) ApplyAgentRemapRulesToSession(
	ctx context.Context, sessionID string,
) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rules, err := db.ListAgentRemapRules(ctx)
	if err != nil {
		return "", err
	}
	hasEnabled := false
	for _, rule := range rules {
		if rule.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		return "", nil
	}
	sess, err := db.GetSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if sess == nil {
		return "", nil
	}
	models, err := db.agentRemapSessionModels(ctx, sessionID)
	if err != nil {
		return "", err
	}
	next := AgentRemapTarget(rules, *sess, models)
	if next == sess.Agent {
		return sess.Agent, nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf(
			"beginning single-session agent remap: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET agent = ?,
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ? AND agent = ? AND deleted_at IS NULL`,
		next, sessionID, sess.Agent,
	)
	if err != nil {
		return "", fmt.Errorf(
			"remapping session %s: %w", sessionID, err,
		)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf(
			"committing single-session agent remap: %w", err,
		)
	}
	if changed > 0 {
		db.notifyUsageSessions([]string{sessionID})
	}
	return next, nil
}

// applyAgentRemapRuleOrder sorts rules by ID for deterministic evaluation.
func applyAgentRemapRuleOrder(rules []AgentRemapRule) []AgentRemapRule {
	out := append([]AgentRemapRule(nil), rules...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ApplyAgentRemapRulesToSessions evaluates the enabled rules against a set
// of freshly written sessions and rewrites each matched session's agent.
// Used by the full-parse batch write path (WriteSessionBatch) so sessions
// that skip the incremental path still land with rules applied. Returns the
// IDs of sessions whose agent actually changed.
func (db *DB) ApplyAgentRemapRulesToSessions(
	ctx context.Context, sessionIDs []string,
) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	rules, err := db.ListAgentRemapRules(ctx)
	if err != nil {
		return nil, err
	}
	hasEnabled := false
	for _, rule := range rules {
		if rule.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		return nil, nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf(
			"beginning batch agent remap: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	var matched []string
	seen := make(map[string]bool, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		if seen[sessionID] {
			continue
		}
		seen[sessionID] = true
		sess, err := getSessionForRemapTx(ctx, tx, sessionID)
		if err != nil {
			return nil, err
		}
		if sess == nil {
			continue
		}
		models, err := sessionModelsForRemapTx(ctx, tx, sessionID)
		if err != nil {
			return nil, err
		}
		next := AgentRemapTarget(rules, *sess, models)
		if next == sess.Agent {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET agent = ?,
				local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE id = ? AND agent = ? AND deleted_at IS NULL`,
			next, sessionID, sess.Agent,
		); err != nil {
			return nil, fmt.Errorf(
				"remapping session %s: %w", sessionID, err,
			)
		}
		matched = append(matched, sessionID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf(
			"committing batch agent remap: %w", err,
		)
	}
	if len(matched) > 0 {
		db.notifyUsageSessions(matched)
	}
	return matched, nil
}

// getSessionForRemapTx reads one session's identity fields inside the
// caller's transaction.
func getSessionForRemapTx(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (*Session, error) {
	var (
		sess    Session
		started sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, agent, COALESCE(started_at, '')
		FROM sessions WHERE id = ? AND deleted_at IS NULL`,
		sessionID,
	).Scan(&sess.ID, &sess.Agent, &started)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading session %s for remap: %w", sessionID, err)
	}
	if started.Valid {
		sess.StartedAt = &started.String
	}
	return &sess, nil
}

// sessionModelsForRemapTx reads a session's distinct non-empty models
// inside the caller's transaction.
func sessionModelsForRemapTx(
	ctx context.Context, tx *sql.Tx, sessionID string,
) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT model FROM messages
		 WHERE session_id = ? AND model <> ''`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("reading session models: %w", err)
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, rows.Err()
}
