package postgres

import (
	"context"
	"errors"
)

// Keep the current UTC hour and its 23 predecessors: the health report's
// 24-hour outcome window. The transition and pruning share one transaction.
const rawJobOutcomeBody = `
DECLARE bucket timestamptz := date_trunc('hour',clock_timestamp(),'UTC');
BEGIN
 IF OLD.state IS NOT DISTINCT FROM NEW.state OR NEW.state NOT IN ('retrying','complete','failed','superseded') THEN RETURN NULL; END IF;
 INSERT INTO raw_job_outcome_buckets(tenant_id,bucket_start,stage,outcome,count)
 VALUES(NEW.tenant_id,bucket,NEW.stage,NEW.state,1)
 ON CONFLICT(tenant_id,bucket_start,stage,outcome) DO UPDATE SET count=raw_job_outcome_buckets.count+1;
 DELETE FROM raw_job_outcome_buckets WHERE tenant_id=NEW.tenant_id AND bucket_start<bucket-interval '23 hours';
 RETURN NULL;
END; `

const rawJobOutcomeDDL = `
CREATE OR REPLACE FUNCTION raw_record_job_outcome() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $outcome$` + rawJobOutcomeBody + `$outcome$;
CREATE OR REPLACE TRIGGER raw_job_outcome AFTER UPDATE ON raw_ingest_jobs FOR EACH ROW EXECUTE FUNCTION raw_record_job_outcome();
`

func checkRawJobOutcomeTrigger(ctx context.Context, q hostedQuerier, schema string) error {
	var valid bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
 JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_language l ON l.oid=p.prolang
 WHERE n.nspname=$1 AND c.relname='raw_ingest_jobs' AND t.tgname='raw_job_outcome'
 AND t.tgenabled='O' AND t.tgtype=17 AND t.tgqual IS NULL AND t.tgnargs=0 AND t.tgattr=''::int2vector
 AND NOT t.tgisinternal AND t.tgconstraint=0 AND NOT t.tgdeferrable AND NOT t.tginitdeferred
 AND p.pronamespace=c.relnamespace AND p.proname='raw_record_job_outcome' AND p.pronargs=0
 AND NOT p.prosecdef AND p.proconfig IS NULL AND p.prosrc=$2 AND p.provolatile='v' AND l.lanname='plpgsql')`, schema, rawJobOutcomeBody).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("raw job outcome protection is missing or altered; owner provisioning required")
	}
	return nil
}
