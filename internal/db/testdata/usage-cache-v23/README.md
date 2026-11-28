# Format 23 Usage fixture

The archive and its populated cache were written by commit
`822023b03b3c611408b1074098917621b934060e`, the parent of the format 24 change.
They contain synthetic sessions, a cross-session snapshot, and a Cursor row.

To regenerate, use a clean worktree at that commit. From that worktree, copy
`generate_test.go.txt` from this directory to
`internal/db/usage_v23_fixture_generate_test.go`, then run:

```sh
make pricing-snapshot ensure-embed-dir
mkdir -p /tmp/usage-v23-fixture
USAGE_V23_FIXTURE_DIR=/tmp/usage-v23-fixture go test -tags fts5 ./internal/db -run '^TestGenerateUsageCacheV23$'
gzip -n -c /tmp/usage-v23-fixture/archive.db > archive.db.gz
gzip -n -c /tmp/usage-v23-fixture/cache.db > cache.db.gz
```

Place the compressed files in this directory. The archive ID changes each time.
