//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawparity"
)

// These use real publication, not public served readers. Every relationship
// family must obey the same physical source authority, including shared owners.
func TestRawParityPhysicalRelationships(t *testing.T) {
	for _, mode := range []string{"same_source", "fallback", "trashed", "missing", "excluded", "cross_device", "other_root", "other_provider", "plural", "equal_cohort", "shared_missing", "shared_plural", "prefix_owner", "historical_exact"} {
		t.Run(mode, func(t *testing.T) {
			f := newProjectionFixture(t)
			child := linkChildOutcome()
			call := &child.Outcome.Results[0].Result.Messages[1].ToolCalls[0]
			call.SubagentSessionID = "codex:portable"
			call.ResultEvents = []parser.ParsedToolResultEvent{{SubagentSessionID: "codex:portable", Source: "tool_result", Status: "completed", Content: "done"}}
			own, accepted := f.acceptScoped(t, "device-a", "own", "", parser.AgentCodex, "root-a", "own.jsonl")
			combined := child
			combined.Outcome.Results = append(combined.Outcome.Results, projectionOutcome("parent").Outcome.Results...)
			initial := combined
			fallback := mode == "cross_device" || mode == "other_root" || mode == "other_provider" || mode == "fallback" || mode == "plural" || mode == "equal_cohort"
			if mode == "missing" || fallback {
				initial = child
			}
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, own), own, initial))
			switch mode {
			case "trashed":
				require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
			case "excluded":
				require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
				removed, err := f.sink.ExcludeTrashedSession(t.Context(), "codex:portable")
				require.NoError(t, err)
				require.True(t, removed)
			case "historical_exact":
				next, _ := f.acceptScoped(t, "device-a", "removed", accepted.Receipt, parser.AgentCodex, "root-a", "own.jsonl")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, child))
			case "shared_plural", "prefix_owner":
				other, _ := f.acceptScoped(t, "device-b", "shared", "", parser.AgentCodex, "root-a", "shared.jsonl")
				second := linkChildOutcome()
				if mode == "prefix_owner" {
					second.Outcome.Results[0].Result.Messages = second.Outcome.Results[0].Result.Messages[:1]
				} else {
					second = child
				}
				text := "parent"
				if mode == "shared_plural" {
					text = "different parent"
				}
				second.Outcome.Results = append(second.Outcome.Results, projectionOutcome(text).Outcome.Results...)
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, other), other, second))
			case "shared_missing":
				other, _ := f.acceptScoped(t, "device-b", "shared", "", parser.AgentCodex, "root-a", "shared.jsonl")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, other), other, child))
			}
			if fallback || mode == "historical_exact" || mode == "excluded" {
				device := "device-a"
				if mode == "cross_device" {
					device = "device-b"
				}
				root, provider := "root-a", parser.AgentCodex
				if mode == "other_root" {
					root = "root-b"
				}
				if mode == "other_provider" {
					provider = parser.AgentClaude
				}
				other, _ := f.acceptScoped(t, device, "other", "", provider, root, "other.jsonl")
				p := projectionOutcome("parent")
				p.Outcome.Results[0].Result.Session.Agent = provider
				if mode == "historical_exact" || mode == "excluded" {
					p.Outcome.Results[0].Result.Session.SourceSessionID = "different-group"
				}
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, other), other, p))
				if mode == "plural" || mode == "equal_cohort" {
					another, _ := f.acceptScoped(t, "device-a", "another", "", parser.AgentCodex, "root-a", "another.jsonl")
					text := "parent"
					if mode == "plural" {
						text = "different parent"
					}
					require.NoError(t, f.sink.Project(t.Context(), f.lease(t, another), another, projectionOutcome(text)))
				}
			}
			require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
			tx := parityTx(t, f.runtime)
			var id string
			var children int
			require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(DISTINCT session_id),min(session_id) FROM raw_session_branches WHERE member_id='codex:child' AND active`).Scan(&children, &id))
			require.Equal(t, 1, children)
			links, err := LoadRawParityLinks(t.Context(), tx, id)
			require.NoError(t, err)
			require.Len(t, links, 3)
			ok := mode == "same_source" || mode == "fallback" || mode == "trashed" || mode == "equal_cohort" || mode == "prefix_owner"
			targets := []string{}
			if ok || mode == "shared_missing" || mode == "shared_plural" {
				targetRows, err := tx.QueryContext(t.Context(), `SELECT DISTINCT session_id FROM raw_session_branches WHERE member_id='codex:portable' AND active ORDER BY session_id`)
				require.NoError(t, err)
				for targetRows.Next() {
					var target string
					require.NoError(t, targetRows.Scan(&target))
					targets = append(targets, target)
				}
				require.NoError(t, targetRows.Err())
				require.NoError(t, targetRows.Close())
				count := 1
				if mode == "shared_plural" {
					count = 2
				}
				require.Len(t, targets, count)
			}
			missing := !ok && mode != "shared_plural"
			assert.Equal(t, []rawparity.Link{
				{Kind: "call", Ordinal: 1, CallIndex: 0, EventIndex: -1, Targets: targets, Missing: missing},
				{Kind: "event", Ordinal: 1, CallIndex: 0, EventIndex: 0, Targets: targets, Missing: missing},
				{Kind: "parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Targets: targets, Missing: missing},
			}, links)
			snapshots, err := LoadRawParitySnapshots(t.Context(), tx, []string{id})
			require.NoError(t, err)
			snapshot := snapshots[id]
			require.Equal(t, new("codex:portable"), snapshot.Session.ParentSessionID)
			// Same literal links and content on both sides isolate the prior false pass.
			// Only the target's unique physical match can justify equal meaning.
			matches := map[string]string{}
			if len(targets) == 1 {
				matches[targets[0]] = "codex:portable"
			}
			compared, err := rawparity.CompareResolved(snapshot, snapshot, links, matches)
			require.NoError(t, err)
			if ok {
				assert.Empty(t, compared.Differing)
			} else {
				assert.Equal(t, []string{"relationships"}, compared.Differing)
			}

			// A later committed removal cannot change this report's pinned snapshot.
			if mode == "same_source" {
				_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_session_branches SET active=false WHERE member_id='codex:portable'`)
				require.NoError(t, err)
				again, err := LoadRawParityLinks(t.Context(), tx, id)
				require.NoError(t, err)
				assert.Equal(t, links, again)
				fresh, err := LoadRawParityLinks(t.Context(), parityTx(t, f.runtime), id)
				require.NoError(t, err)
				require.Len(t, fresh, 3)
				for _, link := range fresh {
					assert.True(t, link.Missing)
				}
			}
		})
	}
}

func TestRawParityLinksRequireSnapshot(t *testing.T) {
	_, err := LoadRawParityLinks(t.Context(), nil, "anything")
	assert.ErrorContains(t, err, "REPEATABLE READ READ ONLY")
	f := newProjectionFixture(t)
	for _, options := range []sql.TxOptions{{Isolation: sql.LevelReadCommitted, ReadOnly: true}, {Isolation: sql.LevelRepeatableRead}} {
		tx, err := f.runtime.BeginTx(t.Context(), &options)
		require.NoError(t, err)
		_, err = LoadRawParityLinks(t.Context(), tx, "anything")
		assert.ErrorContains(t, err, "REPEATABLE READ READ ONLY")
		require.NoError(t, tx.Rollback())
	}
}

func TestRawParityAnchoredRelationships(t *testing.T) {
	f := newProjectionFixture(t)
	own, accepted := f.acceptScoped(t, "device-a", "own", "", parser.AgentCodex, "root-a", "own.jsonl")
	combined := linkChildOutcome()
	combined.Outcome.Results = append(combined.Outcome.Results, projectionOutcome("parent").Outcome.Results...)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, own), own, combined))
	var alias, target string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT a.alias_id,b.session_id FROM raw_session_public_aliases a JOIN raw_session_branches b ON a.anchor_branch=b.branch_id WHERE b.source_id=$1 AND b.member_id='codex:portable'`, rawSourceID(own)).Scan(&alias, &target))
	child := &combined.Outcome.Results[0].Result
	child.Session.ParentSessionID = alias
	child.Messages[1].ToolCalls[0].SubagentSessionID = alias
	child.Messages[1].ToolCalls[0].ResultEvents = []parser.ParsedToolResultEvent{{SubagentSessionID: alias, Source: "tool_result", Status: "completed", Content: "done"}}
	next, _ := f.acceptScoped(t, "device-a", "anchored", accepted.Receipt, parser.AgentCodex, "root-a", "own.jsonl")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, combined))
	other, _ := f.acceptScoped(t, "device-b", "variant", "", parser.AgentCodex, "root-a", "other.jsonl")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, other), other, projectionOutcome("different parent")))
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	tx := parityTx(t, f.runtime)
	var childID string
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT session_id FROM raw_session_branches WHERE source_id=$1 AND member_id='codex:child' AND active`, rawSourceID(own)).Scan(&childID))
	links, err := LoadRawParityLinks(t.Context(), tx, childID)
	require.NoError(t, err)
	require.Len(t, links, 3)
	for _, link := range links {
		assert.False(t, link.Missing)
		assert.Equal(t, []string{target}, link.Targets)
	}
}
