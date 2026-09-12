package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const HostedEmbeddingActivationAutomatic = "automatic"
const HostedEmbeddingActivationManual = "manual"

const embeddingImmutableBody = `BEGIN IF TG_OP<>'UPDATE' OR ROW(OLD.tenant_id,OLD.id,OLD.instance_key,OLD.recipe_fingerprint,OLD.recipe_json,OLD.dimensions,OLD.activation_mode) IS DISTINCT FROM ROW(NEW.tenant_id,NEW.id,NEW.instance_key,NEW.recipe_fingerprint,NEW.recipe_json,NEW.dimensions,NEW.activation_mode) THEN RAISE EXCEPTION 'embedding recipe is immutable'; END IF; RETURN NEW; END; `

func embeddingActivationGuardBody(tenant string) string {
	literal := "'" + strings.ReplaceAll(tenant, "'", "''") + "'"
	return `BEGIN
 IF NOT NEW.active_valid OR NEW.active_generation_id IS NULL THEN RETURN NEW; END IF;
 IF TG_OP='UPDATE' THEN
 IF OLD.active_valid AND OLD.active_generation_id IS NOT DISTINCT FROM NEW.active_generation_id THEN RETURN NEW; END IF;
 END IF;
 IF EXISTS(SELECT 1 FROM hosted_embedding_generations WHERE tenant_id=NEW.tenant_id AND id=NEW.active_generation_id AND activation_mode='manual') THEN
 IF NEW.tenant_id IS DISTINCT FROM ` + literal + ` OR current_setting('agentsview.embedding_activation_tenant',true) IS DISTINCT FROM NEW.tenant_id OR current_setting('agentsview.embedding_activation_generation',true) IS DISTINCT FROM NEW.active_generation_id::text THEN
 RAISE EXCEPTION 'manual embedding activation requires explicit approval';
 END IF;
 END IF;
 RETURN NEW; END; `
}

// Owner provisioning alone may recognize a completely absent extension. Any
// remnant (including the updated immutable body) requires the full contract.
// This never catches a catalog error to retry a weaker validation path.
func checkEmbeddingOwnerBaseline(ctx context.Context, q hostedQuerier, schema, tenant string) (bool, error) {
	var legacy bool
	err := q.QueryRowContext(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass(format('%I.hosted_embedding_generations',$1::text)) AND attname='activation_mode' AND NOT attisdropped)
 AND NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass(format('%I.hosted_embedding_generations',$1::text)) AND conname='hosted_embedding_activation_mode_check')
 AND NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=to_regclass(format('%I.hosted_embedding_state',$1::text)) AND tgname='hosted_embedding_activation_guard')
 AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname='hosted_embedding_activation_guard')
 AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname='hosted_embedding_recipe_immutable' AND p.prosrc<>$2)`, schema, embeddingLegacyImmutableBody).Scan(&legacy)
	if err != nil {
		return false, err
	}
	return legacy, checkHostedCatalogEmbeddingPolicy(ctx, q, schema, tenant, legacy)
}

// Caller holds the corpus fence, after source DDL locks on first provision.
// Validate all old protections before extending; preserve data and selection.
func upgradeEmbeddingCutover(ctx context.Context, tx *sql.Tx, schema, tenant string) error {
	legacy, err := checkEmbeddingOwnerBaseline(ctx, tx, schema, tenant)
	if err != nil {
		return err
	}
	if !legacy {
		return nil
	}
	_, err = tx.ExecContext(ctx, `ALTER TABLE hosted_embedding_generations ADD COLUMN activation_mode text NOT NULL DEFAULT 'automatic' CONSTRAINT hosted_embedding_activation_mode_check CHECK(activation_mode IN ('automatic','manual'));
 CREATE OR REPLACE FUNCTION hosted_embedding_recipe_immutable() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $embed$`+embeddingImmutableBody+`$embed$;
 CREATE FUNCTION hosted_embedding_activation_guard() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $embed$`+embeddingActivationGuardBody(tenant)+`$embed$;
 CREATE TRIGGER hosted_embedding_activation_guard BEFORE INSERT OR UPDATE ON hosted_embedding_state FOR EACH ROW EXECUTE FUNCTION hosted_embedding_activation_guard();`)
	if err != nil {
		return err
	}
	return checkHostedCatalog(ctx, tx, schema, tenant)
}

func checkEmbeddingCutover(ctx context.Context, q hostedQuerier, schema, tenant string) error {
	var valid bool
	err := q.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass(format('%I.hosted_embedding_generations',$1::text)) AND attname='activation_mode' AND atttypid='text'::regtype AND attnotnull AND NOT attisdropped)
 AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass(format('%I.hosted_embedding_generations',$1::text)) AND conname='hosted_embedding_activation_mode_check' AND contype='c' AND convalidated AND pg_get_expr(conbin,conrelid)=$2)`, schema, `(activation_mode = ANY (ARRAY['automatic'::text, 'manual'::text]))`).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("hosted embedding activation mode missing or altered; owner upgrade required for legacy schemas")
	}
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace WHERE t.tgrelid=to_regclass(format('%I.hosted_embedding_state',$1::text)) AND t.tgname='hosted_embedding_activation_guard' AND t.tgenabled='O' AND t.tgtype=23 AND t.tgattr=''::int2vector AND NOT t.tgisinternal AND t.tgqual IS NULL AND t.tgnargs=0 AND p.prosrc=$2 AND NOT p.prosecdef AND p.proconfig IS NULL AND n.nspname=$1 AND p.proname='hosted_embedding_activation_guard')`, schema, embeddingActivationGuardBody(tenant)).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("hosted embedding activation guard missing or altered")
	}
	var unsafe bool
	err = q.QueryRowContext(ctx, `SELECT c.relowner<>(SELECT oid FROM pg_roles WHERE rolname=current_user) AND has_column_privilege(c.oid,'activation_mode','UPDATE') FROM pg_class c WHERE c.oid=to_regclass(format('%I.hosted_embedding_generations',$1::text))`, schema).Scan(&unsafe)
	if err != nil {
		return err
	}
	if unsafe {
		return fmt.Errorf("hosted embedding activation mode requires owner-only updates")
	}
	return nil
}
