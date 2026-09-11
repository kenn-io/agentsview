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

### Timing budgets

Run `go run ./scripts/check-timing-budgets .` or `make check-timing-budgets` to check test source. Both lint targets and the pre-commit hook enforce this check. It rejects literal budgets below one second, including zero and negative durations, in package-qualified `Eventually` and `Never` calls from testify's `assert` and `require` packages. It resolves ordinary import aliases and checks the third argument. Exactly one second and longer budgets are accepted; polling intervals stay outside the rule.

Literal expressions can use numbers, arithmetic, standard `time` units, and `time.Duration` conversions. Expressions containing application identifiers, including named constants, stay outside the rule. Formatted variants, `EventuallyWithT`, assertion-object methods, dot-import calls, and sleeps also stay outside this initial check. The checker reads `_test.go` files regardless of build tags or platform suffixes, excluding descendant `vendor`, `node_modules`, `testdata`, dot-prefixed, and underscore-prefixed directories.

The finite `allowedBudgets` inventory in `scripts/check-timing-budgets/main.go` preserves the retained integration-test scope through explicit file, enclosing function, assertion, duration, and occurrence limits. Watcher allowances include fake-backend fixtures retained by the earlier conversions. Mutex acquisition does not durably block in a synctest bubble. Justify each allowance addition with a concrete reason and review it. Deleted waits may disappear; remove obsolete records during reviewed cleanup. Replacing a call within the same allowed identity and duration retains its allowance.

## Frontend and End-to-End Tests

- Keep frontend unit tests beside the code in `*.test.ts` files.
- Put Playwright tests in `frontend/e2e/`.

## Shell Tests

Run scripts against controlled input and assert their output, exit code, or side
effects. Do not read a script and assert that it contains an implementation
line, flag, or snippet.
