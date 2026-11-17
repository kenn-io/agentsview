// Each key is a platform's agent type, the name the server sends. requests/
// holds one allowlist per key, copied from internal/importer/browser_requests.
const uuid = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";

export const platforms = {
  "claude-ai": {
    origin: "https://claude.ai",
    newTabURL: "https://claude.ai/new",
    scripts: ["page_fetch.js"],
    fetchFunction: "pageFetch",
    // Patterns for each {placeholder} in the allowlist.
    params: { organization: uuid, conversation: uuid, offset: "[0-9]+" },
  },
};
