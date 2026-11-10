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

// ensureImageTextFTS rebuilds an index only when it is missing or rebuild is
// set. ensureCJKFTS drops the CJK index whenever its tokenizer changes.
func ensureImageTextFTS(ctx context.Context, w cjkFTSTransactor, rebuild bool) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tables := [][2]string{{"image_text_fts", imageTextFTS}}
	if simpleFTSRuntimeConfig.available() {
		tables = append(tables, [2]string{"image_text_cjk_fts", imageTextCJKFTS})
	}
	for _, table := range tables {
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name=?`, table[0]).Scan(&exists); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, table[1]); err != nil {
			return fmt.Errorf("creating %s: %w", table[0], err)
		}
		if exists == 0 || rebuild {
			if _, err = tx.ExecContext(ctx, `INSERT INTO `+table[0]+`(`+table[0]+`) VALUES('rebuild')`); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
