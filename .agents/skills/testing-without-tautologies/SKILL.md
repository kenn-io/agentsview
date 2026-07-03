---
name: testing-without-tautologies
description: Use when creating, editing, fixing, or reviewing tests; when adding mocks, fakes, assertions, unit tests, PG integration tests, frontend component tests, or Playwright e2e tests; or when changing tests after failures.
---

# Testing Without Tautologies

## Core Idea

Tests should fail when protected behavior breaks. A passing test helps only if
it can catch a real problem.

Before writing or changing a test, ask: "What production change should make this
test fail?" If you cannot answer, redesign the test.

## Quality Gate

Before writing the test body, answer these:

- **Who uses this?** Prefer public APIs, HTTP responses, SSE events, persisted
  rows, parsed session output, rendered UI, or CLI output. Avoid private
  state.
- **What example proves it?** Use concrete inputs and literal expected outputs.
  For parsers, hand-write the fixture JSONL and the expected
  sessions/messages; do not run the parser to generate expectations.
- **What break would this catch?** Name the wrong branch, missing side effect,
  wrong argument, boundary case, or contract violation.
- **Do we own it?** Test our choices at framework, database, and filesystem
  boundaries. Do not re-test documented dependency mechanics.
- **Can you state it?** Given this setup, when the user/system does X, then Y
  observable behavior changes. If Y is not assertable, the test is not ready.

## Required Checks

Apply these checks to every new or modified test:

1. **Assert observable effects**

    - Check returned values, rows in the DB, HTTP status and body, emitted SSE
      events, parsed message content, rendered DOM, or errors.
    - A no-assertion test is acceptable only when the failure mode is the
      subject, such as "this config rejects invalid input." Prefer explicit
      assertions anyway.
    - Use `require.X` for checks that must abort the test (setup errors, nil
      receivers, length before indexing) and `assert.X` for independent checks.
      Never `if got != want { t.Fatalf(...) }` in new tests.

1. **Make fakes and mocks specific**

    - Verify arguments, call counts, order, and branches when they are part of
      the contract.
    - Do not let a fake accept any input when the code must pass one value.

1. **Separate branch doubles**

    - Do not reuse one fake handler for success, error, incomplete, or malformed
      paths.
    - Give each branch its own fixture or spy so the wrong branch cannot satisfy
      the expectation.

1. **Do not mock the subject**

    - Mock dependencies, boundaries, and slow or nondeterministic collaborators.
      Prefer real in-process collaborators: `testDB(t)` for SQLite, `httptest`
      for handlers, `t.TempDir()` fixture files for parsers and sync.
    - Do not replace the parser, handler, query, sync step, or component under
      test.
    - Do not fabricate impossible state to provoke an error (for example,
      dropping a column so a query fails). Return a typed error from a seam and
      unit-test the error-to-message mapping directly.

1. **Investigate failures before changing expectations**

    - Do not flip expected values just to make a failing test pass.
    - First decide whether the production change is intended. Then update the
      test to describe the new contract.

1. **Avoid mirror assertions**

    - Do not compute expected values with the same logic under test. Do not build
      the expected FTS query with the query builder, or the expected timestamp
      with the same `timeutil` call the code makes.
    - Use literals, hand-checked fixtures, small examples, or invariant/property
      assertions.
    - Keep test logic simple enough to review by inspection. Table-driven tests
      with literal `want` fields are the preferred shape.

1. **Do not test upstream functionality**

    - Do not prove that `net/http` routing, `mattn/go-sqlite3`, FTS5 tokenizer
      mechanics, `fsnotify`, `pgx`, Svelte reactivity, or Vite work as
      documented.
    - Test your boundary contract instead: route registration, the SQL your code
      emits, value handoff from HTTP params to DB queries, migration effects,
      SSE payload shape, and error responses.
    - For surprising upstream behavior (an FTS5 quoting rule, a WAL edge case, a
      watcher event ordering), write a narrow characterization test around your
      integration point and name the upstream assumption in the test name or a
      comment.

1. **Avoid blindingly obvious current-code assertions**

    - Do not test that the implementation is written the way it is written now.
    - Skip tests for plain constructor assignment, getters, trivial forwarding,
      constants, and data-only structs (for example, that an `AgentDef` entry
      has the fields it was declared with).
    - Test them only when they validate, normalize, default, derive, copy,
      enforce permissions, handle errors, cause side effects, or protect
      compatibility.
    - Prefer the first consumer-visible result that depends on the fields: a
      discovered session, an API response, a rendered row.

1. **Exercise shell scripts, do not read them**

    - Shell script tests must run the script against controlled inputs and assert
      outputs, side effects, or exit codes.
    - Never read a script's source and assert that it contains a specific line,
      flag, or snippet.

1. **Never write negative-existence tests**

    - When removing or refactoring code, never add a test asserting that a
      function, method, file, import, or symbol does not exist — grepping the
      source tree, reflecting over types, or asserting a compile failure.
    - Deletion is proven by the deletion itself: the code is gone, the build
      compiles, and the behavior tests for the replacement pass. A
      "still-deleted" test protects nothing, breaks legitimate future reuse of
      the name, and outlives the migration it policed.
    - If the concern is that the old path could silently come back into use, test
      the new path's observable contract instead — the behavior that would
      break if someone rewired it.
    - Removing such guard tests is part of finishing a cleanup, not a test
      coverage regression.

## Backend Parity

When a behavior is required to match across SQLite and PostgreSQL, protect it on
both sides: the SQLite test in `internal/db` and the PG test in
`internal/postgres` (behind the `pgtest` tag) should assert the same observable
contract — same filtering, ordering, aggregation, and edge cases. A parity bug
that only one backend's test can catch is a parity bug the suite will miss.

## Test Level

Use the narrowest test that can catch the break:

- Parser, sync, db, config, and server logic: Go unit tests with `testDB(t)`,
  `httptest`, and `t.TempDir()` fixtures. Run with `make test` (needs
  `CGO_ENABLED=1` and `-tags "fts5"`).
- PostgreSQL behavior: integration tests with the `pgtest` tag via
  `make test-postgres`.
- Frontend logic and components: colocated `*.test.ts` files.
- User-visible workflows across the HTTP/SQLite/SPA stack: Playwright specs in
  `frontend/e2e/`, seeded via `cmd/testfixture`.

Keep e2e tests non-tautological: assert the workflow result, stored state,
rendered UI, or API contract — not just that the server started or the page did
not crash.

## Mutation Check

Before finishing, mentally mutate the production code. At least one relevant
test should fail for each realistic mutation.

- Wrong constant or argument.
- Wrong branch handler.
- Missing state change (row not written, session not resynced).
- Empty/default return.
- Missing side effect (no SSE event, no FTS row).
- Broken fake at a boundary your code should notice.
- Renamed or rearranged private fields with behavior preserved.
- Missing validation for zero, empty, nil, unauthorized, or malformed input.

If none fail, the test is probably tautological.

## Red Flags

- Reuses the same setup/assertion object, guaranteeing equality.
- Can fail only through panic, error, missing selector, or server crash.
- Still matters if only the framework/library remains.
- Translates a constructor, getter, setter, mapper, or wrapper line by line.
- Exists for coverage without checking side effects, boundaries, or outcomes.
- Hides expected values behind loops, formatters, builders, or helpers.
- Greps source files (Go or shell) for implementation strings instead of
  observing behavior.
- Asserts that a removed function, file, or symbol stays removed.
