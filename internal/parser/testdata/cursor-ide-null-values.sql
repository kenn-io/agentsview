-- Synthetic reproduction of https://github.com/kenn-io/agentsview/issues/1676.
-- Applies NULL composer and bubble values to a cursorDiskKV table built by
-- the test helper. All identifiers are synthetic; nullvalue-* bubbles are
-- selected by prefix so the fixture does not depend on sibling composer IDs.

INSERT INTO cursorDiskKV (key, value)
VALUES ('composerData:null-value-composer', NULL);

UPDATE cursorDiskKV SET value = NULL
WHERE key LIKE 'bubbleId:%:nullvalue-%';
