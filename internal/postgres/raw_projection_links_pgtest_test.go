//go:build pgtest

package postgres

import (
	"fmt"
	"go.kenn.io/agentsview/internal/db"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func linkChildOutcome() rawderive.ParsedManifest {
	p := projectionOutcome("child")
	p.Outcome.Results[0].Result.Session.ID = "codex:child"
	p.Outcome.Results[0].Result.Session.SourceSessionID = "child"
	p.Outcome.Results[0].Result.Session.ParentSessionID = "codex:portable"
	p.Outcome.Results[0].Result.Session.RelationshipType = "subagent"
	p.Outcome.Results[0].Result.Session.EndedAt = p.Outcome.Results[0].Result.Session.StartedAt.Add(7 * time.Second)
	return p
}

// The fallback is bounded to the immutable capture scope, and counts content
// cohorts rather than duplicate proofs. Public reads must not choose a variant.
func TestHostedCrossSourceRelationshipScope(t *testing.T) {
	for _, mode := range []string{"unique", "equal_cohort", "ambiguous", "other_device", "other_root", "other_provider"} {
		t.Run(mode, func(t *testing.T) {
			f := newProjectionFixture(t)
			child, _ := f.acceptScoped(t, "device-a", "child", "", parser.AgentCodex, "root-a", "child.jsonl")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, child), child, linkChildOutcome()))
			device, root, provider := "device-a", "root-a", parser.AgentCodex
			switch mode {
			case "other_device":
				device = "device-b"
			case "other_root":
				root = "root-b"
			case "other_provider":
				provider = parser.AgentClaude
			}
			parent, _ := f.acceptScoped(t, device, "parent", "", provider, root, "parent.jsonl")
			p := projectionOutcome("parent")
			p.Outcome.Results[0].Result.Session.Agent = provider
			if mode == "unique" {
				call := &p.Outcome.Results[0].Result.Messages[1].ToolCalls[0]
				call.SubagentSessionID = "codex:child"
				call.ResultEvents = []parser.ParsedToolResultEvent{{ToolUseID: "call-1", SubagentSessionID: "codex:child", Source: "tool_result", Status: "completed", Content: "child complete"}}
				p.Outcome.Results[0].Result.Messages[1].HasToolUse = true
			}
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, parent), parent, p))
			if mode == "equal_cohort" || mode == "ambiguous" {
				second, _ := f.acceptScoped(t, "device-a", "second", "", parser.AgentCodex, "root-a", "second.jsonl")
				text := "parent"
				if mode == "ambiguous" {
					text = "different parent"
				}
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, second), second, projectionOutcome(text)))
			}
			h, err := newHostedAdapter(f.runtime, f.tenant)
			require.NoError(t, err)
			got, err := h.GetSessionFull(t.Context(), "codex:child")
			require.NoError(t, err)
			require.NotNil(t, got)
			if mode == "unique" || mode == "equal_cohort" {
				require.NotNil(t, got.ParentSessionID)
				assert.Equal(t, "codex:portable", *got.ParentSessionID)
				children, err := h.GetChildSessions(t.Context(), "codex:portable")
				require.NoError(t, err)
				require.Len(t, children, 1)
				assert.Equal(t, "codex:child", children[0].ID)
				if mode == "unique" {
					messages, err := h.GetAllMessages(t.Context(), "codex:portable")
					require.NoError(t, err)
					require.Len(t, messages, 2)
					require.Len(t, messages[1].ToolCalls, 1)
					call := messages[1].ToolCalls[0]
					assert.Equal(t, "codex:child", call.SubagentSessionID)
					require.Len(t, call.ResultEvents, 1)
					assert.Equal(t, "codex:child", call.ResultEvents[0].SubagentSessionID)
					timing, err := h.GetSessionTiming(t.Context(), "codex:portable")
					require.NoError(t, err)
					require.NotNil(t, timing.SlowestCall)
					require.NotNil(t, timing.SlowestCall.DurationMs)
					assert.Equal(t, int64(7000), *timing.SlowestCall.DurationMs)
					sidebar, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: 10})
					require.NoError(t, err)
					assert.Equal(t, 1, sidebar.Total)
					require.Len(t, sidebar.Sessions, 2)
				}
			} else {
				assert.Nil(t, got.ParentSessionID)
				assert.Empty(t, got.ParentSessionIDs)
			}
		})
	}
}

func TestHostedCrossSourceHistoricalExactAuthority(t *testing.T) {
	for _, mode := range []string{"removed", "excluded"} {
		t.Run(mode, func(t *testing.T) {
			f := newProjectionFixture(t)
			own, accepted := f.acceptScoped(t, "device-a", "own", "", parser.AgentCodex, "root-a", "own.jsonl")
			combined := projectionOutcome("exact parent")
			combined.Outcome.Results = append(combined.Outcome.Results, linkChildOutcome().Outcome.Results...)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, own), own, combined))
			if mode == "removed" {
				next, _ := f.acceptScoped(t, "device-a", "removed", accepted.Receipt, parser.AgentCodex, "root-a", "own.jsonl")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, linkChildOutcome()))
			} else {
				require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
				removed, err := f.sink.ExcludeTrashedSession(t.Context(), "codex:portable")
				require.NoError(t, err)
				require.True(t, removed)
			}
			// A different logical group advertises the same explicit alias. Its raw
			// capture tuple is compatible, but it cannot defeat historical authority in
			// another alias group. This deliberately exercises the broader alias set.
			alternate, _ := f.acceptScoped(t, "device-a", "alternate", "", parser.AgentCodex, "root-a", "alternate.jsonl")
			p := projectionOutcome("fallback parent")
			p.Outcome.Results[0].Result.Session.SourceSessionID = "alternate-key"
			p.Outcome.Results[0].Result.Session.StartedAt = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, alternate), alternate, p))
			h, err := newHostedAdapter(f.runtime, f.tenant)
			require.NoError(t, err)
			got, err := h.GetSessionFull(t.Context(), "codex:child")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Nil(t, got.ParentSessionID)
			assert.Empty(t, got.ParentSessionIDs)
		})
	}
}

func TestHostedCrossSourceAliasAnchorLimitsHistoricalAuthority(t *testing.T) {
	f := newProjectionFixture(t)
	own, accepted := f.acceptScoped(t, "device-a", "own", "", parser.AgentCodex, "root-a", "own.jsonl")
	combined := projectionOutcome("historical parent")
	combined.Outcome.Results = append(combined.Outcome.Results, linkChildOutcome().Outcome.Results...)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, own), own, combined))
	alternate, _ := f.acceptScoped(t, "device-a", "alternate", "", parser.AgentCodex, "root-a", "alternate.jsonl")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, alternate), alternate, projectionOutcome("selected parent")))
	anchor := f.alias(t, alternate)
	child := linkChildOutcome()
	child.Outcome.Results[0].Result.Session.ParentSessionID = anchor
	next, _ := f.acceptScoped(t, "device-a", "removed", accepted.Receipt, parser.AgentCodex, "root-a", "own.jsonl")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, child))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	got, err := h.GetSessionFull(t.Context(), "codex:child")
	require.NoError(t, err)
	require.NotNil(t, got.ParentSessionID)
	assert.Equal(t, "codex:portable", *got.ParentSessionID)
	messages, err := h.GetAllMessages(t.Context(), *got.ParentSessionID)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	assert.Equal(t, "selected parent", messages[0].Content)
}

func TestHostedCrossSourceLinkLookupsStayBounded(t *testing.T) {
	f := newProjectionFixture(t)
	child, _ := f.acceptScoped(t, "device-a", "child", "", parser.AgentCodex, "root-a", "child.jsonl")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, child), child, linkChildOutcome()))
	parent, _ := f.acceptScoped(t, "device-a", "parent", "", parser.AgentCodex, "root-a", "parent.jsonl")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, parent), parent, projectionOutcome("parent")))
	identity, err := f.sink.Resolve(t.Context(), "codex:child")
	require.NoError(t, err)
	for _, size := range []int{32, 8192} {
		seedRawUnrelatedProof(t, f, rawSourceID(child), identity.SessionID, size)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_public_aliases(alias_id,group_id) SELECT base_alias,group_id FROM raw_session_groups WHERE group_id LIKE 'filler-group-%' ON CONFLICT DO NOTHING;
 INSERT INTO raw_session_links(branch_id,kind,ordinal,call_index,event_index,target_alias) SELECT branch_id,'parent',-1,-1,-1,'filler-alias-'||substr(branch_id,15) FROM raw_session_branches WHERE branch_id LIKE 'filler-branch-%' ON CONFLICT DO NOTHING`)
		require.NoError(t, err)
		_, err = f.admin.ExecContext(t.Context(), `ANALYZE sessions; ANALYZE raw_session_links; ANALYZE raw_session_branches; ANALYZE raw_source_projections; ANALYZE raw_session_public_aliases`)
		require.NoError(t, err)
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			t.Run(fmt.Sprintf("%d/%s", size, mode), func(t *testing.T) {
				plan := rawLookupPlan(t, f.runtime, mode, `SELECT DISTINCT target.session_id `+hostedLinkFromSQL+` AND owner.session_id=$1`, identity.SessionID, size == 32)
				assert.Equal(t, float64(1), plan["Actual Rows"])
				buffers := plan["Shared Hit Blocks"].(float64) + plan["Shared Read Blocks"].(float64)
				assert.Less(t, buffers, float64(200), "targeted relation touches unrelated pages: %v", plan)
				indexedAlias := false
				var visit func(map[string]any)
				visit = func(node map[string]any) {
					if cond, ok := node["Index Cond"].(string); ok && strings.Contains(cond, "alias_id =") {
						indexedAlias = true
					}
					if removed, ok := node["Rows Removed by Filter"].(float64); ok {
						assert.LessOrEqual(t, removed, float64(3), "unrelated proof scanned: %v", node)
					}
					if children, ok := node["Plans"].([]any); ok {
						for _, c := range children {
							visit(c.(map[string]any))
						}
					}
				}
				visit(plan)
				assert.True(t, indexedAlias, "alias must constrain candidate search")
				t.Logf("size=%d %s shared buffers=%.0f", size, mode, buffers)
			})
		}
	}
}

func TestHostedCrossSourcePluralOwnersRemainContextual(t *testing.T) {
	f := newProjectionFixture(t)
	for _, device := range []string{"device-a", "device-b"} {
		child, _ := f.acceptScoped(t, device, "child", "", parser.AgentCodex, "root-a", "child.jsonl")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, child), child, linkChildOutcome()))
		parent, _ := f.acceptScoped(t, device, "parent", "", parser.AgentCodex, "root-a", "parent.jsonl")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, parent), parent, projectionOutcome(device)))
	}
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = h.GetSessionFull(t.Context(), "codex:portable")
	var conflict *db.SessionIdentityError
	require.ErrorAs(t, err, &conflict)
	require.Len(t, conflict.Variants, 2)
	child, err := h.GetSessionFull(t.Context(), "codex:child")
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Nil(t, child.ParentSessionID)
	assert.Equal(t, conflict.Variants, child.ParentSessionIDs)
	for _, parent := range conflict.Variants {
		children, err := h.GetChildSessions(t.Context(), parent)
		require.NoError(t, err)
		require.Len(t, children, 1)
		require.NotNil(t, children[0].ParentSessionID)
		assert.Equal(t, parent, *children[0].ParentSessionID)
	}
}

// The owner is published once. Only its separately captured target changes,
// including a target revision between physical reads and public edge mapping.
func TestHostedCrossSourceTargetLifecycleAndReadRevision(t *testing.T) {
	f := newProjectionFixture(t)
	own, _ := f.acceptScoped(t, "device-a", "owner", "", parser.AgentCodex, "root-a", "child.jsonl")
	child := linkChildOutcome()
	child.Outcome.Results[0].Result.Messages[1].HasToolUse = true
	child.Outcome.Results[0].Result.Messages[1].ToolCalls[0].SubagentSessionID = "codex:portable"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, own), own, child))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	ownerIdentity, err := f.sink.Resolve(t.Context(), "codex:child")
	require.NoError(t, err)
	receipt := ""
	publish := func(capture string, seconds int) {
		target, accepted := f.acceptScoped(t, "device-a", capture, receipt, parser.AgentCodex, "root-a", "parent.jsonl")
		receipt = accepted.Receipt
		result := projectionOutcome("parent")
		result.Outcome.Results[0].Result.Session.EndedAt = result.Outcome.Results[0].Result.Session.StartedAt.Add(time.Duration(seconds) * time.Second)
		if seconds == 0 {
			result.Outcome.Results = nil
		}
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, target), target, result))
	}
	check := func(present bool, seconds int) {
		got, err := h.GetSessionFull(t.Context(), "codex:child")
		require.NoError(t, err)
		require.NotNil(t, got)
		if present {
			require.NotNil(t, got.ParentSessionID)
			assert.Equal(t, "codex:portable", *got.ParentSessionID)
		} else {
			assert.Nil(t, got.ParentSessionID)
		}
		children, err := h.GetChildSessions(t.Context(), "codex:portable")
		require.NoError(t, err)
		if present {
			require.Len(t, children, 1)
			assert.Equal(t, "codex:child", children[0].ID)
		} else {
			assert.Empty(t, children)
		}
		messages, err := h.GetAllMessages(t.Context(), "codex:child")
		require.NoError(t, err)
		require.Len(t, messages, 2)
		require.Len(t, messages[1].ToolCalls, 1)
		expected := ""
		if present {
			expected = "codex:portable"
		}
		assert.Equal(t, expected, messages[1].ToolCalls[0].SubagentSessionID)
		timing, err := h.GetSessionTiming(t.Context(), "codex:child")
		require.NoError(t, err)
		if present {
			require.NotNil(t, timing.SlowestCall)
			require.NotNil(t, timing.SlowestCall.DurationMs)
			assert.Equal(t, int64(seconds*1000), *timing.SlowestCall.DurationMs)
		} else {
			assert.Nil(t, timing.SlowestCall)
			require.Len(t, timing.Turns, 1)
			require.Len(t, timing.Turns[0].Calls, 1)
			assert.Nil(t, timing.Turns[0].Calls[0].DurationMs)
		}
		sidebar, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: 10})
		require.NoError(t, err)
		assert.Equal(t, 1, sidebar.Total)
		count := 1
		if present {
			count = 2
		}
		require.Len(t, sidebar.Sessions, count)
		var ownerSession, ownerManifest string
		var generation int
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT session_id,manifest_id,projection_generation FROM raw_session_branches WHERE source_id=$1`, rawSourceID(own)).Scan(&ownerSession, &ownerManifest, &generation))
		assert.Equal(t, ownerIdentity.SessionID, ownerSession)
		assert.Equal(t, own.ManifestID, ownerManifest)
		assert.Equal(t, 1, generation)
	}
	check(false, 0)
	publish("target", 7)
	check(true, 7)
	publish("removed", 0)
	check(false, 0)
	publish("reintroduced", 7)
	check(true, 7)
	type snapshot struct {
		session  *db.Session
		duration int64
	}
	reads := 0
	result, err := hostedRead(t.Context(), h, func(_ hostedRevision) (snapshot, error) {
		reads++
		physical, err := h.physical.GetSessionFull(t.Context(), ownerIdentity.SessionID)
		if err != nil {
			return snapshot{}, err
		}
		timing, err := h.GetSessionTiming(t.Context(), "codex:child")
		if err != nil {
			return snapshot{}, err
		}
		require.NotNil(t, timing.SlowestCall)
		require.NotNil(t, timing.SlowestCall.DurationMs)
		duration := *timing.SlowestCall.DurationMs
		if reads == 1 {
			publish("mid-read-target", 13)
		}
		refs := hostedRefs{}
		refs.session(physical)
		err = refs.mapIDs(t.Context(), h)
		return snapshot{physical, duration}, err
	})
	require.NoError(t, err)
	assert.Equal(t, 2, reads)
	assert.Equal(t, int64(13000), result.duration)
	require.NotNil(t, result.session.ParentSessionID)
	assert.Equal(t, "codex:portable", *result.session.ParentSessionID)
	check(true, 13)
}
