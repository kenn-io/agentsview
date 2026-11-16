# SQLite Usage page after a write

Each paired run copied the same SQLite archive of 30,946 sessions, applied the
same 24-session write cohort, and measured the full Usage page. The next visit
was a warm control. Each range ran three times in base/branch, branch/base,
base/branch order with a 300% CPU quota. Samples are in pair order.

| Range and visit | Base samples (s) | Branch samples (s) | Median change |
| --- | --- | --- | ---: |
| All-time after write | 8.632, 7.745, 7.958 | 6.884, 6.633, 6.620 | 7.958 → 6.633 s (-16.6%) |
| All-time warm | 2.800, 2.671, 2.576 | 2.705, 2.629, 2.680 | 2.671 → 2.680 s |
| 30 days after write | 5.440, 4.972, 5.029 | 4.186, 4.004, 4.129 | 5.029 → 4.129 s (-17.9%) |
| 30 days warm | 2.441, 2.380, 2.305 | 2.302, 2.570, 2.325 | 2.380 → 2.325 s |

Median request times for Summary and Pairwise were 5.434 / 5.843 s on base and
4.056 / 4.001 s on branch for all-time, and 2.512 / 3.006 s on base and
1.830 / 1.791 s on branch for 30 days.

All paired after-write visits returned identical Summary content (totals, daily
rows, project, model, and agent breakdowns, and session counts, excluding
pricing metadata) and identical full Pairwise responses. A later cancellation
retry change affects failed builds only.
