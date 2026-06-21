var lightboxOpener = null;

function ensureLightbox() {
  var overlay = document.getElementById("lightbox-overlay");
  if (overlay) return overlay;

  overlay = document.createElement("div");
  overlay.id = "lightbox-overlay";
  overlay.setAttribute("role", "dialog");
  overlay.setAttribute("aria-modal", "true");
  overlay.setAttribute("aria-label", "Image preview");
  overlay.style.cssText =
    "position:fixed;inset:0;z-index:9999;background:rgba(0,0,0,.92);display:none;cursor:zoom-out;justify-content:center;align-items:center;";
  overlay.innerHTML =
    '<button type="button" aria-label="Close image preview" style="position:absolute;top:1rem;right:1rem;width:2.5rem;height:2.5rem;border:1px solid rgba(255,255,255,.45);border-radius:999px;background:rgba(0,0,0,.55);color:#fff;font-size:1.75rem;line-height:1;cursor:pointer;">&times;</button>' +
    '<img style="max-width:95vw;max-height:95vh;object-fit:contain;" alt="">';
  overlay.addEventListener("click", function (event) {
    if (event.target === overlay) closeLightbox();
  });
  overlay.querySelector("button").addEventListener("click", function () {
    closeLightbox();
  });
  document.body.appendChild(overlay);
  document.addEventListener("keydown", function (event) {
    if (overlay.style.display === "none") return;
    if (event.key === "Escape") closeLightbox();
    if (event.key === "Tab") trapLightboxFocus(event, overlay);
  });
  return overlay;
}

function getLightboxFocusableElements(overlay) {
  return Array.prototype.slice
    .call(
      overlay.querySelectorAll(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
      )
    )
    .filter(function (element) {
      return element.offsetParent !== null;
    });
}

function trapLightboxFocus(event, overlay) {
  var focusable = getLightboxFocusableElements(overlay);
  if (!focusable.length) {
    event.preventDefault();
    overlay.focus();
    return;
  }

  var first = focusable[0];
  var last = focusable[focusable.length - 1];
  var active = document.activeElement;

  if (!overlay.contains(active)) {
    event.preventDefault();
    first.focus();
  } else if (event.shiftKey && active === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && active === last) {
    event.preventDefault();
    first.focus();
  }
}

function openLightbox(img) {
  var overlay = ensureLightbox();
  var overlayImg = overlay.querySelector("img");
  lightboxOpener = img;
  overlayImg.src = img.src;
  overlayImg.alt = img.alt || "";
  overlay.style.display = "flex";
  overlay.querySelector("button").focus();
}

function closeLightbox() {
  var overlay = document.getElementById("lightbox-overlay");
  if (!overlay) return;

  overlay.style.display = "none";
  if (lightboxOpener && typeof lightboxOpener.focus === "function") {
    lightboxOpener.focus();
  }
  lightboxOpener = null;
}

function initLightbox() {
  var selector = [
    "[data-lightbox] img",
    '.md-content img[src^="/assets/generated/screenshots/"]',
  ].join(", ");
  var images = document.querySelectorAll(selector);
  if (!images.length) return;

  images.forEach(function (img) {
    if (img.dataset.lightboxBound) return;
    img.dataset.lightboxBound = "1";
    img.style.cursor = "zoom-in";
    if (!img.hasAttribute("tabindex")) img.setAttribute("tabindex", "0");
    img.setAttribute("role", "button");
    img.setAttribute("aria-label", "Open image preview" + (img.alt ? ": " + img.alt : ""));
    img.addEventListener("click", function (event) {
      event.stopPropagation();
      openLightbox(img);
    });
    img.addEventListener("keydown", function (event) {
      if (event.key !== "Enter" && event.key !== " ") return;
      event.preventDefault();
      openLightbox(img);
    });
  });
}

if (typeof document$ !== "undefined") {
  document$.subscribe(initLightbox);
} else {
  document.addEventListener("DOMContentLoaded", initLightbox);
}
