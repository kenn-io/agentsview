# Testing Rules

Read this file before adding or changing tests.

## Coverage

- Add unit tests for every new feature and bug fix.
- Run the smallest relevant test set before committing. State which checks you
  could not run.
- Keep tests fast and isolated.

## Go Tests

- Prefer table-driven tests.
- Use `github.com/stretchr/testify` for assertions.
- Use `require.X` when failure must stop the test, such as setup errors, nil
  values, or length checks before indexing.
- Use `assert.X` for independent checks that can continue after failure.
- Do not add `if got != want { t.Fatalf(...) }` comparisons.
- Test helpers must use testify for their own assertions.
- Use the existing `testDB(t)` helper for database tests.
- Use `t.TempDir()` for temporary directories.
- Never locate files from a path returned by `runtime.Caller` or a stack trace;
  builds use `-trimpath`, so those paths are module-relative. Tests run from
  their package directory, so use paths relative to it, such as `testdata/...`
  or `filepath.Join("..", "..", "docs")`.
- A package whose tests load IANA timezone names, such as `America/New_York`,
  keeps a `tzdata_test.go` that imports `time/tzdata`. Trimmed test binaries
  cannot read the zone database in `GOROOT`, and Windows has no system copy.

### Timing budgets

The kit `deadlinetest` analyzer, run by `kennlint` in `make lint` and
`make lint-ci`, rejects sub-second constant budgets in tests outside a
`testing/synctest` bubble, such as `context.WithTimeout`, `time.After`, timers,
and testify `Eventually`. Wait on a real event, run the test under
`synctest.Test`, or use a long hang guard on a `select` that already waits for
the event. Mutex acquisition does not durably block in a synctest bubble.

Mark a site `//nolint:kennlint // reason` only when the asserted result is the
deadline or cancellation expiring, or when a `select` on a timer shows that an
event does not happen. The reason names which case applies and what holds the
wait.

The check sees only the `fts5` test files each lint run compiles: Linux and
Windows in full, and on macOS only the packages the macOS job lints. Tests
behind `pgtest`, `chtest`, `duckdbtest`, or `s3test` go unchecked. It skips
short positive `Never` windows, which cannot fail.

## Frontend and End-to-End Tests

- Keep frontend unit tests beside the code in `*.test.ts` files.
- Put Playwright tests in `frontend/e2e/`.

## Shell Tests

Run scripts against controlled input and assert their output, exit code, or side
effects. Do not read a script and assert that it contains an implementation
line, flag, or snippet.
