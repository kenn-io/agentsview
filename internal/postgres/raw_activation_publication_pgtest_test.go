//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A legacy trigger in the first materialized group must not acquire corpus
// before another group or the source proof has finished its ordinary writes.
func TestRawActivationMultiGroupWritersPublishLast(t *testing.T) {
	for _, mode := range []string{"projection later group", "projection source proof", "empty trash later group"} {
		t.Run(mode, func(t *testing.T) {
			f := newProjectionFixture(t)
			f.projectSession(t, "device-seed", "seed")
			require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
			activateFixture(t, t.Context(), f, "", "")
			_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('codex:one','legacy','device','codex'),('codex:two','legacy','device','codex')`)
			require.NoError(t, err)
			m, _ := f.accept(t, "device-pair", "pair", "")
			out := projectionOutcome("one")
			out.Outcome.Results[0].Result.Session.ID = "codex:one"
			out.Outcome.Results[0].Result.Session.SourceSessionID = "one"
			two := projectionOutcome("two")
			two.Outcome.Results[0].Result.Session.ID = "codex:two"
			two.Outcome.Results[0].Result.Session.SourceSessionID = "two"
			out.Outcome.Results = append(out.Outcome.Results, two.Outcome.Results...)
			lease := f.lease(t, m)
			write := func(ctx context.Context) error { return f.sink.Project(ctx, lease, m, out) }
			switch mode {
			case "projection later group":
				f.parkOn(t, "INSERT", "messages", `NEW.session_id IN (SELECT id FROM sessions WHERE raw_group_id=(SELECT group_id FROM raw_session_groups WHERE base_alias IN ('codex:one','codex:two') ORDER BY group_id DESC LIMIT 1))`)
			case "projection source proof":
				f.parkOn(t, "UPDATE", "raw_source_projections", `true`)
			case "empty trash later group":
				require.NoError(t, write(t.Context()))
				require.NoError(t, f.sink.SetCuration(t.Context(), "codex:one", "trashed", true))
				require.NoError(t, f.sink.SetCuration(t.Context(), "codex:two", "trashed", true))
				f.parkOn(t, "DELETE", "sessions", `OLD.raw_group_id=(SELECT group_id FROM raw_session_groups WHERE base_alias IN ('codex:one','codex:two') ORDER BY group_id DESC LIMIT 1)`)
				write = func(ctx context.Context) error {
					n, err := f.sink.EmptyTrash(ctx)
					assert.Equal(t, 2, n)
					return err
				}
			}
			f.whileParked(t, write, func(ctx context.Context) error {
				tx, err := f.admin.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				_, err = tx.ExecContext(ctx, `SELECT singleton FROM raw_corpus_state WHERE singleton=1 FOR UPDATE NOWAIT`)
				return err
			})
			var maps, hidden int
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&maps))
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE provenance_kind='legacy' AND rollout_hidden`).Scan(&hidden))
			if mode == "empty trash later group" {
				assert.Zero(t, maps)
				assert.Zero(t, hidden)
			} else {
				assert.Equal(t, 2, maps)
				assert.Equal(t, 2, hidden)
			}
		})
	}
}

// Alias collisions may reach a group that is neither projected nor a map owner.
// The legacy revision trigger still touches it, so it belongs in the prelocks.
func TestRawActivationFutureProjectionPrelocksLegacyTriggerCollision(t *testing.T) {
	f := newProjectionFixture(t)
	collision := f.projectSession(t, "device-collision", "other")
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	activateFixture(t, t.Context(), f, "", "")
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('codex:portable','legacy','device','codex')`)
	require.NoError(t, err)
	_, err = f.runtime.Exec(`INSERT INTO raw_session_public_aliases(alias_id,group_id,anchor_branch) VALUES(hosted_legacy_alias('codex:portable'),$1,'')`, collision.GroupID)
	require.NoError(t, err)
	m, _ := f.accept(t, "device-new", "future-collision", "")
	lease := f.lease(t, m)
	blocker, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer blocker.Rollback()
	var blockerPID int
	require.NoError(t, blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = blocker.Exec(`SELECT group_id FROM raw_session_groups WHERE group_id=$1 FOR UPDATE`, collision.GroupID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.sink.Project(ctx, lease, m, projectionOutcome("new")) }()
	waitRawActivationBlockedBy(t, f, blockerPID)
	observer, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer observer.Rollback()
	_, err = observer.ExecContext(ctx, `SELECT singleton FROM raw_corpus_state WHERE singleton=1 FOR UPDATE NOWAIT`)
	require.NoError(t, err)
	require.NoError(t, observer.Rollback())
	require.NoError(t, blocker.Rollback())
	require.NoError(t, <-done)
	var hidden bool
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&hidden))
	assert.True(t, hidden)
}

// The scope write occurs after the fence. Statement triggers reject even a
// content rewrite that happens to recreate byte-identical rows, which snapshots
// alone cannot detect. Ordinary curation and visibility writes remain allowed.
func TestRawActivationDoesNotRewriteAcceptedContentAfterFence(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	_, err := f.admin.Exec(`CREATE FUNCTION mark_activation_fenced() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='active' THEN PERFORM set_config('test.activation_fenced','yes',true); END IF; RETURN NEW; END $$; CREATE TRIGGER mark_activation_fenced BEFORE INSERT OR UPDATE ON raw_rollout_scopes FOR EACH ROW EXECUTE FUNCTION mark_activation_fenced(); CREATE FUNCTION reject_postfence_content() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF current_setting('test.activation_fenced',true)='yes' THEN RAISE EXCEPTION 'activation rewrote accepted content'; END IF; RETURN NULL; END $$`)
	require.NoError(t, err)
	for _, table := range []string{"messages", "tool_calls", "tool_result_events", "usage_events", "session_plan_usage", "secret_findings", "friction_findings", "raw_content_revisions", "raw_message_dialogue", "raw_session_branches", "raw_source_contributions", "session_sources", "raw_session_links", "raw_source_projections", "raw_source_heads", "raw_embedding_outbox"} {
		_, err = f.admin.Exec(`CREATE TRIGGER reject_postfence_content BEFORE INSERT OR UPDATE OR DELETE ON ` + table + ` FOR EACH STATEMENT EXECUTE FUNCTION reject_postfence_content()`)
		require.NoError(t, err)
	}
	before := activationContentSnapshot(t, f.runtime)
	activateFixture(t, t.Context(), f, "device-a", "")
	assert.Equal(t, before, activationContentSnapshot(t, f.runtime))
	assertActivationGuardFree(t, f)
}
