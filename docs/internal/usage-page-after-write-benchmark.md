# SQLite Usage page after a write

Each paired run copied the same SQLite archive of 30,946 sessions, applied the
same 24-session write cohort, and measured the full Usage page. The next visit
was a warm control. Each range ran three times in base/branch, branch/base,
base/branch order with a 300% CPU quota.

## Page wait

| Range | Base median | Branch median | Change | Warm median, base / branch |
| --- | ---: | ---: | ---: | ---: |
| All-time | 7.958 s | 6.633 s | -16.6% | 2.671 s / 2.680 s |
| 30 days | 5.029 s | 4.129 s | -17.9% | 2.380 s / 2.325 s |

Samples appear in pair order.

| Range | Base after write | Branch after write | Base warm | Branch warm |
| --- | --- | --- | --- | --- |
| All-time | 8.632 s, 7.745 s, 7.958 s | 6.884 s, 6.633 s, 6.620 s | 2.800 s, 2.671 s, 2.576 s | 2.705 s, 2.629 s, 2.680 s |
| 30 days | 5.440 s, 4.972 s, 5.029 s | 4.186 s, 4.004 s, 4.129 s | 2.441 s, 2.380 s, 2.305 s | 2.302 s, 2.570 s, 2.325 s |

## Request medians

| Range | Request | Base | Branch |
| --- | --- | ---: | ---: |
| All-time | Summary | 5.434 s | 4.056 s |
| All-time | Pairwise | 5.843 s | 4.001 s |
| 30 days | Summary | 2.512 s | 1.830 s |
| 30 days | Pairwise | 3.006 s | 1.791 s |

All three paired after-write visits returned identical Summary content, excluding
pricing metadata, and identical full Pairwise responses. The Summary comparison
included totals, daily rows, project, model, and agent breakdowns, and session
counts. A later cancellation retry change affects failed builds only.
