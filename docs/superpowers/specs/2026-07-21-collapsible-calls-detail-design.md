# Collapsible Calls Detail Design

## Goal

Let users reclaim vertical space in the session analysis sidebar by collapsing
the detailed Calls visualization, while keeping the section summary visible and
remembering the preference across sessions and reloads.

## Interaction

The existing Calls section header becomes a full-width disclosure button. A
small right-pointing chevron appears before the Calls label and rotates downward
when expanded. The call-count summary remains aligned at the opposite edge in
both states.

Activating the header toggles the detail body, which contains the time axis and
the call rows. The section itself remains visible when collapsed, so users can
still see the total call count and restore the detail without searching for a
separate control.

The disclosure defaults to expanded when no preference has been stored. The
choice is global to the analysis sidebar rather than session-specific: moving
between sessions and reloading the app preserves the most recent state.

## Visual Treatment

The control reuses the sidebar's existing compact section typography, spacing,
colors, and Lucide chevron vocabulary. Hover and keyboard-focus states cover the
full header target. The implementation does not add a card, extra text action,
or decorative animation. The only motion is the short chevron rotation that
communicates disclosure state.

## Accessibility

The header is a semantic button with `aria-expanded` reflecting whether the
detail body is visible. Its accessible name comes from the localized Calls label
and existing localized summary. The chevron is decorative and hidden from
assistive technology. The control remains keyboard reachable and has a visible
focus indicator.

## Persistence

The global UI store owns a boolean Calls-expanded preference and persists it to
LocalStorage using an `agentsview-` namespaced key. Stored `"true"` and
`"false"` values are accepted; missing, invalid, or unavailable storage falls
back to expanded. Storage failures do not prevent the disclosure from working
for the current page lifetime.

## Testing

Component coverage will verify the user-visible contract: Calls details render
by default, activating the disclosure hides the axis and rows while retaining
the summary, `aria-expanded` changes with the state, and activating it again
restores the detail.

Store coverage will verify that a persisted collapsed value is restored and that
toggling the preference writes the corresponding boolean string. Tests will
assert rendered behavior and the owned persistence boundary, not Svelte or
LocalStorage implementation mechanics.

## Alternatives Considered

An icon-only button would be visually lighter but would provide a smaller and
less obvious target. A separate Show/Hide text action would be explicit but add
noise to an already dense header. Making the full header interactive provides
the clearest affordance and largest target without increasing visual density.
