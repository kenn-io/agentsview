---
last_edited: 2026-09-23
---

# Task 6: seat pattern matching

Implemented `CompileSeatPatterns` and `SeatFromPath` in `internal/friction/seat.go`. Patterns require exactly one whole-segment `{seat}`, validate all remaining segments with `path.Match`, and preserve the specified plain error strings. Matching supports rooted patterns, floating contiguous windows, first-pattern priority, leftmost-window priority, and either slash style in input paths.

Added anonymized generic tests in `internal/friction/seat_test.go` for compilation errors, globs, rooted and floating matches, priority, Windows separators, and no-match cases.

## TDD evidence

- RED: `CGO_ENABLED=1 go test -tags fts5 ./internal/friction/ -run 'TestCompileSeatPatterns|TestSeatFromPath' -v` failed to compile with `undefined: CompileSeatPatterns`, `undefined: SeatPattern`, and `undefined: SeatFromPath`, as expected before implementation.
- GREEN: the same command passed all 19 subtests after implementation.

## Checks

- `go fmt ./...` completed successfully.
- `go vet ./...` completed successfully.
- `git diff --check` completed successfully.

## Self-review

The matcher scans each candidate window in path order and patterns in configuration order. Every capture is a non-empty segment because path splitting omits empty segments. No personal path layouts or worker names are encoded.

## Files

- `internal/friction/seat.go`
- `internal/friction/seat_test.go`
