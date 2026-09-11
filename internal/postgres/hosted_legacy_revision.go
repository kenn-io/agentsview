package postgres

// Triggers run after sessions row locks. Membership changes therefore never
// wait for an alias/group lock: contention aborts atomically with SQLSTATE 40001
// and the owner must retry the complete write. Ordinary metadata only bumps
// corpus revision and does not acquire identity locks.
const hostedLegacyRevisionBody = `
DECLARE old_legacy boolean := false; new_legacy boolean := false; identity_change boolean; bound_tenant text; legacy_ids text[] := '{}'; lock_bucket integer;
BEGIN
 IF TG_OP <> 'INSERT' THEN old_legacy := OLD.provenance_kind='legacy' AND COALESCE(OLD.owner_marker,'') NOT LIKE 'raw-projection:%'; END IF;
 IF TG_OP <> 'DELETE' THEN new_legacy := NEW.provenance_kind='legacy' AND COALESCE(NEW.owner_marker,'') NOT LIKE 'raw-projection:%'; END IF;
 IF NOT old_legacy AND NOT new_legacy THEN RETURN NULL; END IF;
 IF TG_OP='DELETE' THEN bound_tenant:=OLD.tenant_id; ELSE bound_tenant:=NEW.tenant_id; END IF;
 identity_change := TG_OP <> 'UPDATE';
 IF TG_OP='UPDATE' THEN identity_change := old_legacy IS DISTINCT FROM new_legacy OR OLD.id IS DISTINCT FROM NEW.id; END IF;
 IF identity_change THEN
  IF old_legacy THEN legacy_ids:=array_append(legacy_ids,OLD.id); legacy_ids:=array_append(legacy_ids,hosted_legacy_alias(OLD.id)); END IF;
  IF new_legacy THEN legacy_ids:=array_append(legacy_ids,NEW.id); legacy_ids:=array_append(legacy_ids,hosted_legacy_alias(NEW.id)); END IF;
  FOR lock_bucket IN SELECT DISTINCT (hashtextextended(a,0)&255)::int FROM unnest(legacy_ids) a ORDER BY 1 LOOP
   IF NOT pg_try_advisory_xact_lock(hashtext(TG_TABLE_SCHEMA),lock_bucket) THEN
    RAISE EXCEPTION 'hosted legacy identity is busy; retry transaction' USING ERRCODE='40001';
   END IF;
  END LOOP;
  BEGIN
   PERFORM g.group_id FROM raw_session_groups g WHERE EXISTS(SELECT 1 FROM raw_session_public_aliases a WHERE a.group_id=g.group_id AND a.alias_id=ANY(legacy_ids)) ORDER BY g.group_id FOR UPDATE OF g NOWAIT;
  EXCEPTION WHEN lock_not_available THEN
   RAISE EXCEPTION 'hosted legacy identity is busy; retry transaction' USING ERRCODE='40001';
  END;
 END IF;
 INSERT INTO raw_corpus_state(tenant_id,singleton,identity_revision,corpus_revision) VALUES(bound_tenant,1,CASE WHEN identity_change THEN 1 ELSE 0 END,1)
 ON CONFLICT(tenant_id,singleton) DO UPDATE SET identity_revision=raw_corpus_state.identity_revision+EXCLUDED.identity_revision,corpus_revision=raw_corpus_state.corpus_revision+1;
 RETURN NULL;
END; `
const hostedLegacyAliasBody = ` SELECT 'legacy~'||encode(sha256(convert_to($1,'UTF8')),'hex') `
const hostedLegacyRevisionDDL = `
CREATE OR REPLACE FUNCTION hosted_legacy_alias(text) RETURNS text LANGUAGE sql IMMUTABLE STRICT AS $alias$` + hostedLegacyAliasBody + `$alias$;
CREATE OR REPLACE FUNCTION hosted_legacy_revision() RETURNS trigger LANGUAGE plpgsql AS $revision$` + hostedLegacyRevisionBody + `$revision$;
CREATE OR REPLACE TRIGGER hosted_legacy_revision AFTER INSERT OR UPDATE OR DELETE ON sessions FOR EACH ROW EXECUTE FUNCTION hosted_legacy_revision();
CREATE INDEX IF NOT EXISTS hosted_legacy_alias ON sessions(tenant_id,(hosted_legacy_alias(id))) WHERE provenance_kind='legacy';
`
