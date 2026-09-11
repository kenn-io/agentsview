package postgres

// Claim binds one server timestamp and one generation per query. The ordering
// matches the generation-leading partial indexes, including the due-time range.
const hostedEmbeddingExpiredSQL = `WITH expired AS (SELECT tenant_id,generation_id,session_id FROM hosted_embedding_requirements WHERE tenant_id=$1 AND generation_id=$4 AND state='leased' AND lease_expires_at<=$5 ORDER BY lease_expires_at,session_id LIMIT $2 FOR UPDATE SKIP LOCKED) UPDATE hosted_embedding_requirements r SET state=CASE WHEN attempts>=$3 THEN 'failed' ELSE 'retry' END,error_code='lease_expired',lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,available_at=$5,snapshot_manifest_hash=NULL FROM expired x WHERE r.tenant_id=x.tenant_id AND r.generation_id=x.generation_id AND r.session_id=x.session_id`

// Limit before the source join: stale/dirty source filtering must not cause an
// arbitrarily large queue scan. Reconcile makes stale sources runnable later.
// Retire exhausted candidates so lowering the retry ceiling cannot pin the head.
const hostedEmbeddingDueSQL = `WITH candidates AS MATERIALIZED (SELECT r.* FROM hosted_embedding_requirements r WHERE r.tenant_id=$1 AND r.generation_id=$4 AND r.state IN ('ready','retry') AND r.available_at<=$5 ORDER BY r.available_at,r.session_id LIMIT $2 FOR UPDATE SKIP LOCKED), exhausted AS (UPDATE hosted_embedding_requirements r SET state='failed',error_code='attempts_exhausted' FROM candidates x WHERE r.tenant_id=x.tenant_id AND r.generation_id=x.generation_id AND r.session_id=x.session_id AND x.attempts>=$3) SELECT r.generation_id,CASE WHEN octet_length(r.session_id)<=65536 THEN r.session_id ELSE repeat('!',65537) END,r.required_revision,r.attempt_fence FROM candidates r JOIN hosted_embedding_sources j ON j.tenant_id=r.tenant_id AND j.session_id=r.session_id AND j.revision=r.required_revision WHERE NOT j.dirty AND r.attempts<$3 ORDER BY r.available_at,r.session_id`
