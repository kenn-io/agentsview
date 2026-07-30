# Worktree Tooltip Width Design

## Problem

The shared kit-ui tooltip limits all content to 280 pixels. That is useful for
short explanatory copy, but it forces ordinary filesystem paths to wrap inside
the narrow session-analysis sidebar. The tooltip uses fixed positioning and can
already move outside that sidebar, so the generic width limit wastes available
viewport space.

## Design

Keep the shared tooltip default unchanged. Pass a dedicated class through the
worktree tooltip's supported `class` prop and override only that tooltip's
maximum width:

```css
max-width: min(50vw, calc(100vw - 32px));
```

The tooltip retains kit-ui's existing `width: max-content`. A path that fits
within half of the viewport therefore stays on one line. A longer path wraps
naturally once it reaches the 50 percent cap. Kit-ui's fixed-position floating
logic continues to shift the tooltip left when the trigger is near the right
viewport edge, allowing the bubble to extend outside the sidebar without leaving
the viewport.

The displayed path and tooltip content remain LTR-isolated technical
identifiers. Hover, keyboard focus, copy behavior, and the compact truncated row
remain unchanged.

## Verification

Extend the session-vitals browser coverage to assert that the worktree tooltip:

- contains the complete path;
- never exceeds 50 percent of the viewport width;
- stays on one line when the fixture path fits under that limit;
- extends left of the sidebar when additional width is needed; and
- wraps when synthetic content exceeds the viewport-relative cap.

Keep the existing mixed-direction path geometry regression and component tooltip
interaction test.
