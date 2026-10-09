import { allowedPath } from "./paths.js";

// Covers message encoding and request shapes; bump both peers together.
const version = 1;

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

async function request({ id, path, version: peerVersion }, port) {
  let result;
  try {
    if (peerVersion !== version) throw new Error((peerVersion ?? 0) < version ? "Upgrade AgentsView, then Sync again" : "Run agentsview chrome setup, reload the extension at chrome://extensions, then Sync again");
    if (!await allowedPath(path)) throw new Error("Unsupported Claude fetch path");
    let [tab] = await chrome.tabs.query({ url: "https://claude.ai/*", discarded: false });
    if (!tab) tab = await chrome.tabs.create({ url: "https://claude.ai/new", active: false });
    await loadedTab(tab.id);
    const target = { tabId: tab.id };
    await chrome.scripting.executeScript({ target, files: ["claude_fetch.js"], world: "ISOLATED" });
    let reply;
    for (let attempt = 0; attempt < 2; attempt++) {
      [reply] = await chrome.scripting.executeScript({
        target,
        world: "ISOLATED",
        func: (url) => typeof claudeFetch === "function" ? claudeFetch(url) : undefined,
        args: [`https://claude.ai${path}`],
      });
      if (reply?.result !== undefined) break;
      if (attempt === 1) throw new Error("Claude.ai page changed during Sync; try Sync again");
      await chrome.scripting.executeScript({ target, files: ["claude_fetch.js"], world: "ISOLATED" });
    }
    result = { ...reply.result, id, version };
  } catch (error) {
    result = { id, version, status: 0, error: error.message };
  }
  if (new TextEncoder().encode(JSON.stringify(result)).byteLength > 64 * 1024 * 1024) result = { id, version, status: 413 };
  try { port.postMessage(result); } catch { /* Disconnect fails the server's pending request. */ }
}

let connection;
function connect() {
  if (connection) return;
  const port = chrome.runtime.connectNative("io.kenn.agentsview");
  connection = port;
  port.onMessage.addListener((message) => { void request(message, port); });
  port.onDisconnect.addListener(() => {
    void chrome.runtime.lastError;
    connection = undefined;
    setTimeout(connect, 5000);
  });
}
chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
connect();
