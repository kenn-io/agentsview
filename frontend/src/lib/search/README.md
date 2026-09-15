# In-session find

Find searches the selected session's message text, thinking, skills, code, tool
inputs, outputs, and result history. Inline subagent transcripts remain scoped
to their own sessions. Counts come from the filtered message data, independently
of virtualized rows and disclosure state. Thinking within grouped tool messages
uses the same indexed text and stable keys as standalone thinking blocks.

The underlined `ab` control enables **Match whole word**. Substring matching is
the default. Whole words are delimited by characters other than Unicode letters,
combining marks, numbers, and connector punctuation (including underscores). For
example, `cat` matches `(cat)` and `cat-dog`, but not `scatter`, `cat2`, or
`cat_name`. Both modes are case-insensitive and use the same matching rules for
counts, result snippets, and transcript highlights. The selected mode is
retained when the find bar is closed and reopened.

## Navigation

The index stays chronological. Newest-first changes message order only; each
message's blocks and occurrences retain their top-to-bottom reading order. The
cursor, result list, and overview rail use that same ordering. An initial match
is pinned before older history arrives, so loading earlier pages does not change
the selected occurrence. Explicit next/previous commands wrap at the ends of the
results.

Enter, Shift+Enter, F3, Shift+F3, and the platform find-next shortcuts operate
on the current input. A navigation request commits pending debounce text before
choosing its target. Result rows from the previous query cannot navigate while a
replacement query is pending. IME composition is allowed to confirm or cancel
candidates without invoking navigation or closing find.

## Presentation and recovery

Search respects block-type filters and Focused mode. The transcript and index
share `projectSessionScope`, so hidden categories do not contribute matches.
Changing a filter updates counts, results, and navigation without retyping.
Collapsed content within that scope remains searchable; only the current
matching disclosure is opened automatically. Skim layout temporarily lifts while
searching without changing the saved layout preference. Native CSS highlights do
not rewrite transcript text. Precise reveal handles nested scrolling and the
application's text-size/zoom settings.

Native Markdown details elements also open for the current match. Search
restores only its own temporary changes on navigation or close, preserving
previously open elements and explicit user choices. Matching a visible summary
does not expand its body. Reselecting the only match starts a new reveal after a
manual close. Reveal success checks reject closed disclosures and CSS-hidden
boundaries.

Historical messages are loaded when find opens. Completeness is independent of
pagination direction: a failed forward page is partial even when there are no
older messages. Recovery appends the missing tail without replacing existing
rows, deduplicates concurrent updates, and coalesces repeated requests. The bar
reports partial results and offers explicit retry after a failed attempt. Failed
loads do not cause an automatic request loop. Session and request identity
checks prevent old responses from changing a new session.

Text extraction also follows the XML rendering preference and the tool-result
display conversion. Image placeholders are indexed as displayed text, while
copying a tool result still preserves its original stored representation.

## Matching cost

Each stable content block weakly owns prepared lowercase text. Queries compile
once per scan, so subsequent queries do not refold every character. ASCII runs
use native string conversion; non-ASCII code points retain independent casing
semantics. Only length-changing folds need packed expansion boundaries, rather
than dense offset arrays for the whole block. Offsets remain original UTF-16
positions and matches stay non-overlapping. Replacement blocks invalidate the
cache, and an in-place text change cannot reuse a stale prepared value.

Indexing remains synchronous. Initial content parsing and rendering still have a
cost; cached matching does not replace pagination, virtual rendering, or the
existing query debounce. No results are omitted or capped for performance.

## Regression coverage

`navigation.test.ts` covers ordering, stable tuples, opening anchors, and wrap
behavior. `find-input.test.ts` covers composition and event ownership.
`details-reveal.test.ts` covers native disclosures, manual overrides, cleanup,
and named groups. `matcher-equivalence.test.ts` and
`session-index-cache.test.ts` cover Unicode offsets and prepared-text
invalidation. Store recovery coverage is in `messages-history.test.ts` and
`inSessionSearch-history.test.ts`. `ToolCallGroup-thinking.test.ts` covers
grouped thinking and display ordering. Application browser cases are in
`frontend/e2e/session-find.spec.ts` and
`frontend/e2e/session-find-regressions.spec.ts`.
