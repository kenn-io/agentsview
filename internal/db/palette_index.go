package db

import (
	"context"
	"fmt"
	"strings"
)

const paletteCallsSQL = `COALESCE((SELECT json_group_array(json_object(
 'tool_name', tc.tool_name, 'input_json', COALESCE(tc.input_json, ''),
 'rendering', COALESCE(tc.rendering, ''), 'result_content', COALESCE(tc.result_content, ''),
 'result_events', json(COALESCE((SELECT json_group_array(json_object('content', content))
 FROM (SELECT tre.content FROM tool_result_events tre
 WHERE tre.session_id = m.session_id AND tre.tool_call_message_ordinal = m.ordinal
 AND tre.call_index = COALESCE(tc.call_index, 0) ORDER BY tre.event_index)), '[]'))))
 FROM (SELECT * FROM tool_calls WHERE message_id = m.id ORDER BY call_index, id) tc), '[]')`

const schemaPaletteFTS = `CREATE VIRTUAL TABLE IF NOT EXISTS palette_fts USING fts5(
 content, content='palette_messages', content_rowid='id', tokenize='porter unicode61'
);
CREATE TRIGGER IF NOT EXISTS palette_ai AFTER INSERT ON palette_messages BEGIN
 INSERT INTO palette_fts(rowid, content) VALUES(new.id, new.content);
END;
CREATE TRIGGER IF NOT EXISTS palette_ad AFTER DELETE ON palette_messages BEGIN
 INSERT INTO palette_fts(palette_fts, rowid, content) VALUES('delete', old.id, old.content);
END;
CREATE TRIGGER IF NOT EXISTS palette_au AFTER UPDATE ON palette_messages BEGIN
 INSERT INTO palette_fts(palette_fts, rowid, content) VALUES('delete', old.id, old.content);
 INSERT INTO palette_fts(rowid, content) VALUES(new.id, new.content);
END;`

const schemaPaletteCJKFTS = `CREATE VIRTUAL TABLE IF NOT EXISTS palette_cjk_fts USING fts5(
 content, content='palette_messages', content_rowid='id', tokenize='simple 0'
);`

const schemaPaletteCJKTriggers = `
CREATE TEMP TRIGGER IF NOT EXISTS palette_cjk_ai AFTER INSERT ON main.palette_messages
WHEN ` + cjkFTSRuntimeMatchesSQL + ` BEGIN
 INSERT INTO palette_cjk_fts(rowid, content) VALUES(new.id, new.content);
END;
CREATE TEMP TRIGGER IF NOT EXISTS palette_cjk_ad AFTER DELETE ON main.palette_messages
WHEN ` + cjkFTSRuntimeMatchesSQL + ` BEGIN
 INSERT INTO palette_cjk_fts(palette_cjk_fts, rowid, content) VALUES('delete', old.id, old.content);
END;
CREATE TEMP TRIGGER IF NOT EXISTS palette_cjk_au AFTER UPDATE ON main.palette_messages
WHEN ` + cjkFTSRuntimeMatchesSQL + ` BEGIN
 INSERT INTO palette_cjk_fts(palette_cjk_fts, rowid, content) VALUES('delete', old.id, old.content);
 INSERT INTO palette_cjk_fts(rowid, content) VALUES(new.id, new.content);
END;`

func paletteRefreshSQL(predicate string) string {
	return `INSERT INTO palette_messages(id, content)
 SELECT m.id, agentsview_palette_text(m.content, COALESCE(m.thinking_text, ''),
 COALESCE(m.tool_result_text, ''), COALESCE(m.content_layout, ''), ` + paletteCallsSQL + `)
 FROM messages m WHERE ` + predicate + `
 ON CONFLICT(id) DO UPDATE SET content = excluded.content
 WHERE palette_messages.content IS NOT excluded.content;`
}

// ensurePaletteCorpus rebuilds derived text only on its recipe upgrade. Triggers
// keep every saved owner, including late results and policy projections, current.
func ensurePaletteCorpus(ctx context.Context, conn cjkFTSTransactor, ftsAvailable bool) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS palette_messages (
 id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE, content TEXT NOT NULL
);`); err != nil {
		return err
	}
	for _, owner := range []struct{ table, predicate string }{
		{"messages", "m.id = %s.id"},
		{"tool_calls", "m.id = %s.message_id"},
		{"tool_result_events", "m.session_id = %[1]s.session_id AND m.ordinal = %[1]s.tool_call_message_ordinal"},
	} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := "palette_owner_" + owner.table + "_" + strings.ToLower(event)
			body := ""
			if event != "INSERT" {
				body += paletteRefreshSQL(fmt.Sprintf(owner.predicate, "old"))
			}
			if event != "DELETE" {
				body += paletteRefreshSQL(fmt.Sprintf(owner.predicate, "new"))
			}
			if owner.table == "messages" && event == "DELETE" {
				body = "DELETE FROM palette_messages WHERE id = old.id;"
			}
			if _, err := tx.ExecContext(ctx, "CREATE TRIGGER IF NOT EXISTS "+name+" AFTER "+event+" ON "+owner.table+" BEGIN "+body+" END;"); err != nil {
				return err
			}
		}
	}
	var current bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM stats WHERE key = 'palette_corpus_recipe' AND value = ?)`, PaletteCorpusRecipe).Scan(&current); err != nil {
		return err
	}
	if !current {
		if _, err := tx.ExecContext(ctx, "DELETE FROM stats WHERE key = ?", cjkFTSFingerprintStatsKey); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, paletteRefreshSQL("true")); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO stats(key,value) VALUES('palette_corpus_recipe',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, PaletteCorpusRecipe); err != nil {
			return err
		}
	}
	if ftsAvailable {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name = 'palette_fts')`).Scan(&exists); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, schemaPaletteFTS); err != nil {
			return err
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, `INSERT INTO palette_fts(palette_fts) VALUES('rebuild')`); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
