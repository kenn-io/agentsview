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

## Frontend and End-to-End Tests

- Keep frontend unit tests beside the code in `*.test.ts` files.
- Put Playwright tests in `frontend/e2e/`.

## Shell Tests

Run scripts against controlled input and assert their output, exit code, or side
effects. Do not read a script and assert that it contains an implementation
line, flag, or snippet.
