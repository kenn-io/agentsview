// Temporary workaround until agentsview adopts the kit-ui TopBar fix on
// the kit-ui branch `topbar-no-current-tab`, where an `active` that names
// no tab means no tab is current. The pinned TopBar instead falls back to
// the first tab, so on Settings (and on Friction while its tab is hidden)
// Sessions gets aria-current="page" and the collapsed dropdown shows
// "Sessions". Delete this action, its wrapper in AppHeader.svelte, and the
// `no-current-tab` styles when the pin moves past that fix.
//
// Svelte skips DOM writes whose value matches the one it last wrote, so
// after this action edits a node Svelte will not restore it when the same
// fallback tab becomes current for real. The action therefore owns the
// result in both directions: it keeps aria-current on the routed tab only,
// and it puts back the trigger text and label it replaced.

export interface TopBarNoCurrentTabParams {
  /** Routed tab id, or "" when the route has no tab. */
  active: string;
  /** Tab ids in the order TopBar renders them. */
  tabIds: string[];
  /** Collapsed dropdown text while no tab is current. */
  placeholder: string;
  /** Accessible name of the collapsed dropdown while no tab is current. */
  navLabel: string;
}

const STASH_TEXT = "agentsviewTabText";
const STASH_LABEL = "agentsviewTabLabel";

export function topBarNoCurrentTab(node: HTMLElement, initial: TopBarNoCurrentTabParams) {
  let params = initial;
  const observer = new MutationObserver(sync);

  function observe(): void {
    observer.observe(node, {
      subtree: true,
      childList: true,
      characterData: true,
      attributes: true,
      attributeFilter: ["aria-current", "aria-label"],
    });
  }

  function syncTabs(): void {
    const current = params.tabIds.indexOf(params.active);
    const buttons = node.querySelectorAll<HTMLElement>(".kit-top-bar__tabs > .kit-top-bar__tab");
    buttons.forEach((button, index) => {
      if (index === current) {
        if (button.getAttribute("aria-current") !== "page") {
          button.setAttribute("aria-current", "page");
        }
      } else if (button.hasAttribute("aria-current")) {
        button.removeAttribute("aria-current");
      }
    });
  }

  function syncTrigger(): void {
    const trigger = node.querySelector<HTMLElement>(
      ".kit-top-bar__nav-select .kit-select-dropdown__trigger",
    );
    const value = trigger?.querySelector<HTMLElement>(".kit-select-dropdown__value");
    const text = value?.firstChild;
    if (!trigger || !value || !(text instanceof Text)) return;

    if (params.active === "") {
      // Edit the existing text node so Svelte keeps writing to it.
      if (text.data !== params.placeholder) {
        value.dataset[STASH_TEXT] = text.data;
        text.data = params.placeholder;
      }
      if (trigger.getAttribute("aria-label") !== params.navLabel) {
        trigger.dataset[STASH_LABEL] = trigger.getAttribute("aria-label") ?? "";
        trigger.setAttribute("aria-label", params.navLabel);
      }
      return;
    }

    const stashedText = value.dataset[STASH_TEXT];
    if (stashedText !== undefined) {
      if (text.data === params.placeholder) text.data = stashedText;
      delete value.dataset[STASH_TEXT];
    }
    const stashedLabel = trigger.dataset[STASH_LABEL];
    if (stashedLabel !== undefined) {
      if (trigger.getAttribute("aria-label") === params.navLabel) {
        trigger.setAttribute("aria-label", stashedLabel);
      }
      delete trigger.dataset[STASH_LABEL];
    }
  }

  function sync(): void {
    observer.disconnect();
    syncTabs();
    syncTrigger();
    observer.takeRecords();
    observe();
  }

  sync();
  return {
    update(next: TopBarNoCurrentTabParams) {
      params = next;
      sync();
    },
    destroy() {
      observer.disconnect();
    },
  };
}
