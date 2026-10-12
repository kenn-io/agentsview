import { allowedPath } from "./paths.js";

// Covers message encoding and request shapes; bump both peers together.
const version = 2;

async function loadedTab(id) {
  await new Promise((resolve, reject) => {
    const updated = (tabId, change) => {
      if (tabId === id && change.status === "complete") finish();
    };
    const removed = (tabId) => {
      if (tabId === id) finish(new Error("Browser tab closed"));
    };
    const timer = setTimeout(() => finish(new Error("Browser tab did not load")), 45000);
    function finish(error) {
      clearTimeout(timer);
      chrome.tabs.onUpdated.removeListener(updated);
      chrome.tabs.onRemoved.removeListener(removed);
      if (error) reject(error);
      else resolve();
    }
    chrome.tabs.onUpdated.addListener(updated);
    chrome.tabs.onRemoved.addListener(removed);
    chrome.tabs.get(id).then((tab) => {
      if (tab.status === "complete") finish();
    }, finish);
  });
}

const platforms = {
  "claude-ai": {
    origin: "https://claude.ai",
    tabURL: "https://claude.ai/*",
    newTabURL: "https://claude.ai/new",
    script: "claude_fetch.js",
    fetchFunction: "claudeFetch",
  },
  chatgpt: {
    origin: "https://chatgpt.com",
    tabURL: "https://chatgpt.com/*",
    newTabURL: "https://chatgpt.com/",
    script: "chatgpt_fetch.js",
    fetchFunction: "chatgptFetch",
  },
};

async function request({ id, path, platform }, port) {
  let result;
  try {
  if (new TextEncoder().encode(JSON.stringify(result)).byteLength > 64 * 1024 * 1024) result = { id, platform, version, status: 413 };
  try { port.postMessage(result); } catch { /* Disconnect fails the server's pending request. */ }
