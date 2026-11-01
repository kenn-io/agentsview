package db

// Install after column migrations: legacy sessions lack transcript_revision and
// other columns referenced by the session update trigger. Binding changes queue
// both the changed owner and native-ID peers for database-only turn derivation.
const commitLinksMigratedTriggerSQL = `
DROP TRIGGER IF EXISTS trg_commit_links_update;
CREATE TRIGGER trg_commit_links_update AFTER UPDATE ON commit_session_links
WHEN OLD.payload IS NOT NEW.payload OR OLD.project_hint IS NOT NEW.project_hint OR OLD.source_revision IS NOT NEW.source_revision BEGIN
    UPDATE commit_link_publication_state SET revision=revision+1 WHERE singleton=1;
END;
CREATE TRIGGER IF NOT EXISTS trg_commit_links_session_update AFTER UPDATE ON sessions
WHEN OLD.transcript_revision IS NOT NEW.transcript_revision OR OLD.project IS NOT NEW.project
    OR OLD.deleted_at IS NOT NEW.deleted_at OR OLD.cwd IS NOT NEW.cwd OR OLD.agent IS NOT NEW.agent
    OR OLD.machine IS NOT NEW.machine OR OLD.started_at IS NOT NEW.started_at BEGIN
    UPDATE commit_link_publication_state SET revision=revision+1 WHERE singleton=1;
END;
DROP TRIGGER IF EXISTS trg_commit_links_session_delete;
CREATE TRIGGER trg_commit_links_session_delete AFTER DELETE ON sessions BEGIN
    UPDATE commit_session_links SET project_hint=OLD.project,payload=json_set(payload,
        '$.session',json('null'),'$.gone',json('true'),'$.turn',json('null'),
        '$.agent','','$.project','','$.session_started_at','','$.web_url','')
    WHERE session_id=OLD.id OR json_extract(payload,'$.session.id')=OLD.id
       OR session_id IN (
           SELECT native.parser_id FROM raw_archive_sessions AS native
           WHERE native.session_id=OLD.id AND native.provider=CASE
               WHEN instr(native.parser_id,':')>0 THEN substr(native.parser_id,1,instr(native.parser_id,':')-1)
               ELSE 'claude' END
             AND (SELECT COUNT(*) FROM raw_archive_sessions AS owner
                  WHERE owner.provider=native.provider AND owner.parser_id=native.parser_id)=1
       );
    DELETE FROM commit_link_state WHERE session_id=OLD.id;
    UPDATE commit_link_publication_state SET revision=revision+1 WHERE singleton=1;
END;
DROP TRIGGER IF EXISTS trg_commit_links_binding_insert;
CREATE TRIGGER trg_commit_links_binding_insert AFTER INSERT ON raw_archive_sessions BEGIN
    DELETE FROM commit_link_state WHERE session_id=NEW.session_id OR session_id IN (SELECT session_id FROM raw_archive_sessions WHERE provider=NEW.provider AND parser_id=NEW.parser_id);
    UPDATE commit_link_publication_state SET revision=revision+1 WHERE singleton=1;
END;
DROP TRIGGER IF EXISTS trg_commit_links_binding_update;
CREATE TRIGGER trg_commit_links_binding_update AFTER UPDATE ON raw_archive_sessions BEGIN
    DELETE FROM commit_link_state WHERE session_id=OLD.session_id OR session_id IN (SELECT session_id FROM raw_archive_sessions WHERE provider=OLD.provider AND parser_id=OLD.parser_id) OR session_id=NEW.session_id OR session_id IN (SELECT session_id FROM raw_archive_sessions WHERE provider=NEW.provider AND parser_id=NEW.parser_id);
    UPDATE commit_link_publication_state SET revision=revision+1 WHERE singleton=1;
END;
DROP TRIGGER IF EXISTS trg_commit_links_binding_delete;
CREATE TRIGGER trg_commit_links_binding_delete AFTER DELETE ON raw_archive_sessions BEGIN
    DELETE FROM commit_link_state WHERE session_id=OLD.session_id OR session_id IN (SELECT session_id FROM raw_archive_sessions WHERE provider=OLD.provider AND parser_id=OLD.parser_id);
    UPDATE commit_link_publication_state SET revision=revision+1 WHERE singleton=1;
END;
`
