# CI Docker Build Retry Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep transient Docker Hub failures from discarding an otherwise
successful integration run while preserving prompt failure for persistent
errors.

**Architecture:** Add one generic, bounded shell retry helper and exercise it
through real subprocesses in a behavioral shell test. Route only the SSH test
image build through the helper and add the test to the existing scripts CI job.

**Tech Stack:** Bash, GitHub Actions, Docker CLI

______________________________________________________________________

### Task 1: Specify retry behavior with an executable shell test

**Files:**

- Create: `scripts/retry_test.sh`

- [ ] **Step 1: Write the failing behavioral test**

    Create a temporary fake command that records its arguments and attempt count,
    fails twice, and succeeds on its third invocation. Assert that
    `scripts/retry.sh 3 0 <fake-command> "argument with spaces"` succeeds after
    exactly three attempts and forwards the argument unchanged.

    Put a fake `sleep` on `PATH`, invoke the helper with a base delay of 10
    seconds, and assert that the two waits receive literal durations `10` and
    `20` without actually pausing the test.

    Add a second fake command that always exits 17. Assert that the helper invokes
    it exactly three times and returns exit status 17.

- [ ] **Step 2: Run the test to verify it fails for the missing helper**

    Run: `bash scripts/retry_test.sh`

    Expected: FAIL because `scripts/retry.sh` does not exist.

### Task 2: Implement the bounded retry helper

**Files:**

- Create: `scripts/retry.sh`

- Test: `scripts/retry_test.sh`

- [ ] **Step 1: Add the minimal retry loop**

    Accept a maximum-attempt count and delay in seconds followed by the command
    and its arguments. Run the command until it succeeds or reaches the limit,
    sleep for `base delay * failed-attempt number` between failures, emit a
    concise retry message to standard error, and return the final command's exit
    status when exhausted.

- [ ] **Step 2: Run the behavioral test to verify it passes**

    Run: `bash scripts/retry_test.sh`

    Expected: PASS with the success message from the test script.

### Task 3: Adopt the helper in CI

**Files:**

- Modify: `.github/workflows/ci.yml:117-123`

- Modify: `.github/workflows/ci.yml:282-283`

- [ ] **Step 1: Add the retry test to the scripts job**

    Run `bash scripts/retry_test.sh` alongside the existing shell-script tests.

- [ ] **Step 2: Wrap the SSH image build**

    Replace the direct build invocation with
    `bash scripts/retry.sh 3 10 docker build -t agentsview-sshd -f testdata/ssh/Dockerfile .`.

- [ ] **Step 3: Run focused validation**

    Run: `bash scripts/retry_test.sh`

    Run: `actionlint .github/workflows/ci.yml` when `actionlint` is available.

    Run: `git diff --check`

    Expected: all commands exit successfully.

### Task 4: Publish the change

**Files:**

- Commit the design, plan, helper, test, and workflow changes.

- [ ] **Step 1: Review and scrub the outgoing diff and messages**

    Verify that no private paths, identities, hostnames, or unrelated changes are
    present.

- [ ] **Step 2: Commit the implementation**

    Use a focused conventional commit explaining why transient registry
    availability should not invalidate successful integration work.

- [ ] **Step 3: Push and open a pull request**

    As explicitly requested by the user, push the current feature branch and open
    a rationale-first PR whose description is a summary only, without a
    test-plan section or checklist.
