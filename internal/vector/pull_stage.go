package vector

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kit/embedmodel"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

const pullStageKey = "pull_stage:"

// A replacement owns a separate mirror and kit store until its final archive
// reservation confirms complete coverage. Failed pulls retain this store for
// retry, without touching the active mirror, its scope, or its vectors.
func (ix *Index) newPullStage(ctx context.Context, gen kitvec.Generation) (*Index, error) {
	ix = ix.forGeneration(gen)
	fp := gen.Fingerprint()
	if _, _, err := ix.resolveBuildTarget(ctx, gen, fp, embedmodel.Descriptor{}, false); err != nil {
		return nil, err
	}
	if err := ix.recordGenerationSplit(ctx, fp); err != nil {
		return nil, err
	}
	prefix := ix.spec.VectorsPrefix + "_pull_" + contentHash(fp)
	spec := ix.pullStageSpec(prefix)
	if _, err := ix.db.ExecContext(ctx, spec.MirrorDDL+`
CREATE TABLE IF NOT EXISTS `+spec.repairQueueTable()+` (
 ordinal INTEGER NOT NULL, doc_key TEXT NOT NULL, PRIMARY KEY (ordinal, doc_key)
);
CREATE INDEX IF NOT EXISTS `+spec.repairQueueTable()+`_doc_key ON `+spec.repairQueueTable()+` (doc_key)`); err != nil {
		return nil, err
	}
	stage, err := ix.withProjection(ctx, spec)
	if err != nil {
		return nil, err
	}
	if _, err := stage.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding); err != nil {
		return nil, err
	}
	stage = stage.forGeneration(gen)
	if err := stage.recordGenerationSplit(ctx, fp); err != nil {
		return nil, err
	}
	if err := ix.metaSet(ctx, pullStageKey+fp, prefix); err != nil {
		return nil, err
	}
	return stage, nil
}

func (ix *Index) pullStageSpec(prefix string) IndexSpec {
	spec := ix.spec
	spec.DocsTable, spec.MetaTable, spec.VectorsPrefix = prefix+"_docs", prefix+"_meta", prefix
	spec.MirrorDDL = strings.NewReplacer(ix.spec.DocsTable, spec.DocsTable, ix.spec.MetaTable, spec.MetaTable).Replace(ix.spec.MirrorDDL)
	return spec
}

func (ix *Index) withProjection(ctx context.Context, spec IndexSpec) (*Index, error) {
	projected := *ix
	projected.spec = spec
	store, err := sqlitevec.New[string, string](ctx, ix.db, spec.schema())
	if err != nil {
		return nil, err
	}
	projected.store = store
	return &projected, nil
}

func (ix *Index) pendingPullStage(ctx context.Context, fp string) (*Index, error) {
	prefix, found, err := ix.metaGet(ctx, pullStageKey+fp)
	if err != nil || !found {
		return ix, err
	}
	stage, err := ix.withProjection(ctx, ix.pullStageSpec(prefix))
	if err != nil {
		return nil, err
	}
	split, err := stage.generationSplit(ctx, fp)
	if err != nil {
		return nil, err
	}
	stage.split = split
	return stage, nil
}

// Called only after kit checked activation, while the archive writer is still
// reserved. SQLite publishes the new mirror, vectors, recipe and active state
// together. Prepared searches keep their previous SQLite snapshot.
func (ix *Index) publishPullStage(ctx context.Context, stage *Index, fp string) error {
	ordinal, err := ix.ordinalForFingerprint(ctx, fp)
	if err != nil {
		return err
	}
	stagedOrdinal, err := stage.ordinalForFingerprint(ctx, fp)
	if err != nil {
		return err
	}
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// The mirror is shared by every generation. Remove disappeared documents
	// from retired vector tables too, or they consume KNN slots on revival.
	rows, err := tx.QueryContext(ctx, `SELECT ordinal FROM `+ix.spec.generationsTable())
	if err != nil {
		return err
	}
	defer rows.Close()
	var ordinals []int64
	for rows.Next() {
		var other int64
		if err := rows.Scan(&other); err != nil {
			return err
		}
		ordinals = append(ordinals, other)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	absent := `doc_key NOT IN (SELECT doc_key FROM ` + stage.spec.DocsTable + `)`
	for _, other := range ordinals {
		if other == ordinal {
			continue // The target is replaced in full below.
		}
		query := fmt.Sprintf(`DELETE FROM %s_v%d WHERE rowid IN (SELECT vec_rowid FROM %s WHERE ordinal=? AND %s)`, ix.spec.VectorsPrefix, other, ix.spec.chunksTable(), absent)
		if _, err := tx.ExecContext(ctx, query, other); err != nil {
			return fmt.Errorf("delete disappeared hub vectors: %w", err)
		}
	}
	for _, table := range []string{ix.spec.chunksTable(), ix.spec.stampsTable(), ix.spec.repairQueueTable()} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+absent); err != nil {
			return fmt.Errorf("delete disappeared hub documents: %w", err)
		}
	}
	vec := fmt.Sprintf("%s_v%d", ix.spec.VectorsPrefix, ordinal)
	stagedVec := fmt.Sprintf("%s_v%d", stage.spec.VectorsPrefix, stagedOrdinal)
	queries := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM ` + vec, nil},
		{`INSERT INTO ` + vec + ` (rowid, embedding) SELECT rowid, embedding FROM ` + stagedVec, nil},
		{`DELETE FROM ` + ix.spec.chunksTable() + ` WHERE ordinal=?`, []any{ordinal}},
		{`INSERT INTO ` + ix.spec.chunksTable() + ` SELECT ?,doc_key,chunk_index,vec_rowid FROM ` + stage.spec.chunksTable() + ` WHERE ordinal=?`, []any{ordinal, stagedOrdinal}},
		{`DELETE FROM ` + ix.spec.stampsTable() + ` WHERE ordinal=?`, []any{ordinal}},
		{`DELETE FROM ` + ix.spec.repairQueueTable() + ` WHERE ordinal=?`, []any{ordinal}},
		{`INSERT INTO ` + ix.spec.stampsTable() + ` SELECT ?,doc_key,revision FROM ` + stage.spec.stampsTable() + ` WHERE ordinal=?`, []any{ordinal, stagedOrdinal}},
		{`DELETE FROM ` + ix.spec.DocsTable, nil},
		{`INSERT INTO ` + ix.spec.DocsTable + ` SELECT * FROM ` + stage.spec.DocsTable, nil},
		{`DELETE FROM ` + ix.spec.MetaTable + ` WHERE key=?`, []any{sourceDataVersionKey}},
		{`INSERT OR REPLACE INTO ` + ix.spec.MetaTable + ` SELECT key,value FROM ` + stage.spec.MetaTable, nil},
		{`UPDATE ` + ix.spec.generationsTable() + ` SET state=CASE WHEN gen_key=? THEN 'active' ELSE 'retired' END WHERE gen_key=? OR state IN ('active','building')`, []any{fp, fp}},
		{`DELETE FROM ` + ix.spec.MetaTable + ` WHERE key=? OR key=?`, []any{pullStageKey + fp, activeFullRebuildKey}},
	}
	for _, q := range queries {
		if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("publish hub projection: %w", err)
		}
	}
	// Successful publication consumes the retry store. Dropping the virtual
	// table also drops its shadow tables. Failed publication rolls it all back.
	for _, table := range []string{stagedVec, stage.spec.chunksTable(), stage.spec.stampsTable(), stage.spec.generationsTable(), stage.spec.repairQueueTable(), stage.spec.DocsTable, stage.spec.MetaTable} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+table); err != nil {
			return err
		}
	}
	return tx.Commit()
}
