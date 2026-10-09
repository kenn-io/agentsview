document.documentElement.dataset.agentsviewClaudeHost = "chrome";
let revocation;

window.addEventListener("pageshow", (event) => {
  if (event.persisted) revocation = chrome.runtime.sendMessage({ method: "revoke" });
});

window.addEventListener("message", async (event) => {
  if (event.source !== window || event.data?.type !== "agentsview-claude-request") return;
  const { id, method, path } = event.data;
  let reply;
  try {
    await revocation;
    reply = await chrome.runtime.sendMessage({ id, method, path });
  } catch (error) {
    reply = { error: String(error) };
  }
  window.postMessage({ type: "agentsview-claude-reply", id, ...reply }, location.origin);
});
