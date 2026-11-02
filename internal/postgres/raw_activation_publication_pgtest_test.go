//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
