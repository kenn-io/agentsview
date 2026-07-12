# DuckDB Runtime Warning Flake Design

## Problem

`TestDuckDBServeRuntimeRecordWriteFailureWarnsVisible` launches a helper process
that exits after three seconds. On a loaded Windows runner, DuckDB startup can
consume that entire interval, so the helper exits successfully before the
runtime-record warning is written. The parent receives empty output and the
assertion fails even though production behavior is unchanged.

## Design

Keep production DuckDB serve behavior unchanged. Replace the child process's
fixed exit timer with condition-based completion in the parent test helper. The
parent will stream the child's stdout, collect it for assertions, and stop the
child only after observing the expected runtime-record warning. Stderr will also
be collected for diagnostic failures.

The command will use a new bounded context as a failure guard so a regression
that never emits the warning cannot hang the test suite. Only stdout will use a
manual pipe; stderr will be assigned directly to a `bytes.Buffer`, allowing
`os/exec` to manage and join that copy. The child will be waited exactly once
after the stdout reader reaches EOF. Completion has three explicit outcomes:

1. After the warning is observed, canceling and reaping the still-running child
   is successful intentional termination; the cancellation exit error is not
   returned.
1. A process exit before the warning is an error, even if its exit status is
   zero.
1. A context deadline before the warning is a timeout error.

The latter two errors will include captured stdout and stderr so CI failures
remain actionable.

The child will retain a two-minute self-exit timer solely as an orphan guard if
the parent test process dies without canceling its context. Normal completion
will always happen by observing the warning and canceling the child long before
that guard fires, so runner speed no longer determines the assertion outcome.

This approach exercises the same real `runDuckDBServe` path as the current test,
avoids a production-only hook, and makes completion depend on the observable
behavior under test rather than runner speed.

The plain-serve and PostgreSQL helpers use the same timer pattern, but they have
not exhibited this failure and remain out of scope. The condition-based helper
can be generalized for them in a separate change if evidence warrants it.

## Testing

The existing visible-warning test will use the condition-based helper and must
continue to assert the warning captured specifically from stdout, preserving the
visible-stream contract instead of accepting the duplicate log line on stderr. A
helper-process regression will prove that the parent returns after observing the
warning even when the child would otherwise remain running. The focused test
will be repeated enough times to exercise process startup and cleanup, followed
by the package test suite and Go formatting/vetting checks.
