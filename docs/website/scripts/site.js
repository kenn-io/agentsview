const repoApi = "https://api.github.com/repos/kenn-io/agentsview";
const cacheMaxAgeMs = 60 * 60 * 1000;

function installLightboxes() {
  const dialog = document.querySelector("[data-lightbox-dialog]");
  if (!(dialog instanceof HTMLDialogElement)) return;

  const image = dialog.querySelector("img");
  const title = dialog.querySelector("[data-lightbox-title]");
  const close = dialog.querySelector("[data-lightbox-close]");
  let trigger = null;

  for (const link of document.querySelectorAll("a[data-lightbox]")) {
    link.addEventListener("click", (event) => {
      if (!(image instanceof HTMLImageElement)) return;
      event.preventDefault();
      trigger = link;
      const source = link.getAttribute("href");
      const preview = link.querySelector("img");
      if (!source || !(preview instanceof HTMLImageElement)) return;
      image.src = source;
      image.alt = preview.alt;
      if (title) title.textContent = preview.alt;
      dialog.showModal();
      if (close instanceof HTMLElement) close.focus();
    });
  }

  close?.addEventListener("click", () => dialog.close());
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) dialog.close();
  });
  dialog.addEventListener("close", () => {
    if (image instanceof HTMLImageElement) image.removeAttribute("src");
    if (trigger instanceof HTMLElement) trigger.focus();
  });
}

function installCopyButton() {
  const root = document.querySelector("[data-install-command]");
  const status = document.querySelector("[data-install-status]");
  const button = root?.querySelector("[data-install-copy]");
  const command = root instanceof HTMLElement ? root.dataset.command : undefined;
  if (!(button instanceof HTMLButtonElement) || !(status instanceof HTMLElement) || !command) return;

  let resetTimer;
  button.addEventListener("click", async () => {
    clearTimeout(resetTimer);
    try {
      await navigator.clipboard.writeText(command);
      status.textContent = "Copied";
      resetTimer = setTimeout(() => {
        status.textContent = "";
      }, 2000);
    } catch {
      status.textContent = "Copy failed — select the command text instead";
    }
  });
}

function readCache(key, now) {
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return null;
    const entry = JSON.parse(raw);
    if (now - entry.at > cacheMaxAgeMs) return null;
    return entry.value;
  } catch {
    return null;
  }
}

function writeCache(key, value, now) {
  try {
    localStorage.setItem(key, JSON.stringify({ at: now, value }));
  } catch {
    // Storage can be unavailable (private browsing); facts refetch next visit.
  }
}

async function cachedJson(key, url) {
  const now = Date.now();
  const cached = readCache(key, now);
  if (cached !== null) return cached;
  const response = await fetch(url, { headers: { Accept: "application/vnd.github+json" } });
  if (!response.ok) return null;
  const value = await response.json();
  writeCache(key, value, now);
  return value;
}

function setFact(name, text) {
  const fact = document.querySelector(`[data-fact="${name}"]`);
  if (!(fact instanceof HTMLElement)) return;
  const label = fact.querySelector("[data-fact-text]");
  if (!label) return;
  label.textContent = text;
  fact.hidden = false;
  const row = document.querySelector("[data-facts]");
  if (row instanceof HTMLElement) row.hidden = false;
}

function formatCount(count) {
  if (count < 1000) return String(count);
  const thousands = count / 1000;
  const rounded = thousands >= 10 ? Math.round(thousands) : Math.round(thousands * 10) / 10;
  return `${rounded}k`;
}

async function installRepoFacts() {
  if (!document.querySelector("[data-facts]")) return;
  const [repo, release] = await Promise.all([
    cachedJson("agentsview:repo", repoApi),
    cachedJson("agentsview:release", `${repoApi}/releases/latest`),
  ]);
  if (repo) {
    setFact("stars", formatCount(repo.stargazers_count));
    setFact("forks", formatCount(repo.forks_count));
  }
  if (release) setFact("version", release.tag_name);
}

installLightboxes();
installCopyButton();
installRepoFacts().catch((error) => {
  console.warn("github api unavailable, keeping the static header", error);
});
