import { allowedPath } from "./paths.js";

const ownedTabs = new Map();

chrome.action.onClicked.addListener(async (tab) => {
  const url = new URL(tab.url);
  if (url.protocol !== "http:" || !["localhost", "127.0.0.1"].includes(url.hostname)) return;
  const { allowedOrigins = [] } = await chrome.storage.local.get("allowedOrigins");
  await chrome.storage.local.set({ allowedOrigins: [...new Set([...allowedOrigins, url.origin])] });
});

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
  const { allowedOrigins = [] } = await chrome.storage.local.get("allowedOrigins");
  if (sender.frameId !== 0 || !allowedOrigins.includes(sender.origin)) {
    throw new Error("Click the AgentsView toolbar button on this tab to allow Claude.ai Sync.");
  }
  if (method === "connect") {
    await chrome.tabs.create({ url: "https://claude.ai/login?return_url=%2Fnew" });
  } else if (method === "fetch") {
    if (!allowedPath(path)) throw new Error("Unsupported Claude fetch path");
    let [tab] = await chrome.tabs.query({ url: "https://claude.ai/*" });
    if (!tab) {
      tab = await chrome.tabs.create({ url: "https://claude.ai/new", active: false });
      ownedTabs.set(sender.origin, tab.id);
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
  } else if (method === "close") {
    const id = ownedTabs.get(sender.origin);
    if (id !== undefined) {
      ownedTabs.delete(sender.origin);
      await chrome.tabs.remove(id);
    }
  } else {
    throw new Error("Unsupported Claude host method");
  }
}

chrome.runtime.onMessage.addListener((message, sender, respond) => {
  request(message, sender).then((result) => respond({ result }), (error) => respond({ error: String(error) }));
  return true;
});
