function markHost() {
  if (!document.documentElement) return false;
  document.documentElement.dataset.agentsviewClaudeHost = "chrome";
  return true;
}
if (!markHost()) {
  const observer = new MutationObserver(() => {
    if (markHost()) observer.disconnect();
  });
  observer.observe(document, { childList: true });
}

window.addEventListener("message", async (event) => {
  if (event.source !== window || event.origin !== location.origin || event.data?.type !== "agentsview-claude-request") return;
  const { id, method, path } = event.data;
  let reply;
  try {
    reply = await chrome.runtime.sendMessage({ id, method, path });
  } catch (error) {
    reply = { error: String(error) };
  }
  window.postMessage({ type: "agentsview-claude-reply", id, ...reply }, location.origin);
});
