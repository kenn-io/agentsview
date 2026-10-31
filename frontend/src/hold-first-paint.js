// Copied from kenn-io/actions first-paint/hold-first-paint.js. The server CSP
// allows it by hash; update holdFirstPaintScriptHash when this file changes.
// Keeps the page hidden until its stylesheets and fonts are ready, so text
// never paints in a fallback face. Chromium decodes even a font embedded in
// the stylesheet only after its first paint, so embedding alone is not enough.
// Inline this script in <head> before any stylesheet.
(() => {
  const root = document.documentElement;
  // Opacity hides every descendant. A descendant can override visibility.
  // The important priority keeps a stylesheet's opacity rule from showing the
  // page early.
  const opacity = root.style.getPropertyValue("opacity");
  const priority = root.style.getPropertyPriority("opacity");
  root.style.setProperty("opacity", "0", "important");
  let shown = false;
  const show = () => {
    if (shown) return;
    shown = true;
    root.style.setProperty("opacity", opacity, priority);
  };
  // Never hide the page longer than a browser blocks text on a slow font.
  setTimeout(show, 3000);
  const ready = async () => {
    const pending = [...document.querySelectorAll("link")]
      .filter((link) => {
        const rel = link.rel.toLowerCase().split(/\s+/);
        return rel.includes("stylesheet") && !rel.includes("alternate");
      })
      // A disabled or print stylesheet does not style the screen.
      .filter((link) => !link.disabled && matchMedia(link.media || "all").matches && !link.sheet)
      .map((link) => new Promise((done) => {
        link.addEventListener("load", done);
        link.addEventListener("error", done);
      }));
    await Promise.all(pending);
    // Load every declared face, including ones only text rendered later by
    // JavaScript uses. A failed face falls back the way it would anyway.
    await Promise.all([...document.fonts].map((face) => face.load().catch(() => {})));
    show();
  };
  // The head is parsed, and every stylesheet link is in the document, once
  // the document leaves "loading". Deferred and module scripts run later.
  if (document.readyState === "loading") {
    document.addEventListener("readystatechange", ready, { once: true });
  } else {
    ready();
  }
})();
