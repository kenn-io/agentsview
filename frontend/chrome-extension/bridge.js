document.documentElement.dataset.agentsviewClaudeHost = "chrome";

window.addEventListener("message", async (event) => {
  if (event.source !== window || event.data?.type !== "agentsview-claude-request") return;
  const { id, method, path } = event.data;
  let reply;
  try {
    reply = await chrome.runtime.sendMessage({ id, method, path });
  } catch (error) {
    reply = { error: String(error) };
  }
  window.postMessage({ type: "agentsview-claude-reply", id, ...reply }, location.origin);
});
