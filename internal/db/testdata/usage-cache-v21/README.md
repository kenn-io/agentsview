# Format 21 Usage fixture

The archive and populated cache were written by release `v0.47.0`, commit
`1079cb632051e8c9cbad2dcc945a60643c756d81`, with synthetic sessions, a Claude response split across sessions
without a request ID, and a Cursor row.

To regenerate, use a clean worktree at `v0.47.0`. Copy
`generate_test.go.txt` from this directory to
`internal/db/usage_v21_fixture_generate_test.go` in that worktree, then run:

```sh
make pricing-snapshot ensure-embed-dir
mkdir -p /tmp/usage-v21-fixture
USAGE_V21_FIXTURE_DIR=/tmp/usage-v21-fixture go test -tags fts5 ./internal/db -run '^TestGenerateUsageCacheV21$'
gzip -n -c /tmp/usage-v21-fixture/archive.db > archive.db.gz
gzip -n -c /tmp/usage-v21-fixture/cache.db > cache.db.gz
```

Place the compressed files in this directory. The archive ID changes each time.
