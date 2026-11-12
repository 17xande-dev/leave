BEGIN IMMEDIATE;
CREATE TABLE goose_db_version (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  version_id INTEGER NOT NULL,
  is_applied INTEGER NOT NULL,
  tstamp TEXT NOT NULL DEFAULT (datetime('now'))
) STRICT;
INSERT INTO goose_db_version (version_id, is_applied) VALUES (0, 1), (1, 1);
COMMIT;
