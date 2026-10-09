import { allowedPath } from "./paths.js";

chrome.action.onClicked.addListener(async (tab) => {
  const url = new URL(tab.url);
  if (url.protocol !== "http:" || !["localhost", "127.0.0.1"].includes(url.hostname)) return;
  const [document] = await chrome.scripting.executeScript({ target: { tabId: tab.id }, func: () => {} });
  if (document.documentId) await chrome.storage.session.set({ [String(tab.id)]: document.documentId });
});

chrome.tabs.onRemoved.addListener((id) => chrome.storage.session.remove(String(id)));

async function loadedTab(id) {
  await new Promise((resolve, reject) => {
    const updated = (tabId, change) => {
      if (tabId === id && change.status === "complete") finish();
    };
    const removed = (tabId) => {
      if (tabId === id) finish(new Error("Claude.ai tab closed"));
    };
    const timer = setTimeout(() => finish(new Error("Claude.ai tab did not load")), 45000);
    function finish(error) {
      clearTimeout(timer);
      chrome.tabs.onUpdated.removeListener(updated);
      chrome.tabs.onRemoved.removeListener(removed);
      if (error) reject(error); else resolve();
    }
    chrome.tabs.onUpdated.addListener(updated);
    chrome.tabs.onRemoved.addListener(removed);
    chrome.tabs.get(id).then((tab) => {
      if (tab.status === "complete") finish();
    }, finish);
  });
}

async function request({ method, path }, sender) {
  const key = String(sender.tab?.id);
  const consent = await chrome.storage.session.get(key);
  if (method === "revoke") {
    if (sender.documentId && consent[key] === sender.documentId) await chrome.storage.session.remove(key);
    return null;
  }
  if (!sender.documentId || consent[key] !== sender.documentId) {
    throw new Error("Click the AgentsView toolbar button on this tab to allow Claude.ai Sync.");
  }
  if (method === "connect") {
    await chrome.tabs.create({ url: "https://claude.ai/login?return_url=%2Fnew" });
  } else if (method === "fetch") {
    if (!allowedPath(path)) throw new Error("Unsupported Claude fetch path");
    let [tab] = await chrome.tabs.query({ url: "https://claude.ai/*", discarded: false });
    if (!tab) {
      tab = await chrome.tabs.create({ url: "https://claude.ai/new", active: false });
    }
    await loadedTab(tab.id);
    const target = { tabId: tab.id };
    await chrome.scripting.executeScript({ target, files: ["claude_fetch.js"], world: "ISOLATED" });
    const [reply] = await chrome.scripting.executeScript({
      target,
      world: "ISOLATED",
      func: (url) => claudeFetch(url),
      args: [`https://claude.ai${path}`],
    });
    return reply.result;
  } else {
    throw new Error("Unsupported Claude host method");
  }
  return null;
}

chrome.runtime.onMessage.addListener((message, sender, respond) => {
  request(message, sender).then((result) => respond({ result }), (error) => respond({ error: error.message }));
  return true;
});
