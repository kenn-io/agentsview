-- Reporter artifact, kenn-io/agentsview#1676
-- Captured verbatim from https://github.com/kenn-io/agentsview/issues/1676
-- by hankel-ai, 2026-09-08T21:45:14Z.
--
-- Version: agentsview v0.42.0 (commit ff8fb4e8, built 2026-09-01T19:35:44Z),
-- Windows, daemon started as `serve --background --no-browser`.
--
-- Reported symptom, verbatim:
--
--   2026/09/07 19:52:35 sync error: decoding cursor IDE composer 1bf6b690-…: jsontext: unexpected EOF
--   … repeated every ~5s …
--   ERROR: startup sync worker ran but did not complete: startup worker pass reported failed
--   (surfacing incomplete pass; not re-syncing in process)
--
--   332/332 sessions (100%) · 43 messages   fatal: sync worker startup: failed
--
-- 13,734 occurrences of that line accumulated in debug.log over about 20 hours.
--
-- Reported root cause, verbatim: the offending record is not corrupt or
-- truncated, the value is NULL. Reading
-- %APPDATA%\Cursor\User\globalStorage\state.vscdb directly:
--
--   table = cursorDiskKV
--   key   = composerData:1bf6b690-2756-4be9-8cfd-033354f0e040
--   value = NULL
--
-- Distribution: 64 rows with a NULL value out of 2,189 in cursorDiskKV.
-- 2 composerData: keys and 62 bubbleId: keys. Only the first is ever logged,
-- because the pass gives up there.
--
-- Reported workaround, verbatim:
--
--   DELETE FROM cursorDiskKV WHERE value IS NULL;
--
-- Reported reproduce step, verbatim: insert a NULL-valued composerData: row
-- into cursorDiskKV in a state.vscdb under a cursor-ide root and start the
-- daemon.
--
-- Applied on top of a cursorDiskKV table the existing test helpers build.
-- Keyed by prefix so internal/parser and internal/sync tests load the same
-- file without hardcoding each other's composer IDs, following
-- internal/parser/testdata/zed-legacy-threads.sql.

INSERT INTO cursorDiskKV (key, value)
VALUES ('composerData:1bf6b690-2756-4be9-8cfd-033354f0e040', NULL);

UPDATE cursorDiskKV SET value = NULL
WHERE key LIKE 'bubbleId:%:nullvalue-%';
