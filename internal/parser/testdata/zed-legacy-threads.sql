CREATE TABLE threads (
  id TEXT PRIMARY KEY,
  summary TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  data_type TEXT NOT NULL,
  data BLOB NOT NULL
);
