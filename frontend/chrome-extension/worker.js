import { allowedPath } from "./paths.js";
import { platforms } from "./platforms.js";

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

async function request({ id, platform, path }, port) {
  let result;
  try {
    if (!Object.hasOwn(platforms, platform)) {
      // An older extension than the server: the server reports that the
      // extension needs an update without dropping the connection.
      result = { id, version, status: 0, unsupported: true };
    } else {
      const config = platforms[platform];
      if (!(await allowedPath(platform, path))) throw new Error("Unsupported browser fetch path");
      let [tab] = await chrome.tabs.query({ url: `${config.origin}/*`, discarded: false });
      if (!tab) tab = await chrome.tabs.create({ url: config.newTabURL, active: false });
      await loadedTab(tab.id);
      const target = { tabId: tab.id };
      await chrome.scripting.executeScript({ target, files: config.scripts, world: "ISOLATED" });
      const [reply] = await chrome.scripting.executeScript({
        target,
        world: "ISOLATED",
        func: (name, url) =>
          typeof globalThis[name] === "function" ? globalThis[name](url) : undefined,
        args: [config.fetchFunction, `${config.origin}${path}`],
      });
      if (typeof reply?.result?.status !== "number")
        throw new Error("The page changed during Sync; try Sync again");
      result = { ...reply.result, id, version };
    }
  } catch (error) {
    result = { id, version, status: 0, error: error.message };
  }
  if (new TextEncoder().encode(JSON.stringify(result)).byteLength > 64 * 1024 * 1024)
    result = { id, version, status: 413 };
  try {
    port.postMessage(result);
  } catch {
    /* Disconnect fails the server's pending request. */
  }
}

let connection;
function connect() {
  if (connection) return;
  const port = chrome.runtime.connectNative("io.kenn.agentsview");
  connection = port;
  port.onMessage.addListener((message) => {
    void request(message, port);
  });
  port.onDisconnect.addListener(() => {
    void chrome.runtime.lastError;
    connection = undefined;
    setTimeout(connect, 5000);
  });
}
chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
connect();
