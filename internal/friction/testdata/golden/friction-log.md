---
date: 2026-09-16
signals_captured: 16
p0_count: 2
corrections: 2
errors: 6
workarounds: 1
deferrals: 1
patterns: 2
frustrations: 1
interruptions: 3
---

# Friction Log — 2026-09-16

## P0 Alerts

- **P0 ALERT**: `Bash` failed in 3 distinct sessions: s-1, s-2, s-3
- **P0 ALERT**: `Read` failed in 3 distinct sessions: s-1, s-2, s-3

## Corrections

- `s-1` — 'no, use the other branch'
- `seat:seat-02` `agent:claude` `machine:host-a.example` `s-2` — 'don\'t change the API'

## Errors

- `s-3` / `Read`: fatal: denied check file permissions
- `s-1` / `Bash`: command failed
- `s-2` / `Read`: file unavailable
- `s-3` / `Bash`: command failed
- `s-1` / `Read`: file unavailable
- `s-2` / `Bash`: command failed

## Workarounds

- `s-1` pattern=`for now`: 'use the fallback for now\nthen retry'

## Deferrals

- `s-2` pattern=`next session`

## Patterns

- `s-1` kind=`retry_loop`: `Bash` x3 identical arguments 09:00-09:02
- `s-3` kind=`runaway_loop`: 12 tool calls 09:00-09:11

## Frustration

- `s-1` — 'why won\'t it\nload???'

## Interruptions

- `agent:claude` `s-2` interruptions=2
- `s-1` interruptions=1

