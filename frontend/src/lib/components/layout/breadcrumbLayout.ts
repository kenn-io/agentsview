// Layout helpers for the session header. Both attach to elements as Svelte
// attachments.
const PANE_GAP = 8;

// Fired on the header when a metadata group starts or stops wrapping.
const META_WRAP_EVENT = "metawrap";

// The element's effective CSS zoom. WebKit has no currentCSSZoom, so
// multiply the computed zoom of the element and its ancestors instead.
function cssZoom(element: HTMLElement): number {
  if (typeof element.currentCSSZoom === "number") return element.currentCSSZoom;
  let zoom = 1;
  for (let node: Element | null = element; node; node = node.parentElement) {
    const value = Number.parseFloat(getComputedStyle(node).zoom);
    if (Number.isFinite(value) && value > 0) zoom *= value;
  }
  return zoom;
}

// The transcript pane clips overflow, and side panels can make it
// narrower than a usage menu. Keep each menu inside the pane: as wide as
// it fits, right-aligned to its trigger when there is room, otherwise
// shifted left until it fits.
export function keepInsidePane(menu: HTMLElement) {
  const anchor = menu.parentElement;
  const pane = menu.closest(".session-breadcrumb");
  if (!anchor || !pane) return;
  const place = () => {
    menu.style.removeProperty("left");
    menu.style.removeProperty("right");
    menu.style.removeProperty("max-width");
    // Phones pin the menu to the viewport instead.
    if (getComputedStyle(menu).position !== "absolute") return;
    // Rects are in viewport pixels; interface zoom scales the menu's own
    // CSS lengths, so measure in viewport pixels and convert back.
    const zoom = cssZoom(menu);
    const gap = PANE_GAP * zoom;
    const bounds = pane.getBoundingClientRect();
    const trigger = anchor.getBoundingClientRect();
    menu.style.maxWidth = `${Math.max(0, bounds.width - gap * 2) / zoom}px`;
    const width = menu.getBoundingClientRect().width;
    const left = Math.max(
      bounds.left + gap,
      Math.min(trigger.right - width, bounds.right - gap - width),
    );
    menu.style.left = `${(left - trigger.left) / zoom}px`;
    menu.style.right = "auto";
  };
  place();
  // Side panels resize the pane without resizing the window. Watch the
  // pane's border box: once the reading column caps its content, a
  // resize only changes the pane's centring padding.
  const observer = new ResizeObserver(place);
  observer.observe(pane, { box: "border-box" });
  observer.observe(anchor);
  window.addEventListener("resize", place);
  // A wrapped metadata group shifts its trigger without resizing it.
  pane.addEventListener(META_WRAP_EVENT, place);
  return () => {
    observer.disconnect();
    window.removeEventListener("resize", place);
    pane.removeEventListener(META_WRAP_EVENT, place);
  };
}

// A metadata group that wraps to start a new line would lead with its
// separator. CSS cannot tell where a line starts, so mark those groups
// after layout; the style block hides their separator without changing
// their size, so marking cannot reflow the row.
export function markWrappedGroups(header: HTMLElement) {
  if (typeof ResizeObserver === "undefined") return;
  let frame = 0;
  const mark = () => {
    frame = 0;
    let previous: DOMRect | null = null;
    let changed = false;
    for (const group of header.querySelectorAll<HTMLElement>(".meta-group")) {
      const rect = group.getBoundingClientRect();
      if (rect.width === 0 && rect.height === 0) continue;
      const wraps = previous !== null && rect.top >= previous.bottom - 1;
      if (group.hasAttribute("data-wraps") !== wraps) {
        group.toggleAttribute("data-wraps", wraps);
        changed = true;
      }
      previous = rect;
    }
    // Open usage menus re-place themselves against the moved trigger.
    if (changed) header.dispatchEvent(new Event(META_WRAP_EVENT));
  };
  const schedule = () => {
    if (!frame) frame = requestAnimationFrame(mark);
  };
  const resize = new ResizeObserver(schedule);
  resize.observe(header);
  const mutations = new MutationObserver(schedule);
  mutations.observe(header, { childList: true, subtree: true, characterData: true });
  schedule();
  return () => {
    cancelAnimationFrame(frame);
    resize.disconnect();
    mutations.disconnect();
  };
}
