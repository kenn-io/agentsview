package db

import (
	"context"
	"fmt"
)

const imageTextFTS = `
CREATE VIRTUAL TABLE IF NOT EXISTS image_text_fts USING fts5(text, content='image_text_cache', content_rowid='rowid', tokenize='porter unicode61');
CREATE TRIGGER IF NOT EXISTS image_text_ai AFTER INSERT ON image_text_cache BEGIN
 INSERT INTO image_text_fts(rowid,text) VALUES(new.rowid,new.text);
END;
CREATE TRIGGER IF NOT EXISTS image_text_ad AFTER DELETE ON image_text_cache BEGIN
 INSERT INTO image_text_fts(image_text_fts,rowid,text) VALUES('delete',old.rowid,old.text);
END;
CREATE TRIGGER IF NOT EXISTS image_text_au AFTER UPDATE OF text ON image_text_cache BEGIN
 INSERT INTO image_text_fts(image_text_fts,rowid,text) VALUES('delete',old.rowid,old.text);
 INSERT INTO image_text_fts(rowid,text) VALUES(new.rowid,new.text);
END;`

const imageTextCJKFTS = `
CREATE VIRTUAL TABLE IF NOT EXISTS image_text_cjk_fts USING fts5(text, content='image_text_cache', content_rowid='rowid', tokenize='simple 0');
CREATE TEMP TRIGGER IF NOT EXISTS image_text_cjk_ai AFTER INSERT ON main.image_text_cache WHEN ` + cjkFTSRuntimeMatchesSQL + ` BEGIN
 INSERT INTO image_text_cjk_fts(rowid,text) VALUES(new.rowid,new.text);
END;
CREATE TEMP TRIGGER IF NOT EXISTS image_text_cjk_ad AFTER DELETE ON main.image_text_cache WHEN ` + cjkFTSRuntimeMatchesSQL + ` BEGIN
 INSERT INTO image_text_cjk_fts(image_text_cjk_fts,rowid,text) VALUES('delete',old.rowid,old.text);
END;
CREATE TEMP TRIGGER IF NOT EXISTS image_text_cjk_au AFTER UPDATE OF text ON main.image_text_cache WHEN ` + cjkFTSRuntimeMatchesSQL + ` BEGIN
 INSERT INTO image_text_cjk_fts(image_text_cjk_fts,rowid,text) VALUES('delete',old.rowid,old.text);
 INSERT INTO image_text_cjk_fts(rowid,text) VALUES(new.rowid,new.text);
END;`

func ensureImageTextFTS(ctx context.Context, w *writerHandle, rebuild bool) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='image_text_fts'`).Scan(&exists); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, imageTextFTS); err != nil {
		return fmt.Errorf("creating image text FTS: %w", err)
	}
	if exists == 0 || rebuild {
		if _, err = tx.ExecContext(ctx, `INSERT INTO image_text_fts(image_text_fts) VALUES('rebuild')`); err != nil {
			return err
		}
	}
	if simpleFTSRuntimeConfig.available() {
		if _, err = tx.ExecContext(ctx, imageTextCJKFTS); err != nil {
			return fmt.Errorf("creating image text CJK FTS: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO image_text_cjk_fts(image_text_cjk_fts) VALUES('rebuild')`); err != nil {
			return err
		}
	}
	return tx.Commit()
}
